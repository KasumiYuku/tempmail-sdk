/*!
 * Internxt 渠道实现（internxt.com/temporary-email，Next.js + OpenNext）
 *
 * 调研实证结论（与 Go 端 internxt.go 一致）：
 *   - 建箱 GET /api/temp-mail/create-email，读信 GET
 *     /api/temp-mail/get-inbox?email=<e>&token=<t>，详情 GET
 *     /api/temp-mail/get-message?email=<e>&token=<t>&messageId=<id>。
 *     create-email 仅接受 GET（POST 返回 405 Method not allowed）。
 *   - CSRF：首次 GET /temporary-email 响应 Set-Cookie csrfSecret=...
 *     与 XSRF-TOKEN=...（均 HttpOnly）。数据接口校验请求头
 *     csrf-token，其值必须与 Cookie jar 中 XSRF-TOKEN 一致：
 *     头=XSRF-TOKEN 值时 200；头=csrfSecret 值时恒定 500。
 *   - 建箱响应：{"address":"<前缀>@uberip.com","token":"<十六进制>"}。
 *     健壮性提醒：每个 API 响应均会刷新 XSRF-TOKEN 的 Set-Cookie，
 *     故每次读信前都应从 Cookie 罐重取最新值作为 csrf-token 头。
 *   - get-inbox 正常返回顶层数组（空箱 []）；错误 token 返回 401
 *     {"message":"Email has expired"}。
 *
 * 会话粘性：SDK 全局客户端无共享 Cookie 池负担——本渠道用无 Cookie 罐
 *   客户端 + 模块级自管 Cookie（csrfSecret / XSRF-TOKEN 静态状态，响应
 *   Set-Cookie 逐次刷新），杜绝跨渠道与并行会话串池。
 * token 语义：{address, token} JSON（收信凭据）。
 */

use std::sync::RwLock;

use crate::config::{block_on, get_current_ua, http_client_no_cookie_jar};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};
use wreq::Client;

const SITE: &str = "https://internxt.com";
const REF: &str = "https://internxt.com/temporary-email";
const API_BASE: &str = "https://internxt.com/api/temp-mail";

/// 模块级 Cookie 状态（csrfSecret 恒定，XSRF-TOKEN 每个 API 响应刷新）
static COOKIES: RwLock<(String, String)> = RwLock::new((String::new(), String::new()));

/// 收下响应 Set-Cookie：XSRF-TOKEN / csrfSecret 同名覆写
fn merge_set_cookies(resp: &wreq::Response) {
    let mut guard = COOKIES.write().unwrap();
    for val in resp.headers().get_all("set-cookie").iter() {
        let raw = val.to_str().unwrap_or("");
        if let Some(kv) = raw.split(';').next() {
            let mut parts = kv.trim().splitn(2, '=');
            let (Some(name), Some(value)) = (parts.next(), parts.next()) else {
                continue;
            };
            match name {
                "XSRF-TOKEN" => guard.0 = value.to_string(),
                "csrfSecret" => guard.1 = value.to_string(),
                _ => {}
            }
        }
    }
}

/// 组装 Cookie 请求头（有则携带）
fn cookie_header() -> String {
    let (xsrf, secret) = COOKIES.read().unwrap().clone();
    let mut parts = Vec::new();
    if !secret.is_empty() {
        parts.push(format!("csrfSecret={secret}"));
    }
    if !xsrf.is_empty() {
        parts.push(format!("XSRF-TOKEN={xsrf}"));
    }
    parts.join("; ")
}

/// 确保持有本域 csrfSecret 与 XSRF-TOKEN 并返回当前 XSRF-TOKEN 值
async fn prepare_xsrf(client: &Client) -> Result<String, String> {
    let existing = COOKIES.read().unwrap().0.clone();
    if !existing.is_empty() {
        return Ok(existing);
    }

    let resp = client
        .get(REF)
        .header("User-Agent", get_current_ua())
        .header(
            "Accept",
            "text/html,application/xhtml+xml,application/xml;q=0.9,\
             image/avif,image/webp,*/*;q=0.8",
        )
        .header("Accept-Language", "en-US,en;q=0.9")
        .send()
        .await
        .map_err(|e| format!("internxt: 夺取页面请求失败: {e}"))?;
    merge_set_cookies(&resp);
    let _ = resp.text().await;
    let xsrf = COOKIES.read().unwrap().0.clone();
    if xsrf.is_empty() {
        return Err("internxt: 未取得 XSRF-TOKEN Cookie".into());
    }
    Ok(xsrf)
}

/// 带 CSRF 头请求 internxt 数据接口（GET），返回解析后的 JSON Value
/// csrf-token 头取模块 Cookie 状态中 XSRF-TOKEN 最新值（每次请求前重取，
/// 因为每个 API 响应都会刷新该 Cookie）。
async fn api_get(client: &Client, path: &str, query: &[&str]) -> Result<Value, String> {
    let csrf_token = prepare_xsrf(client).await?;
    let cookie = cookie_header();

    let mut url = format!("{API_BASE}{path}");
    if !query.is_empty() {
        url.push('?');
        url.push_str(&query.join("&"));
    }
    let resp = client
        .get(&url)
        .header("User-Agent", get_current_ua())
        .header("Accept", "application/json, text/plain, */*")
        .header("Origin", SITE)
        .header("Referer", REF)
        .header("csrf-token", csrf_token)
        .header("Cookie", cookie)
        .send()
        .await
        .map_err(|e| format!("internxt: {path} 请求失败: {e}"))?;
    let status = resp.status();
    merge_set_cookies(&resp);
    let body = resp
        .text()
        .await
        .map_err(|e| format!("internxt: {path} 读取响应失败: {e}"))?;
    if !status.is_success() {
        return Err(format!("internxt: {path} 失败 http {status}: {body}"));
    }
    serde_json::from_str(&body).map_err(|e| format!("internxt: 解析 {path} 响应失败: {e}"))
}

/// 创建 internxt.com 临时邮箱
/// prepare_xsrf 确保已取得 XSRF-TOKEN，再 GET /api/temp-mail/create-email
/// （带 csrf-token 头），响应 {"address","token"}。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let client = http_client_no_cookie_jar();
        prepare_xsrf(&client).await?;

        let data = api_get(&client, "/create-email", &[]).await?;
        let address = data
            .get("address")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        let api_token = data
            .get("token")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if address.is_empty() || api_token.is_empty() || !address.contains('@') {
            return Err("internxt: 创建邮箱响应缺少必要字段（address/token）".into());
        }
        Ok(EmailInfo {
            channel: Channel::Internxt,
            email: address.clone(),
            token: Some(json!({"address": address, "token": api_token}).to_string()),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 从列表元素中提取邮件 ID，候选字段 id/messageId/message_id（与 Go 端一致）
fn message_id_of(m: &Value) -> String {
    for key in ["id", "messageId", "message_id"] {
        if let Some(v) = m.get(key) {
            let s = match v {
                Value::String(s) => s.clone(),
                other => other.to_string(),
            };
            if !s.trim().is_empty() {
                return s;
            }
        }
    }
    String::new()
}

/// 获取 internxt.com 收件箱
/// 流程：GET /get-inbox?email=&token= 返回顶层数组（列表元素含
/// id/from/subject/date/seen 等）；逐条 GET /get-message 拉单封全文
/// （响应为单封对象，含 html 渲染全文），详情失败回退列表摘要。
///
/// @param email 邮箱地址
/// @param token 会话凭据 JSON（address/token）
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let sess: Value = serde_json::from_str(token)
        .map_err(|e| format!("internxt: 会话凭据解析失败: {e}"))?;
    let address = sess
        .get("address")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .trim()
        .to_string();
    let api_token = sess
        .get("token")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .trim()
        .to_string();
    if address.is_empty() || api_token.is_empty() {
        return Err("internxt: 会话凭据缺少必要字段".into());
    }
    if address != email {
        return Err("internxt: 会话邮箱与查询邮箱不匹配".into());
    }

    block_on(async {
        let client = http_client_no_cookie_jar();

        let inbox = api_get(
            &client,
            "/get-inbox",
            &[&format!("email={address}"), &format!("token={api_token}")],
        )
        .await?;
        let list = inbox.as_array().cloned().unwrap_or_default();

        let mut out = Vec::with_capacity(list.len());
        for item in &list {
            if !item.is_object() {
                continue;
            }
            let mut m = item.clone();
            let mid = message_id_of(&m);
            if !mid.is_empty() {
                if let Ok(detail) = api_get(
                    &client,
                    "/get-message",
                    &[
                        &format!("email={address}"),
                        &format!("token={api_token}"),
                        &format!("messageId={mid}"),
                    ],
                )
                .await
                {
                    if detail.is_object() {
                        if let (Some(dst), Some(src)) =
                            (m.as_object_mut(), detail.as_object())
                        {
                            /* 列表字段优先，详情仅补齐缺失字段 */
                            for (k, v) in src {
                                dst.entry(k.clone()).or_insert_with(|| v.clone());
                            }
                        }
                    }
                }
            }
            out.push(normalize_email(&m, email));
        }
        Ok(out)
    })
}