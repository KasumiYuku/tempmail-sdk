/*!
 * Temporarymail 渠道实现（temporarymail.com）
 * 无认证 REST（key 为空即随机建箱）：
 * - 建箱 GET /api/?action=requestEmailAccess&key=&value=random，
 *   响应 {"address":"...","secretKey":"..."}。
 * - 读信 GET /api/?action=checkInbox&value=<secretKey>，响应有两种形态：
 *   空收件箱为 []，有信时为 map[id]→邮件对象。
 * - 详情 POST /api/?action=getEmail&value=<id>（列表主题常为 "[No Subject]"，
 *   详情覆盖真实主题；失败以列表元数据兜底）。
 * - 全文 GET /view/?i=<id>（平台 HTML 化渲染端点，无风控），
 *   本地剥标签还原纯文本。
 *
 * 地址最长周期固定为 4 小时。
 */

use crate::config::{block_on, get_current_ua, http_client};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use regex::Regex;
use serde_json::Value;
use std::sync::LazyLock;

const BASE_URL: &str = "https://temporarymail.com";
/// 备用浏览器 UA（403 重试用）
const ALT_USER_AGENT: &str = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36";

/* 剥除 HTML 标签（含 script/style 整段） */
static TAG_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"<script[\s\S]*?</script>|<style[\s\S]*?</style>|<[^>]+>").expect("标签正则无效")
});

/* HTML 实体匹配：命名实体与十进制/十六进制数字实体 */
static ENTITY_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"&(?:#([xX][0-9a-fA-F]+)|#([0-9]+)|([a-zA-Z]+));").expect("实体正则无效")
});

/// 构造 /api/ 请求并铺浏览器形态头部集（参照 Go 端固化的最稳组合）
fn api_request(method: &str, api_path: &str, ua: &str) -> wreq::RequestBuilder {
    http_client()
        .request(
            method.parse().unwrap_or(wreq::Method::GET),
            format!("{BASE_URL}{api_path}"),
        )
        .header("Accept", "application/json, text/plain, */*")
        .header("Accept-Language", "en-US,en;q=0.9")
        .header("Sec-Fetch-Site", "same-origin")
        .header("Sec-Fetch-Mode", "cors")
        .header("Sec-Fetch-Dest", "empty")
        .header("Referer", format!("{BASE_URL}/"))
        .header("Origin", BASE_URL)
        .header("User-Agent", ua)
}

/// 反转义 HTML 实体（命名实体 + 数字实体，二次回转常见命名实体）
fn html_unescape_full(s: &str) -> String {
    let decoded = ENTITY_RE.replace_all(s, |caps: &regex::Captures| {
        if let Some(hex_digits) = caps.get(1) {
            if let Ok(code) = u32::from_str_radix(hex_digits.as_str(), 16) {
                if let Some(c) = char::from_u32(code) {
                    return c.to_string();
                }
            }
        } else if let Some(dec_digits) = caps.get(2) {
            if let Ok(code) = dec_digits.as_str().parse::<u32>() {
                if let Some(c) = char::from_u32(code) {
                    return c.to_string();
                }
            }
        } else if let Some(name) = caps.get(3) {
            return match name.as_str() {
                "quot" => "\"".to_string(),
                "apos" => "'".to_string(),
                "amp" => "&".to_string(),
                "lt" => "<".to_string(),
                "gt" => ">".to_string(),
                "nbsp" => " ".to_string(),
                _ => caps[0].to_string(),
            };
        }
        caps[0].to_string()
    });
    decoded.replace("&quot;", "\"").replace("&apos;", "'")
}

/// 将 /view/ 响应剥标签还原为纯文本（<br>/<p> 换行保留）
fn view_to_text(src: &str) -> String {
    let replaced = src
        .replace("<br />", "\n")
        .replace("<br/>", "\n")
        .replace("<br>", "\n")
        .replace("<p>", "\n")
        .replace("</p>", "\n")
        .replace("&nbsp;", " ")
        .replace("&gt;", ">")
        .replace("&lt;", "<")
        .replace("&amp;", "&")
        .replace("&quot;", "\"");
    let stripped = TAG_RE.replace_all(&replaced, " ");
    let unescaped = html_unescape_full(&stripped);
    let lines: Vec<&str> = unescaped.split('\n').map(|line| line.trim()).collect();
    lines.join("\n").trim().to_string()
}

/// 拉取 checkInbox 响应体。
/// 403 时换备用 UA 重试一次；429 返回平台限流错误；其余非 2xx 携带响应体报错。
async fn check_inbox(token: &str) -> Result<String, String> {
    for ua in [get_current_ua(), ALT_USER_AGENT] {
        let resp = api_request(
            "GET",
            &format!(
                "/api/?action=checkInbox&value={}",
                urlencoding::encode(token)
            ),
            ua,
        )
        .send()
        .await
        .map_err(|e| format!("temporarymail: 读取收件箱请求失败: {e}"))?;
        let status = resp.status();
        /* 先取 Retry-After 头，再消费响应体 */
        let retry_after = resp
            .headers()
            .get("Retry-After")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_string();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("temporarymail: 读取收件箱响应失败: {e}"))?;
        if status == 429 {
            return Err(format!(
                "temporarymail: 读取收件箱平台限流(429 Retry-After={retry_after})，请拉大轮询间隔"
            ));
        }
        if status.is_success() {
            return Ok(body);
        }
        /* 403/404 疑似 UA 键控风控，换备用 UA 重试一次 */
        if status != 403 && status != 404 {
            return Err(format!(
                "temporarymail: 读取收件箱失败 http {status}: {body}"
            ));
        }
    }
    Err("temporarymail: 读取收件箱失败 http 403（两次尝试均被拒）".into())
}

/// 拉取单封邮件详情（POST getEmail），失败返回 None（列表元数据兜底）
async fn fetch_detail(id: &str) -> Option<(String, String)> {
    let resp = api_request(
        "POST",
        &format!("/api/?action=getEmail&value={}", urlencoding::encode(id)),
        get_current_ua(),
    )
    .send()
    .await
    .ok()?;
    let status = resp.status();
    if status == 429 || !status.is_success() {
        return None;
    }
    let data = resp.json::<Value>().await.ok()?;
    /* 响应为 {id: {...}} 单元素对象，取第一个元素 */
    data.as_object()?.values().next().map(|value| {
        (
            value
                .get("subject")
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string(),
            value
                .get("from")
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string(),
        )
    })
}

/// 抓取 /view/ 渲染端点全文并剥标签还原纯文本
async fn fetch_view_text(id: &str) -> Option<String> {
    let resp = http_client()
        .get(format!(
            "{BASE_URL}/view/?i={}&width=800",
            urlencoding::encode(id)
        ))
        .header("Accept", "text/html, */*")
        .header("User-Agent", get_current_ua())
        .header("Referer", format!("{BASE_URL}/"))
        .send()
        .await
        .ok()?;
    if !resp.status().is_success() {
        return None;
    }
    let raw = resp.text().await.ok()?;
    let text = view_to_text(&raw);
    if text.is_empty() {
        None
    } else {
        Some(text)
    }
}

/// 创建 temporarymail.com 临时邮箱
/// GET /api/?action=requestEmailAccess&key=&value=random，
/// key 为空时服务端随机分配地址；token 复用 secretKey。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let resp = api_request(
            "GET",
            "/api/?action=requestEmailAccess&key=&value=random",
            get_current_ua(),
        )
        .send()
        .await
        .map_err(|e| format!("temporarymail: 创建邮箱请求失败: {e}"))?;
        let status = resp.status();
        /* 先取 Retry-After 头，再消费响应体 */
        let retry_after = resp
            .headers()
            .get("Retry-After")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("")
            .to_string();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("temporarymail: 读取创建响应失败: {e}"))?;
        if !status.is_success() {
            if status == 429 {
                return Err(format!(
                    "temporarymail: 创建邮箱平台限流(429 Retry-After={retry_after})，请稍后重试: {body}"
                ));
            }
            return Err(format!("temporarymail: 创建邮箱失败 http {status}: {body}"));
        }
        let data: Value = serde_json::from_str(&body)
            .map_err(|e| format!("temporarymail: 解析创建响应失败: {e}"))?;
        let address = data
            .get("address")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        let secret_key = data
            .get("secretKey")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();
        if address.is_empty() || secret_key.is_empty() {
            return Err(format!(
                "temporarymail: 创建响应缺少 address 或 secretKey: {body}"
            ));
        }

        Ok(EmailInfo {
            channel: Channel::TemporarymailCom,
            email: address,
            token: Some(secret_key),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 任意 JSON 值规整为字符串（nil→空串）
fn value_as_str(value: &Value) -> String {
    match value {
        Value::String(s) => s.clone(),
        Value::Number(n) => n.to_string(),
        Value::Bool(b) => b.to_string(),
        _ => String::new(),
    }
}

/// 读取 temporarymail 收件箱
/// 响应为邮件数组（空时为 []）或 map[id]→对象（有信时）。
/// 列表主题常为 "[No Subject]"：逐封拉详情覆盖真实主题，
/// 并逐封从 /view/ 渲染端点抓取全文。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    let secret_key = token.trim();

    block_on(async {
        let body = check_inbox(secret_key).await?;

        /* 平台响应两种合法形态：空箱 []，有信为 map[id]→对象 */
        let parsed: Value = serde_json::from_str(&body).map_err(|e| {
            format!("temporarymail: 解析收件箱响应失败（secretKey 可能已失效）: {e}")
        })?;
        let mut list: Vec<Value> = Vec::new();
        match &parsed {
            Value::Array(items) => list = items.clone(),
            Value::Object(obj) => list = obj.values().cloned().collect(),
            _ => {}
        }

        let mut out = Vec::with_capacity(list.len());
        for m in &list {
            let mut flat = m.clone();
            /* 列表元素无 to 字段，注入收件人地址以归一化 */
            if flat.get("to").is_none() {
                if let Some(obj) = flat.as_object_mut() {
                    obj.insert("to".to_string(), serde_json::json!(em));
                }
            }
            let id = m.get("id").map(value_as_str).unwrap_or_default();
            if !id.is_empty() {
                /* 详情覆盖真实主题（失败不致命：列表元数据兜底） */
                if let Some((subject, from)) = fetch_detail(&id).await {
                    if !subject.is_empty() {
                        if let Some(obj) = flat.as_object_mut() {
                            obj.insert("subject".to_string(), serde_json::json!(subject));
                        }
                    }
                    if !from.is_empty() {
                        if let Some(obj) = flat.as_object_mut() {
                            obj.insert("from".to_string(), serde_json::json!(from));
                        }
                    }
                }
                /* /view/ 渲染端点全文（失败不致命：列表元数据兜底） */
                if let Some(text) = fetch_view_text(&id).await {
                    if let Some(obj) = flat.as_object_mut() {
                        obj.insert("text".to_string(), serde_json::json!(text));
                    }
                }
            }
            out.push(normalize_email(&flat, em));
        }
        Ok(out)
    })
}
