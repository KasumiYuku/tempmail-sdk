/*!
 * Crazymailing 渠道实现（crazymailing.com）
 * Next.js 全栈站点，API 结构：
 * - 建箱 POST /api/mailbox（空 JSON body），响应 {"mailbox":{"id","address","expiresAt"}}。
 * - 读信 GET /api/messages?mailbox=<完整地址>，响应 {"messages":[...]}。
 * - 单封正文 GET /api/message/{id}/body，响应为完整 HTML 页面。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};

const BASE_URL: &str = "https://crazymailing.com";

/// 构造 crazymailing 请求并铺设通用请求头
fn base_request(mut req: wreq::RequestBuilder) -> wreq::RequestBuilder {
    req = req
        .header("Accept", "application/json")
        .header("Origin", BASE_URL)
        .header("Referer", format!("{BASE_URL}/"))
        .header("User-Agent", get_current_ua());
    req
}

/// 从列表元素中提取邮件 ID，候选字段 id/_id/messageId/message_id/slug
fn message_id_of(m: &Value) -> String {
    for key in ["id", "_id", "messageId", "message_id", "slug"] {
        if let Some(v) = m.get(key) {
            let s = match v {
                Value::String(s) => s.clone(),
                other => other.to_string(),
            };
            if !s.trim().is_empty() {
                return s.trim().to_string();
            }
        }
    }
    String::new()
}

/// 创建 crazymailing 临时邮箱（POST /api/mailbox，空 JSON body）
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let resp = base_request(http_client().post(format!("{BASE_URL}/api/mailbox")))
            .header("Content-Type", "application/json")
            .body("{}")
            .send()
            .await
            .map_err(|e| format!("crazymailing: 创建邮箱请求失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("crazymailing: 读取创建响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("crazymailing: 创建邮箱失败 http {status}: {body}"));
        }

        let data: Value = serde_json::from_str(&body)
            .map_err(|e| format!("crazymailing: 解析创建响应失败: {e}"))?;
        let mailbox = data.get("mailbox");
        let id = mailbox
            .and_then(|v| v.get("id"))
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        let address = mailbox
            .and_then(|v| v.get("address"))
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        let expires_at = mailbox
            .and_then(|v| v.get("expiresAt"))
            .and_then(|v| v.as_str())
            .filter(|s| !s.is_empty())
            .and_then(|s| {
                chrono::NaiveDateTime::parse_from_str(s, "%Y-%m-%dT%H:%M:%S%.fZ")
                    .ok()
                    .or_else(|| {
                        chrono::DateTime::parse_from_rfc3339(s)
                            .ok()
                            .map(|dt| dt.naive_utc())
                    })
                    .map(|dt| dt.and_utc().timestamp())
            });
        if address.is_empty() {
            return Err(format!(
                "crazymailing: 创建响应缺少 mailbox.address: {body}"
            ));
        }

        Ok(EmailInfo {
            channel: Channel::Crazymailing,
            email: address,
            token: Some(id),
            expires_at,
            created_at: None,
        })
    })
}

/// 拉取单封正文（响应为完整 HTML 页面），失败返回空串
async fn fetch_body(id: &str) -> String {
    let req = http_client()
        .get(format!(
            "{BASE_URL}/api/message/{}/body",
            urlencoding::encode(id)
        ))
        .header("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
        .header("Origin", BASE_URL)
        .header("Referer", format!("{BASE_URL}/"))
        .header("User-Agent", get_current_ua());
    match req.send().await {
        Ok(resp) if resp.status().is_success() => match resp.text().await {
            Ok(body) => body.trim().to_string(),
            Err(_) => String::new(),
        },
        _ => String::new(),
    }
}

/// 读取 crazymailing 收件箱（逐封二拉正文，详情失败以列表摘要归一）
pub fn get_emails(email: &str, _token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    if em.is_empty() {
        return Err("crazymailing: 邮箱地址为空".into());
    }

    block_on(async {
        let resp = base_request(http_client().get(format!(
            "{BASE_URL}/api/messages?mailbox={}",
            urlencoding::encode(em)
        )))
        .send()
        .await
        .map_err(|e| format!("crazymailing: 读取收件箱请求失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("crazymailing: 读取收件箱响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!(
                "crazymailing: 读取收件箱失败 http {status}: {body}"
            ));
        }

        let list: Value = serde_json::from_str(&body)
            .map_err(|e| format!("crazymailing: 解析收件箱响应失败: {e}"))?;
        let messages = list
            .get("messages")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();

        let mut out = Vec::with_capacity(messages.len());
        for raw in &messages {
            let mut flat = raw.clone();
            if flat.get("to").is_none() {
                if let Some(obj) = flat.as_object_mut() {
                    obj.insert("to".to_string(), json!(em));
                }
            }
            /* 正文须逐封二拉；详情失败时以列表摘要归一，不阻断列表 */
            let id = message_id_of(raw);
            if !id.is_empty() {
                let html = fetch_body(&id).await;
                if !html.is_empty() {
                    if let Some(obj) = flat.as_object_mut() {
                        obj.insert("html".to_string(), json!(html));
                    }
                }
            }
            out.push(normalize_email(&flat, em));
        }
        Ok(out)
    })
}
