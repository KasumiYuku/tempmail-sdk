/*!
 * Smails 渠道实现（smails.dev）
 *
 * 建箱: POST /api/mailbox（空 JSON body）→ address/token
 * 读信: GET /api/mailbox/messages（Header Authorization: Bearer <token>）取列表，
 *       对每个元素按 id 逐封 GET /api/mailbox/messages/{id} 合并详情；
 *       详情失败时以列表摘要归一。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};

const BASE_URL: &str = "https://smails.dev";

/// 设置 smails.dev 请求的通用请求头
fn auth_headers(req: wreq::RequestBuilder, token: &str) -> wreq::RequestBuilder {
    let mut req = req
        .header("User-Agent", get_current_ua())
        .header("Accept", "application/json");
    if !token.is_empty() {
        req = req.header("Authorization", format!("Bearer {token}"));
    }
    req
}

/// 创建 smails.dev 临时邮箱
/// POST /api/mailbox（空 JSON body）返回 address 与 token
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let resp = auth_headers(
            http_client()
                .post(format!("{BASE_URL}/api/mailbox"))
                .header("Content-Type", "application/json")
                .body("{}"),
            "",
        )
        .send()
        .await
        .map_err(|e| format!("smails: 创建邮箱请求失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("smails: 读取创建响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("smails: 创建邮箱失败 http {status}: {body}"));
        }

        let data: Value =
            serde_json::from_str(&body).map_err(|e| format!("smails: 解析创建响应失败: {e}"))?;
        let address = data
            .get("address")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        let token = data
            .get("token")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if address.is_empty() || token.is_empty() {
            return Err(format!("smails: 创建邮箱响应缺少必要字段: {body}"));
        }

        Ok(EmailInfo {
            channel: Channel::Smails,
            email: address,
            token: Some(token),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 获取 smails.dev 邮件列表
/// 流程：GET /api/mailbox/messages 取列表，按 id 逐封 GET /api/mailbox/messages/{id}
/// 合并详情；详情失败时以列表摘要归一。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    let tok = token.trim();
    if tok.is_empty() {
        return Err("smails: token 为空".into());
    }

    block_on(async {
        let resp = auth_headers(
            http_client().get(format!("{BASE_URL}/api/mailbox/messages")),
            tok,
        )
        .send()
        .await
        .map_err(|e| format!("smails: 获取邮件列表失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("smails: 读取邮件列表失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("smails: 获取邮件列表失败 http {status}: {body}"));
        }

        let list: Value =
            serde_json::from_str(&body).map_err(|e| format!("smails: 解析邮件列表失败: {e}"))?;
        let messages = list.as_array().cloned().unwrap_or_default();

        let mut out = Vec::with_capacity(messages.len());
        for m in &messages {
            let id = message_id_of(m);
            let mut flat = m.clone();
            if let Some(obj) = flat.as_object_mut() {
                obj.insert("to".to_string(), json!(em));
            }
            /* 拉取单封详情并合并缺失字段；失败时以列表摘要归一 */
            if let Some(detail) = fetch_detail(tok, &id).await {
                if let (Some(f), Some(d)) = (flat.as_object_mut(), detail.as_object()) {
                    for (k, v) in d {
                        f.entry(k.clone()).or_insert_with(|| v.clone());
                    }
                }
            }
            out.push(normalize_email(&flat, em));
        }
        Ok(out)
    })
}

/// 从列表元素中提取邮件 ID，候选字段 id/Id/slug/messageId/message_id
fn message_id_of(m: &Value) -> String {
    for key in ["id", "Id", "slug", "messageId", "message_id"] {
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

/// 获取 smails.dev 单封邮件详情
async fn fetch_detail(token: &str, message_id: &str) -> Option<Value> {
    if message_id.is_empty() {
        return None;
    }
    let resp = auth_headers(
        http_client().get(format!(
            "{BASE_URL}/api/mailbox/messages/{}",
            urlencoding::encode(message_id)
        )),
        token,
    )
    .send()
    .await
    .ok()?;
    if !resp.status().is_success() {
        return None;
    }
    resp.json().await.ok()
}
