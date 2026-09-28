/*!
 * 30minemail 渠道实现（30minemail.com）
 * 官网邮箱由服务端 16 位 hex 本地名识别：
 * - 建箱 GET /?generate 返回完整 HTML 页面（含 <地址>@30minemail.com），
 *   从其中解析邮箱地址。
 * - 读信 GET /messages.php?email=<完整地址>&_=<unix毫秒>，
 *   响应 {"ok":true,"expired":false,"count":0,"emails":[...],...}，
 *   emails 元素字段实测：{id,from,to,subject,date,html}。
 *
 * 无认证、无 Cookie、无 CSRF。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};

const BASE_URL: &str = "https://30minemail.com";
const DOMAIN: &str = "30minemail.com";

/// 创建 30minemail.com 临时邮箱
/// GET /?generate 返回 HTML 页面，从其中解析 16 位 hex 本地名地址；
/// token 复用完整地址（服务端以地址定位收件箱）。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let resp = http_client()
            .get(format!("{BASE_URL}/?generate"))
            .header(
                "Accept",
                "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
            )
            .header("User-Agent", get_current_ua())
            .send()
            .await
            .map_err(|e| format!("30minemail: 创建邮箱请求失败: {e}"))?;
        let status = resp.status();
        if !status.is_success() {
            return Err(format!("30minemail: 创建邮箱失败 http {status}"));
        }
        let page = resp
            .text()
            .await
            .map_err(|e| format!("30minemail: 读取创建页面失败: {e}"))?;

        let needle = format!("@{DOMAIN}");
        let idx = match page.find(&needle) {
            Some(i) => i,
            None => return Err("30minemail: 创建页面未找到邮箱地址".into()),
        };
        /* 向前查找本地名起点：空白或 > 之后 */
        let mut start = idx;
        while start > 0 {
            let c = page.as_bytes()[start - 1] as char;
            if c == ' ' || c == '\n' || c == '\t' || c == '>' || c == '"' {
                break;
            }
            start -= 1;
        }
        let local = page[start..idx].trim().to_string();
        if local.len() < 8 {
            return Err(format!(
                "30minemail: 创建页面解析地址异常: {}",
                &page[start..idx + DOMAIN.len() + 1]
            ));
        }
        let address = format!("{local}@{DOMAIN}");

        Ok(EmailInfo {
            channel: Channel::Email30Min,
            email: address.clone(),
            token: Some(address),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 读取 30minemail.com 收件箱
/// GET /messages.php?email=<完整地址>&_=<unix毫秒>，模拟官方轮询参数。
pub fn get_emails(email: &str, _token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    if em.is_empty() {
        return Err("30minemail: 邮箱地址为空".into());
    }

    block_on(async {
        let now_ms = chrono::Utc::now().timestamp_millis();
        let resp = http_client()
            .get(format!(
                "{BASE_URL}/messages.php?email={}&_={now_ms}",
                urlencoding::encode(em)
            ))
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .send()
            .await
            .map_err(|e| format!("30minemail: 读取收件箱请求失败: {e}"))?;
        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("30minemail: 读取收件箱响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("30minemail: 读取收件箱失败 http {status}: {body}"));
        }
        let data: Value = serde_json::from_str(&body)
            .map_err(|e| format!("30minemail: 解析收件箱响应失败: {e}"))?;
        if data.get("ok").and_then(|v| v.as_bool()) != Some(true)
            || data.get("expired").and_then(|v| v.as_bool()) == Some(true)
        {
            return Err(format!("30minemail: 收件箱不可用或已过期: {body}"));
        }
        let emails = data
            .get("emails")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();

        let out: Vec<Email> = emails
            .iter()
            .map(|m| {
                let mut flat = m.clone();
                /* 列表元素无 to 字段时注入收件人地址 */
                if flat.get("to").is_none() {
                    if let Some(obj) = flat.as_object_mut() {
                        obj.insert("to".to_string(), json!(em));
                    }
                }
                normalize_email(&flat, em)
            })
            .collect();
        Ok(out)
    })
}
