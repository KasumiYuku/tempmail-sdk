/*!
 * ClawdEmail 渠道实现（clawdemail.com，API 域 api.clawdemail.com）
 * POST /register 建箱（可选 name，响应 success/email/token），
 * GET /inbox?limit=50 读信列表（Header Authorization: Bearer <token>，
 *   响应 success/email/count/unread/emails[]），
 * GET /email/{id} 取单封详情（Bearer，响应含 email 嵌套对象
 *   from_addr/subject/body_text/received_at）。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::Value;

const BASE_URL: &str = "https://api.clawdemail.com";

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

/// 获取单封邮件详情（Bearer token），响应含 email 嵌套对象时提升之
async fn fetch_detail(token: &str, message_id: &str) -> Option<Value> {
    let base_id = message_id.rsplit('/').next().unwrap_or(message_id);
    let resp = http_client()
        .get(format!("{BASE_URL}/email/{}", urlencoding::encode(base_id)))
        .header("Accept", "application/json")
        .header("User-Agent", get_current_ua())
        .header("Authorization", format!("Bearer {token}"))
        .send()
        .await
        .ok()?;
    if !resp.status().is_success() {
        return None;
    }
    let detail = resp.json::<Value>().await.ok()?;
    /* 详情为 {success,email:{...},code,links} 时提升嵌套 email 对象 */
    match detail.get("email").cloned() {
        Some(nested) if nested.is_object() => Some(nested),
        _ => Some(detail),
    }
}

/// 创建 clawdemail.com 临时邮箱
/// POST /register（name 空串）返回 email 与 token。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let resp = http_client()
            .post(format!("{BASE_URL}/register"))
            .header("Content-Type", "application/json")
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .body(serde_json::json!({"name": ""}).to_string())
            .send()
            .await
            .map_err(|e| format!("clawdemail: 创建邮箱请求失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("clawdemail: 读取创建响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("clawdemail: 创建邮箱失败 http {status}: {body}"));
        }
        let data: Value = serde_json::from_str(&body)
            .map_err(|e| format!("clawdemail: 解析创建响应失败: {e}"))?;
        let email = data
            .get("email")
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
        if email.is_empty() || token.is_empty() || !email.contains('@') {
            return Err(format!("clawdemail: 创建邮箱响应缺少必要字段: {body}"));
        }

        Ok(EmailInfo {
            channel: Channel::Clawdemail,
            email,
            token: Some(token),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 获取 clawdemail.com 邮件列表
/// GET /inbox?limit=50 取列表，按 id 逐封 GET /email/{id} 合并详情；
/// 详情失败时以列表摘要归一。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    let tok = token.trim();

    block_on(async {
        let resp = http_client()
            .get(format!("{BASE_URL}/inbox?limit=50"))
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .header("Authorization", format!("Bearer {tok}"))
            .send()
            .await
            .map_err(|e| format!("clawdemail: 获取邮件列表请求失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("clawdemail: 读取邮件列表响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!(
                "clawdemail: 获取邮件列表失败 http {status}: {body}"
            ));
        }
        let data: Value = serde_json::from_str(&body)
            .map_err(|e| format!("clawdemail: 解析邮件列表失败: {e}"))?;
        if data.get("success").and_then(|v| v.as_bool()) != Some(true) {
            let error = data.get("error").and_then(|v| v.as_str()).unwrap_or("");
            return Err(format!("clawdemail: 读取收件箱失败: {error}"));
        }
        let emails = data
            .get("emails")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();

        let mut out = Vec::with_capacity(emails.len());
        for m in &emails {
            let id = message_id_of(m);
            if id.is_empty() {
                out.push(normalize_email(m, em));
                continue;
            }
            match fetch_detail(tok, &id).await {
                Some(detail) => {
                    /* 详情仅补充列表缺失字段 */
                    let mut merged = m.clone();
                    if let (Some(f), Some(d)) = (merged.as_object_mut(), detail.as_object()) {
                        for (k, v) in d {
                            f.entry(k.clone()).or_insert_with(|| v.clone());
                        }
                    }
                    out.push(normalize_email(&merged, em));
                }
                None => {
                    /* 详情失败时回退为列表摘要 */
                    out.push(normalize_email(m, em));
                }
            }
        }
        Ok(out)
    })
}
