/*!
 * Mtempmail 渠道实现（mtempmail.com，公共 key 认证）
 *
 * 建箱: POST /api/emails/{apiKey}（空 JSON body）→
 *   {"status":true,"data":{"email":"xxx@domain","expire_at":"..","email_token":"..."}}
 * 读信: GET /api/messages/{apiKey}/{email} →
 *   {"status":true,"mailbox":"..","email_token":"..","messages":[]}
 * 消息为 mailgun webhook 风格：body[].{content_type,value}、created_at 等。
 * 邮箱 24 小时有效。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};

const BASE_URL: &str = "https://mtempmail.com";

/// mtempmail.com 官方提供的公共固定 API key
const PUBLIC_KEY: &str = "pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw";

/// 设置 mtempmail 请求的通用请求头
fn set_headers(req: wreq::RequestBuilder) -> wreq::RequestBuilder {
    req.header("User-Agent", get_current_ua())
        .header("Accept", "application/json")
}

/// 清洗主题前导分隔符（后台可能以 "• " 开头拼接微件）
fn clean_subject(s: &str) -> String {
    s.trim().trim_start_matches(['•', '·']).trim().to_string()
}

/// 拼接正文纯文本（body[].value 按序拼接）
fn body_text(parts: &[Value]) -> String {
    let mut out = String::new();
    for p in parts {
        if let Some(v) = p.get("value").and_then(|x| x.as_str()) {
            out.push_str(v);
            out.push('\n');
        }
    }
    out.trim_end_matches('\n').to_string()
}

/// 提取首个 text/html 段
fn body_html(parts: &[Value]) -> String {
    for p in parts {
        if p.get("content_type").and_then(|x| x.as_str()) == Some("text/html") {
            if let Some(v) = p.get("value").and_then(|x| x.as_str()) {
                return v.to_string();
            }
        }
    }
    String::new()
}

/// 提取 mailgun webhook 风格 from 数组的显示串：
/// 取首个元素的 full 字段，其次拼接 name <email>，再退 address
fn from_display(m: &Value) -> String {
    let Some(arr) = m.get("from").and_then(|v| v.as_array()) else {
        return m
            .get("from")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();
    };
    let Some(first) = arr.first() else {
        return String::new();
    };
    if let Some(full) = first.get("full").and_then(|v| v.as_str()) {
        return full.trim().to_string();
    }
    let name = first
        .get("name")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .trim();
    let address = first
        .get("address")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .trim();
    match (name.is_empty(), address.is_empty()) {
        (false, false) => format!("{name} <{address}>"),
        (false, true) => name.to_string(),
        (true, false) => address.to_string(),
        _ => String::new(),
    }
}

/// 创建 mtempmail 临时邮箱
/// POST /api/emails/{apiKey}（空 JSON body）
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let resp = set_headers(
            http_client()
                .post(format!("{BASE_URL}/api/emails/{PUBLIC_KEY}"))
                .header("Content-Type", "application/json")
                .body("{}"),
        )
        .send()
        .await
        .map_err(|e| format!("mtempmail: 创建邮箱请求失败: {e}"))?;

        if !resp.status().is_success() {
            return Err(format!("mtempmail create: http {}", resp.status()));
        }

        let data: Value = resp
            .json()
            .await
            .map_err(|e| format!("mtempmail: 解析创建响应失败: {e}"))?;

        let d = data.get("data").ok_or("mtempmail create: 响应缺少 data")?;
        let email = d
            .get("email")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if email.is_empty() {
            return Err("mtempmail create: 响应缺少邮箱".into());
        }
        let token = d
            .get("email_token")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();

        Ok(EmailInfo {
            channel: Channel::Mtempmail,
            email,
            token: Some(token),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 读取 mtempmail 收件箱
/// GET /api/messages/{apiKey}/{email}；token 元数据仅为校验，不参与请求
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    if em.is_empty() {
        return Err("mtempmail: 邮箱为空".into());
    }
    if token.trim().is_empty() {
        return Err("mtempmail: token 为空".into());
    }

    let url = format!(
        "{BASE_URL}/api/messages/{PUBLIC_KEY}/{}",
        urlencoding::encode(em)
    );

    block_on(async {
        let resp = set_headers(http_client().get(&url))
            .send()
            .await
            .map_err(|e| format!("mtempmail: 获取收件箱失败: {e}"))?;

        if !resp.status().is_success() {
            return Err(format!("mtempmail inbox: http {}", resp.status()));
        }

        let data: Value = resp
            .json()
            .await
            .map_err(|e| format!("mtempmail: 解析收件箱响应失败: {e}"))?;

        let messages = data
            .get("messages")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();

        let mut out = Vec::with_capacity(messages.len());
        for m in &messages {
            let parts = m
                .get("body")
                .and_then(|v| v.as_array())
                .cloned()
                .unwrap_or_default();
            let flat = json!({
                "id": m.get("id").and_then(|v| v.as_str()).unwrap_or(""),
                "from": from_display(m),
                "to": em,
                "subject": m.get("subject").and_then(|v| v.as_str()).map(clean_subject).unwrap_or_default(),
                "text": body_text(&parts),
                "html": body_html(&parts),
                "date": m.get("created_at").and_then(|v| v.as_str()).unwrap_or(""),
            });
            out.push(normalize_email(&flat, em));
        }
        Ok(out)
    })
}
