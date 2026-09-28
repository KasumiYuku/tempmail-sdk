/*!
 * Tmpkit 渠道实现（tmpkit.com，Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）
 *
 * 调研实证结论（与 Go 端 tmpkit.go 一致）：
 *   - POST /api/rpc/tempmail/initSession，body 为 tRPC 包裹 {"json":{}}，
 *     免凭据。响应 {"json":{"session":{"sessionId","email","createdAt",
 *     "expiresAt","extendedCount"},"availableDomains":["tmpkit.com"]}}，
 *     同时 Set-Cookie: tempmail_session=<sessionId>; Max-Age=3600（与
 *     sessionId 同值）。
 *   - POST /api/rpc/tempmail/getEmails，body {"json":{"offset":0,
 *     "limit":20}}，需带 tempmail_session Cookie。响应
 *     {"json":{"emails":[...],"total":N,"session":{"email","expiresIn"}}}；
 *     不带 Cookie 时同样 200 但 emails 为空、session 为 null。列表元素
 *     键为 mailId/from/subject/excerpt/date/timestamp/hasAttach/isRead
 *     （无 id 键）。
 *   - POST /api/rpc/tempmail/getEmailDetail，body
 *     {"json":{"mailId":<数字>}}（mailId 为数字，字符串会 zod 400）。
 *     响应为单封详情对象：mailId/from/to/subject/body/date/timestamp/
 *     contentType/sourceEmail；body 为含 <br> 的纯文本正文。
 *
 * 会话粘性：token 保存 sessionId（tempMailSession=<sid>），每次读信以
 *   显式 Cookie 请求头携带，防全局会话被并行会话覆盖后串箱。
 */

use crate::config::{block_on, get_current_ua, http_client_no_cookie_jar};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};

const SITE: &str = "https://tmpkit.com";
const RPC_PREFIX: &str = "https://tmpkit.com/api/rpc/tempmail";

/// 对 tmpkit 发起 rpc 调用（tRPC 包裹 {"json":<reqBody>}）
/// cookie 非空时以显式 Cookie 请求头携带 tempmail_session（sessionId）。
async fn rpc(procedure: &str, req_body: Value, cookie: &str) -> Result<Value, String> {
    let mut req = http_client_no_cookie_jar()
        .post(format!("{RPC_PREFIX}/{procedure}"))
        .header("User-Agent", get_current_ua())
        .header("Accept", "*/*")
        .header("Content-Type", "application/json")
        .header("Origin", SITE)
        .header("Referer", format!("{SITE}/en"))
        .body(json!({"json": req_body}).to_string());
    if !cookie.is_empty() {
        req = req.header("Cookie", format!("tempmail_session={cookie}"));
    }

    let resp = req
        .send()
        .await
        .map_err(|e| format!("tmpkit: {procedure} 请求失败: {e}"))?;
    let status = resp.status();
    let body = resp
        .text()
        .await
        .map_err(|e| format!("tmpkit: {procedure} 读取响应失败: {e}"))?;
    if !status.is_success() {
        return Err(format!("tmpkit: {procedure} 失败 http {status}: {body}"));
    }

    let outer: Value = serde_json::from_str(&body)
        .map_err(|e| format!("tmpkit: 解析 {procedure} 响应失败: {e}"))?;
    let payload = outer.get("json").cloned().unwrap_or(Value::Null);
    if !payload.is_object() {
        return Err(format!("tmpkit: {procedure} 响应缺 json 载荷: {body}"));
    }
    Ok(payload)
}

/// 从 JSON 值容错取字符串（与 Go 端 tmpkitMapGet 语义一致）
fn map_get(v: Option<&Value>) -> String {
    match v {
        Some(Value::String(s)) => s.clone(),
        Some(Value::Number(n)) => n.to_string(),
        Some(Value::Bool(b)) => b.to_string(),
        _ => String::new(),
    }
}

/// 创建 tmpkit.com 临时邮箱
/// 调 initSession（tRPC 包裹 {"json":{}}），sessionId 与邮箱地址同返。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let data = rpc("initSession", json!({}), "").await?;
        let sess = data.get("session").filter(|v| v.is_object());
        if sess.is_none() {
            return Err("tmpkit: 创建会话响应缺 session 字段".into());
        }
        let email = map_get(sess.and_then(|s| s.get("email"))).trim().to_string();
        let session_id = map_get(sess.and_then(|s| s.get("sessionId")))
            .trim()
            .to_string();
        if email.is_empty() || session_id.is_empty() || !email.contains('@') {
            return Err("tmpkit: 创建会话响应缺少必要字段（email/sessionId）".into());
        }
        Ok(EmailInfo {
            channel: Channel::Tmpkit,
            email,
            token: Some(format!("tempMailSession={session_id}")),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 获取 tmpkit.com 邮件列表
/// getEmails（offset 0 / limit 20）取摘要，逐封 getEmailDetail 拉详情并
/// 将详情键并入摘要；详情失败时回退列表摘要。getEmails 返回的
/// session.email 与收信邮箱不符时报错，防止会话被覆盖。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let mailbox = token
        .trim()
        .strip_prefix("tempMailSession=")
        .unwrap_or(token.trim())
        .trim();
    if mailbox.is_empty() {
        return Err("tmpkit: 会话 token 为空".into());
    }

    block_on(async {
        let data = rpc("getEmails", json!({"offset": 0, "limit": 20}), mailbox).await?;

        /* 会话指向校验：getEmails 响应自带 session.email（无 Cookie 时
           session 为 null，同样视为会话失效） */
        match data.get("session").filter(|s| s.is_object()) {
            Some(s) => {
                let got = map_get(s.get("email")).trim().to_string();
                if !got.is_empty() && got != email {
                    return Err(format!(
                        "tmpkit: 会话邮箱不匹配（响应 {got}，请求 {email}）"
                    ));
                }
            }
            None => return Err("tmpkit: 会话已失效（getEmails 返回空会话）".into()),
        }

        let list = data
            .get("emails")
            .and_then(|v| v.as_array())
            .ok_or("tmpkit: 邮件列表响应缺 emails 字段")?
            .clone();

        let mut out = Vec::with_capacity(list.len());
        for item in &list {
            if !item.is_object() {
                continue;
            }
            let mut m = item.clone();
            /* mailId 为数字：合法时逐封拉详情（详情键并入摘要，覆盖同键） */
            let mail_id = m
                .get("mailId")
                .and_then(|v| v.as_f64())
                .filter(|v| *v > 0.0);
            if let Some(mid) = mail_id {
                if let Ok(detail) =
                    rpc("getEmailDetail", json!({"mailId": mid as i64}), mailbox).await
                {
                    if let (Some(dst), Some(src)) = (m.as_object_mut(), detail.as_object()) {
                        for (k, v) in src {
                            if matches!(k.as_str(), "session" | "emails" | "total" | "error") {
                                continue;
                            }
                            dst.insert(k.clone(), v.clone());
                        }
                    }
                }
            }
            out.push(normalize_email(&m, email));
        }
        Ok(out)
    })
}