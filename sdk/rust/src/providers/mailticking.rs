/*!
 * MailTicking 渠道实现（www.mailticking.com，旧域名 temporary-mail.net 的更名站）
 *
 * 实测协议（JSON 交流、Cloudflare 前置、需浏览器 UA，与 Go 端 mailticking.go
 * 一致；触发限流后回 429 且 need_captcha，SDK 低频调用不受影响）：
 *   1. 建箱  POST /get-mailbox   body {"types":["4"]}（4=独立域名；排除
 *      Gmail 别名（type "2"））
 *      响应 {"success":true,"email":"xxx@domain","activate_token":"..."}
 *   2. 激活  POST /activate-email body {"email":..,"source":"homepage",
 *      "activate_token":..}      响应 {"success":true}
 *   3. 同步  GET /?email=..&activate_token=..  服务端把该邮箱写入活跃状态，
 *      页面 #active-mail 渲染 value="{email}" data-code="{64位列信码}";
 *      列信码是"当前活跃邮箱"的派生凭据，与 activate_token 是两套值
 *   4. 列信  POST /get-emails?lang=en body {"email":..,"code":列信码}
 *      空箱实测响应 {"emails":[],"success":true}；`?lang=` 缺省直接 400；
 *      邮箱不活跃（code 失效）时回 {"error":"Invalid request","success":false}
 *
 * token 语义：单字符串 "{列信码}|{email}"。
 * 读信协议：官网没有可观察的独立读信端点，当前实现只提供列表；
 * 列表元素字段名做多候选映射，其余交给 normalize 既有候选字段策略提取。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use regex::Regex;
use serde_json::{json, Value};

const BASE_URL: &str = "https://www.mailticking.com";

/// 首页 #active-mail 的 input 标签与 value/data-code 提取正则
fn input_id_re() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| Regex::new(r#"(?s)<input\b[^>]*\bid=['"]active-mail['"][^>]*>"#).unwrap())
}

fn value_re() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| Regex::new(r#"(?s)\bvalue=['"]([^'"]*)['"]"#).unwrap())
}

fn code_re() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| Regex::new(r#"(?s)\bdata-code=['"]([^'"]*)['"]"#).unwrap())
}

/// 取正则第一个捕获组，无匹配返回空串
fn match_first(re: &Regex, src: &str) -> String {
    re.captures(src)
        .and_then(|m| m.get(1))
        .map(|m| m.as_str().to_string())
        .unwrap_or_default()
}

/// POST JSON 接口并从联合视图解析（error/message 优先进入报错文案）
async fn post_json(path: &str, payload: Value) -> Result<Value, String> {
    let host = "www.mailticking.com";
    let resp = http_client()
        .post(path)
        .header("Accept", "application/json")
        .header("Accept-Language", "en-US,en;q=0.9")
        .header("Content-Type", "application/json")
        .header("User-Agent", get_current_ua())
        .header("Referer", format!("https://{host}/"))
        .header("Origin", format!("https://{host}"))
        .body(payload.to_string())
        .send()
        .await
        .map_err(|e| format!("mailticking: {path} 请求失败: {e}"))?;
    let status = resp.status();
    let body = resp
        .text()
        .await
        .map_err(|e| format!("mailticking: {path} 读取响应失败: {e}"))?;

    let data: Value = serde_json::from_str(body.trim()).map_err(|e| {
        /* 站点偶尔返回非 JSON 网关文案 */
        if !status.is_success() {
            return format!("mailticking: {path} http {status}: {}", body.trim());
        }
        format!("mailticking: {path} 解析响应失败: {e}")
    })?;
    if !status.is_success() {
        let msg = data
            .get("error")
            .or_else(|| data.get("message"))
            .and_then(|v| v.as_str())
            .filter(|s| !s.is_empty())
            .unwrap_or(body.trim());
        return Err(format!("mailticking: {path} http {status}: {msg}"));
    }
    Ok(data)
}

/// GET 页面文本（同步会话用）
async fn get_page(u: &str) -> Result<String, String> {
    let resp = http_client()
        .get(u)
        .header(
            "Accept",
            "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
        )
        .header("Accept-Language", "en-US,en;q=0.9")
        .header("User-Agent", get_current_ua())
        .send()
        .await
        .map_err(|e| format!("mailticking: {u} 请求失败: {e}"))?;
    let status = resp.status();
    let body = resp
        .text()
        .await
        .map_err(|e| format!("mailticking: {u} 读取响应失败: {e}"))?;
    if !status.is_success() {
        return Err(format!("mailticking: {u} http {status}"));
    }
    Ok(body)
}

/// 从首页 HTML 解析活跃邮箱 value 与 data-code
fn parse_active_mail(page: &str) -> Option<(String, String)> {
    let tag = input_id_re().find(page).map(|m| m.as_str())?;
    let val = match_first(value_re(), tag).trim().to_string();
    let cod = match_first(code_re(), tag).trim().to_string();
    if val.is_empty() || cod.is_empty() {
        return None;
    }
    Some((val, cod))
}

/// 激活后的会话同步：带参加载首页 → 解析 #active-mail（value=邮箱, data-code=列信码）
async fn sync_code(email: &str, activate_token: &str) -> Result<(String, String), String> {
    let u = format!(
        "{BASE_URL}/?email={}&activate_token={}",
        urlencoding::encode(email),
        urlencoding::encode(activate_token)
    );
    let page = get_page(&u).await?;
    parse_active_mail(&page).ok_or_else(|| "mailticking: 同步收件箱失败".to_string())
}

/// 创建 mailticking 邮箱账号（type=4 独立域名）
/// 流程：建箱 → 激活 → 会话同步出列信码；token 存 "{列信码}|{email}"。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let box_data = post_json(
            &format!("{BASE_URL}/get-mailbox"),
            json!({"types": ["4"]}),
        )
        .await?;
        if box_data.get("success").and_then(|v| v.as_bool()) != Some(true) {
            let msg = box_data
                .get("error")
                .or_else(|| box_data.get("message"))
                .and_then(|v| v.as_str())
                .unwrap_or("unknown error");
            return Err(format!("mailticking: get-mailbox failed: {msg}"));
        }
        let box_email = box_data
            .get("email")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        let activate_token = box_data
            .get("activate_token")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if box_email.is_empty() {
            return Err("mailticking: get-mailbox returned empty email".into());
        }
        if activate_token.is_empty() {
            return Err("mailticking: get-mailbox returned empty activate_token".into());
        }

        /* 激活邮箱，服务端据此登记活跃状态 */
        let act = post_json(
            &format!("{BASE_URL}/activate-email"),
            json!({
                "email": box_email,
                "source": "homepage",
                "activate_token": activate_token,
            }),
        )
        .await?;
        if act.get("success").and_then(|v| v.as_bool()) != Some(true) {
            let msg = act
                .get("error")
                .or_else(|| act.get("message"))
                .and_then(|v| v.as_str())
                .unwrap_or("unknown error");
            return Err(format!("mailticking: activate-email failed: {msg}"));
        }

        /* 会话同步：带参加载首页，取列信码（列信必需的最新派生凭据） */
        let (sync_email, code) = sync_code(&box_email, &activate_token).await?;
        if sync_email != box_email {
            return Err(format!(
                "mailticking: inbox session mismatch: want {box_email} got {sync_email}"
            ));
        }

        Ok(EmailInfo {
            channel: Channel::Mailticking,
            email: box_email.clone(),
            token: Some(format!("{code}|{box_email}")),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 从 token 拆出列信码与邮箱
fn split_token(token: &str, email: &str) -> (String, String) {
    if let Some(i) = token.find('|') {
        (
            token[..i].trim().to_string(),
            token[i + 1..].trim().to_string(),
        )
    } else {
        (token.trim().to_string(), email.trim().to_string())
    }
}

/// 尽量映射列表字段到统一字段名（多候选提取，未命中的字段交给 normalize 处理）
fn migrated_fields(raw: &Value) -> Value {
    let mut m = raw.clone();
    if !m.is_object() {
        return m;
    }
    let obj = m.as_object_mut().unwrap();

    if obj.get("from").is_none() {
        for key in [
            "mail_from",
            "from_mail",
            "from_email",
            "sender_address",
            "from_address",
            "send_addr",
            "mail_addr",
            "address_from",
            "ho_from",
            "fromname",
            "fromS",
        ] {
            if let Some(s) = obj.get(key).and_then(|v| v.as_str()) {
                if !s.trim().is_empty() {
                    obj.insert("from".to_string(), json!(s));
                    break;
                }
            }
        }
    }
    if obj.get("sender").is_none() {
        if let Some(s) = obj.get("from").and_then(|v| v.as_str()) {
            obj.insert("sender".to_string(), json!(s));
        }
    }
    if obj.get("id").is_none() {
        if let Some(v) = obj.get("mail_id").cloned() {
            obj.insert("id".to_string(), v);
        }
    }
    if obj.get("date").is_none() {
        if let Some(v) = obj.get("received_at").cloned() {
            obj.insert("date".to_string(), v);
        }
    }
    m
}

/// 获取 mailticking 邮箱的邮件列表
/// 发送 {"email":邮箱, "code":token 中的列信码}，空箱返回空列表不报错。
/// 列表字段做候选提取，能识别多少字段取决于站点实际响应。
///
/// @param email 邮箱地址
/// @param token 会话凭据串（"列信码|邮箱"）
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let email = email.trim();
    if email.is_empty() {
        return Err("mailticking: empty email".into());
    }
    let token = token.trim();
    if token.is_empty() {
        return Err("mailticking: empty token".into());
    }
    let (code, tok_email) = split_token(token, email);

    block_on(async {
        let list = post_json(
            &format!("{BASE_URL}/get-emails?lang=en"),
            json!({"email": tok_email, "code": code}),
        )
        .await?;
        if list.get("success").and_then(|v| v.as_bool()) != Some(true) {
            if list.get("needNewEmail").and_then(|v| v.as_bool()) == Some(true) {
                return Err("mailticking: mailbox expired, please renew".into());
            }
            if tok_email == email {
                return Err(
                    "mailticking: get-emails rejected, refresh inbox in Generate".into(),
                );
            }
            return Err("mailticking: get-emails failed".into());
        }
        if tok_email != email {
            return Err("mailticking: token email mismatch".into());
        }

        let emails = list.get("emails").and_then(|v| v.as_array()).cloned();
        let mut out = Vec::new();
        if let Some(rows) = emails {
            for raw in rows {
                if raw.is_null() {
                    continue;
                }
                let flat = migrated_fields(&raw);
                out.push(normalize_email(&flat, email));
            }
        }
        Ok(out)
    })
}