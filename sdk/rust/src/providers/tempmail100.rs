/*!
 * TempMail100 渠道实现（tempmail100.com）
 * POST /init 建箱初始化（空 body，响应 code/data.token（JWT），无 Cookie），
 * POST /web/generate 创建随机地址（Header Authorization: <token>，响应 code/data.address），
 * GET /web/emails 读信列表（Header Authorization: <token>，
 *   响应 code/data.list[]/data.total）。
 *
 * 平台限制：列表元素 content 恒为空字符串（详情端点不存在，为平台限制），
 * 本渠道客观为「列表-only」：subject/fromAddress/fromName/timestamp/read 正确输出，
 * 正文如实留空。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use serde_json::{json, Value};

const BASE_URL: &str = "https://tempmail100.com";

/// 接口字段值安全转换为字符串（非字符串数字转十进制字符串，其余返回空串）
fn str_of(value: &Value) -> String {
    match value {
        Value::String(s) => s.clone(),
        Value::Number(n) => n.to_string(),
        _ => String::new(),
    }
}

/// read 字段归一为布尔已读标记（兼容 bool/number/string）
fn read_of(value: &Value) -> bool {
    match value {
        Value::Bool(b) => *b,
        Value::Number(n) => n.as_f64().map(|f| f != 0.0).unwrap_or(false),
        Value::String(s) => {
            let trimmed = s.trim();
            trimmed.eq_ignore_ascii_case("true") || trimmed == "1"
        }
        _ => false,
    }
}

/// 创建 tempmail100.com 临时邮箱
/// 流程：POST /init 取得 JWT token，再 POST /web/generate 创建地址。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        /* 第一步：初始化取得 token */
        let resp = http_client()
            .post(format!("{BASE_URL}/init"))
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .send()
            .await
            .map_err(|e| format!("tempmail100: 初始化请求失败: {e}"))?;
        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("tempmail100: 读取初始化响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("tempmail100: 初始化失败 http {status}: {body}"));
        }
        let init: Value = serde_json::from_str(&body)
            .map_err(|e| format!("tempmail100: 解析初始化响应失败: {e}"))?;
        let token = init
            .get("data")
            .and_then(|v| v.get("token"))
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if init.get("code").and_then(|v| v.as_i64()) != Some(0) || token.is_empty() {
            return Err(format!("tempmail100: 初始化响应异常: {body}"));
        }

        /* 第二步：创建随机地址（前端使用 Authorization: <token> 不带 Bearer） */
        let resp = http_client()
            .post(format!("{BASE_URL}/web/generate"))
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .header("Authorization", &token)
            .send()
            .await
            .map_err(|e| format!("tempmail100: 创建地址请求失败: {e}"))?;
        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("tempmail100: 读取创建地址响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("tempmail100: 创建地址失败 http {status}: {body}"));
        }
        let gen: Value = serde_json::from_str(&body)
            .map_err(|e| format!("tempmail100: 解析创建地址响应失败: {e}"))?;
        let address = gen
            .get("data")
            .and_then(|v| v.get("address"))
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if gen.get("code").and_then(|v| v.as_i64()) != Some(0)
            || address.is_empty()
            || !address.contains('@')
        {
            return Err(format!("tempmail100: 创建地址响应异常: {body}"));
        }

        Ok(EmailInfo {
            channel: Channel::Tempmail100,
            email: address,
            token: Some(token),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 归一化 /web/emails 列表元素（content 平台恒空，如实留空）
fn normalize_item(raw: &Value, email: &str) -> Email {
    let mut from_address = str_of(raw.get("fromAddress").unwrap_or(&Value::Null));
    let from_name = str_of(raw.get("fromName").unwrap_or(&Value::Null));
    /* fromName+fromAddress 组合为 "Name <address>" 填入 from */
    if !from_name.is_empty()
        && !from_address.is_empty()
        && !from_name.eq_ignore_ascii_case(&from_address)
        && from_address.contains('@')
    {
        from_address = format!("{from_name} <{from_address}>");
    }

    let flat = json!({
        "id": str_of(raw.get("uuid").unwrap_or(&Value::Null)),
        "from": from_address,
        "to": str_of(raw.get("toAddress").unwrap_or(&Value::Null)),
        "subject": str_of(raw.get("subject").unwrap_or(&Value::Null)),
        "content": str_of(raw.get("content").unwrap_or(&Value::Null)),
        "timestamp": raw.get("timestamp").cloned().unwrap_or(Value::Null),
        "isRead": read_of(raw.get("read").unwrap_or(&Value::Null)),
    });
    normalize_email(&flat, email)
}

/// 获取 tempmail100.com 邮件列表
/// GET /web/emails（Authorization 头为裸 token）返回 code/data.list/data.total。
/// 列表元素 content 恒为空（平台限制），如实输出空正文。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    let tok = token.trim();

    block_on(async {
        let resp = http_client()
            .get(format!("{BASE_URL}/web/emails"))
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .header("Authorization", tok)
            .send()
            .await
            .map_err(|e| format!("tempmail100: 获取邮件列表请求失败: {e}"))?;
        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("tempmail100: 读取邮件列表响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!(
                "tempmail100: 获取邮件列表失败 http {status}: {body}"
            ));
        }
        let data: Value = serde_json::from_str(&body)
            .map_err(|e| format!("tempmail100: 解析邮件列表失败: {e}"))?;
        if data.get("code").and_then(|v| v.as_i64()) != Some(0) {
            let message = data.get("message").and_then(|v| v.as_str()).unwrap_or("");
            return Err(format!("tempmail100: 获取邮件列表响应异常: {message}"));
        }
        let list = data
            .get("data")
            .and_then(|v| v.get("list"))
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();

        Ok(list.iter().map(|m| normalize_item(m, em)).collect())
    })
}
