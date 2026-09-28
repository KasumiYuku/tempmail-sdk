/*!
 * tempmails-io 渠道实现（tempmails.io）
 *
 * 无认证 REST：
 *   建箱: POST /api/temp-mail/generate（空 body），响应 success/data{email,token,expires_at}
 *   读信: 先 POST /api/temp-mail/fetch-emails/{token} 触发平台主动同步上游信箱，
 *        再 GET /api/temp-mail/inbox/{token} 取 messages[]。
 * 只轮询 inbox 会读不到新邮件（fetch 后才同步）。
 * 邮箱借用 uberip.com 等公共域，10 分钟自动过期。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};

const BASE_URL: &str = "https://tempmails.io";

/// 设置 tempmails.io 请求通用请求头
fn set_headers(req: wreq::RequestBuilder) -> wreq::RequestBuilder {
    req.header("User-Agent", get_current_ua())
        .header("Accept", "application/json")
}

/// 创建 10 分钟临时邮箱
/// POST /api/temp-mail/generate（空 JSON body），响应 data.email/data.token
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let resp = set_headers(
            http_client()
                .post(format!("{BASE_URL}/api/temp-mail/generate"))
                .header("Content-Type", "application/json")
                .body("{}"),
        )
        .send()
        .await
        .map_err(|e| format!("tempmails-io: 创建邮箱请求失败: {e}"))?;

        if !resp.status().is_success() {
            return Err(format!("tempmails-io generate: http {}", resp.status()));
        }

        let data: Value = resp
            .json()
            .await
            .map_err(|e| format!("tempmails-io: 解析创建响应失败: {e}"))?;

        let email = data
            .get("data")
            .and_then(|d| d.get("email"))
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        let token = data
            .get("data")
            .and_then(|d| d.get("token"))
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if email.is_empty() || token.is_empty() {
            return Err("tempmails-io generate: missing email or token".into());
        }

        let expires_at = data
            .get("data")
            .and_then(|d| d.get("expires_at"))
            .and_then(|v| v.as_str())
            .filter(|s| !s.is_empty())
            .map(|s| s.to_string());

        Ok(EmailInfo {
            channel: Channel::TempmailsIo,
            email,
            token: Some(token),
            expires_at: expires_at.and_then(|s| parse_millis(&s)),
            created_at: None,
        })
    })
}

/// 将 "2026-09-27T12:00:00.123Z" 类时间转毫秒时间戳，失败返回 None
fn parse_millis(s: &str) -> Option<i64> {
    chrono::DateTime::parse_from_rfc3339(s)
        .map(|dt| dt.timestamp_millis())
        .ok()
        .or_else(|| s.parse::<i64>().ok())
}

/// 读取收件箱
/// 流程：POST fetch-emails 触发同步（失败不致命）→ GET inbox 读静态收件箱。
/// messages[] 元素字段为 from_email/text_body/html_body/received_at，映射后归一化。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let token = token.trim();
    if token.is_empty() {
        return Err("tempmails-io: token 为空".into());
    }
    let em = email.trim();

    block_on(async {
        /* 1) 触发平台对上游信箱的主动同步（失败不阻断，仍尝试静态读） */
        let _ = set_headers(
            http_client().post(format!("{BASE_URL}/api/temp-mail/fetch-emails/{token}")),
        )
        .send()
        .await;

        /* 2) 读静态收件箱 */
        let resp =
            set_headers(http_client().get(format!("{BASE_URL}/api/temp-mail/inbox/{token}")))
                .send()
                .await
                .map_err(|e| format!("tempmails-io: 获取收件箱失败: {e}"))?;

        if !resp.status().is_success() {
            return Err(format!("tempmails-io inbox: http {}", resp.status()));
        }

        let data: Value = resp
            .json()
            .await
            .map_err(|e| format!("tempmails-io: 解析收件箱响应失败: {e}"))?;

        let messages = data
            .get("data")
            .and_then(|d| d.get("messages"))
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();

        let mut out = Vec::with_capacity(messages.len());
        for m in &messages {
            let flat = json!({
                "id": m.get("id").and_then(|v| v.as_str()).unwrap_or(""),
                "from": m.get("from_email").and_then(|v| v.as_str()).unwrap_or(""),
                "to": em,
                "subject": m.get("subject").and_then(|v| v.as_str()).unwrap_or(""),
                "text": m.get("text_body").and_then(|v| v.as_str()).unwrap_or(""),
                "html": m.get("html_body").and_then(|v| v.as_str()).unwrap_or(""),
                "date": m.get("received_at").and_then(|v| v.as_str()).unwrap_or(""),
                "attachments": m.get("attachments").cloned().unwrap_or(Value::Null),
            });
            out.push(normalize_email(&flat, em));
        }
        Ok(out)
    })
}
