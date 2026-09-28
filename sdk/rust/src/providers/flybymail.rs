/*!
 * Flybymail 渠道实现（flybymail.com）
 * POST /api/recipients 建箱（空 JSON body，响应 id/email/createdAt/expiresAt），
 * GET /api/recipients/{email}/emails 读信（按邮箱地址查询，响应 {"emails":[...]}）。
 * 信件字段：id/from/to/subject/body（纯文本）/htmlBody（HTML 正文）/preview/time/read。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};

const BASE_URL: &str = "https://flybymail.com";

/// 创建 flybymail.com 临时邮箱
/// POST /api/recipients（空 JSON body），expiresAt 为毫秒时间戳（约 4 小时）。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let resp = http_client()
            .post(format!("{BASE_URL}/api/recipients"))
            .header("Content-Type", "application/json")
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .body("{}")
            .send()
            .await
            .map_err(|e| format!("flybymail: 创建邮箱请求失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("flybymail: 读取创建响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("flybymail: 创建邮箱失败 http {status}: {body}"));
        }
        let data: Value =
            serde_json::from_str(&body).map_err(|e| format!("flybymail: 解析创建响应失败: {e}"))?;
        let id = data
            .get("id")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();
        let email = data
            .get("email")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();
        if id.is_empty() || email.is_empty() || !email.contains('@') {
            return Err(format!("flybymail: 创建邮箱响应缺少必要字段: {body}"));
        }

        /* expiresAt 为毫秒时间戳，转换为秒供 EmailInfo 统一展示 */
        let expires_at = data
            .get("expiresAt")
            .and_then(|v| v.as_i64())
            .filter(|v| *v > 0)
            .map(|ms| ms / 1000);

        Ok(EmailInfo {
            channel: Channel::Flybymail,
            email,
            token: Some(id),
            expires_at,
            created_at: None,
        })
    })
}

/// 从邮件元素提取候选键的首个非空字符串（数字转十进制字符串）
fn any_string(raw: &Value, key: &str) -> String {
    match raw.get(key) {
        Some(Value::String(s)) => s.trim().to_string(),
        Some(Value::Number(n)) => n.to_string(),
        _ => String::new(),
    }
}

/// 归一邮件时间：候选 time/date 字段（time 为毫秒时间戳），
/// 两字段均缺失时优先插入空串以保证归一化走列表日期候选
fn timestamp_value(raw: &Value) -> Value {
    if let Some(v) = raw.get("time") {
        return v.clone();
    }
    if let Some(v) = raw.get("date") {
        return v.clone();
    }
    Value::Null
}

/// 获取 flybymail.com 邮件列表
/// GET /api/recipients/{email}/emails（按地址查询）返回 {"emails":[...]}。
pub fn get_emails(email: &str, _token: &str) -> Result<Vec<Email>, String> {
    let address = email.trim();
    if address.is_empty() || !address.contains('@') {
        return Err("flybymail: 邮箱地址为空或格式错误".into());
    }

    block_on(async {
        let resp = http_client()
            .get(format!(
                "{BASE_URL}/api/recipients/{}/emails",
                urlencoding::encode(address)
            ))
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .send()
            .await
            .map_err(|e| format!("flybymail: 获取邮件列表请求失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("flybymail: 读取邮件列表响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("flybymail: 获取邮件列表失败 http {status}: {body}"));
        }
        let data: Value =
            serde_json::from_str(&body).map_err(|e| format!("flybymail: 解析邮件列表失败: {e}"))?;
        let emails = data
            .get("emails")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();

        let out: Vec<Email> = emails
            .iter()
            .map(|raw| {
                /* id 为数字时转字符串保持一致，time 为毫秒时间戳时按 timestamp 归一 */
                let mut entry = json!({
                    "from": raw.get("from").cloned().unwrap_or(Value::Null),
                    "to": raw.get("to").cloned().unwrap_or(Value::Null),
                    "subject": raw.get("subject").cloned().unwrap_or(Value::Null),
                    "text": raw.get("body").cloned().unwrap_or(Value::Null),
                    "html": raw.get("htmlBody").cloned().unwrap_or(Value::Null),
                    "time": raw.get("time").cloned().unwrap_or(Value::Null),
                    "read": raw.get("read").cloned().unwrap_or(Value::Null),
                    "attachments": raw.get("attachments").cloned().unwrap_or(Value::Null),
                });
                if let Some(obj) = entry.as_object_mut() {
                    obj.insert("id".to_string(), json!(any_string(raw, "id")));
                    obj.insert("timestamp".to_string(), timestamp_value(raw));
                }
                normalize_email(&entry, address)
            })
            .collect();
        Ok(out)
    })
}
