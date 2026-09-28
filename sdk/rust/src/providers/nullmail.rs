/*!
 * Nullmail 渠道实现（nullmail.cc / maildock.store）
 *
 * 无认证 REST：
 *   建箱: POST /api/emails（空 JSON body）→ {"address":"...@maildock.store","expiry":"..."}
 *   读信: GET /api/emails/{address}（URL 编码）→ {"expiry":"...","emails":[...]}
 *   正文: 列表项只有 id/sender/subject/delivered，正文须逐封二拉
 *         GET /api/emails/{addr}/body/{id}（响应 {"body":...}）
 * token 复用完整地址以便收件箱回查。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};

const BASE_URL: &str = "https://www.nullmail.cc";

/// 设置 nullmail 请求的通用请求头
fn set_headers(req: wreq::RequestBuilder) -> wreq::RequestBuilder {
    req.header("User-Agent", get_current_ua())
        .header("Accept", "application/json")
        .header("Origin", BASE_URL)
        .header("Referer", format!("{BASE_URL}/"))
}

/// 创建临时邮箱
/// POST /api/emails（空 JSON body），token 复用完整地址以便收件箱回查
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let resp = set_headers(
            http_client()
                .post(format!("{BASE_URL}/api/emails"))
                .header("Content-Type", "application/json")
                .body("{}"),
        )
        .send()
        .await
        .map_err(|e| format!("nullmail: 建箱请求失败: {e}"))?;

        let status = resp.status();
        let raw = resp
            .text()
            .await
            .map_err(|e| format!("nullmail: 读取建箱响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("nullmail 建箱: http {status} {}", raw.trim()));
        }

        let data: Value =
            serde_json::from_str(&raw).map_err(|e| format!("nullmail: 解析建箱响应失败: {e}"))?;
        let address = data
            .get("address")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if address.is_empty() {
            return Err("nullmail 建箱: 响应缺少 address 字段".into());
        }

        Ok(EmailInfo {
            channel: Channel::Nullmail,
            email: address.clone(),
            token: Some(address),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 单封正文二拉
/// GET /api/emails/{addr}/body/{id}，响应 {"body":"<完整纯文本正文>"}
async fn fetch_body(addr: &str, id: &str) -> Option<String> {
    let url = format!(
        "{BASE_URL}/api/emails/{}/body/{}",
        urlencoding::encode(addr),
        urlencoding::encode(id)
    );
    let resp = set_headers(http_client().get(&url)).send().await.ok()?;
    if !resp.status().is_success() {
        return None;
    }
    let data: Value = resp.json().await.ok()?;
    data.get("body")
        .and_then(|v| v.as_str())
        .map(|s| s.to_string())
}

/// 读取收件箱
/// GET /api/emails/{address}，列表项无正文，逐封二拉 body 端点取纯文本正文；
/// 服务端响应无 id 索引时归一化层自动以序号兜底
pub fn get_emails(email: &str, _token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    if em.is_empty() {
        return Err("nullmail 读信: 邮箱地址为空".into());
    }

    let url = format!("{BASE_URL}/api/emails/{}", urlencoding::encode(em));

    block_on(async {
        let resp = set_headers(http_client().get(&url))
            .send()
            .await
            .map_err(|e| format!("nullmail: 读信请求失败: {e}"))?;

        let status = resp.status();
        let raw = resp
            .text()
            .await
            .map_err(|e| format!("nullmail: 读取收件箱响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("nullmail 读信: http {status} {}", raw.trim()));
        }

        let data: Value =
            serde_json::from_str(&raw).map_err(|e| format!("nullmail: 解析收件箱响应失败: {e}"))?;
        let emails_row = data
            .get("emails")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();

        let mut out = Vec::with_capacity(emails_row.len());
        for m in &emails_row {
            let mut flat = m.clone();
            if let Some(obj) = flat.as_object_mut() {
                obj.insert("to".to_string(), json!(em));
                /* 列表只有 id/sender/subject/delivered，delivered 显式映射为 date */
                if let Some(delivered) = m.get("delivered").and_then(|v| v.as_str()) {
                    obj.insert("date".to_string(), json!(delivered));
                }
            }
            /* 正文逐封二拉 body 端点，失败降级留空不阻断列表 */
            if let Some(id) = m.get("id") {
                let id_str = match id {
                    Value::String(s) => s.clone(),
                    other => other.to_string(),
                };
                if let Some(body) = fetch_body(em, &id_str).await {
                    if !body.is_empty() {
                        if let Some(obj) = flat.as_object_mut() {
                            obj.insert("text".to_string(), json!(body));
                        }
                    }
                }
            }
            out.push(normalize_email(&flat, em));
        }
        Ok(out)
    })
}
