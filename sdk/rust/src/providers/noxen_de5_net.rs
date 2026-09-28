/*!
 * NoxenDe5Net 渠道实现（UniMail-Bot 公共实例 tempmail.noxen.de5.net）
 * 契约（curl 实测 + 上游源码 review，与 Go 端一致）：
 * - 登录：POST /api/login {"username":"guest","password":"123456"}
 *   → 200 并 Set-Cookie: iding-session=<JWT>（会话校验 GET /api/session）。
 * - 建箱：GET /api/generate → {"email","expires"（毫秒时间戳）}。
 * - 读信：GET /api/emails?mailbox=<地址>&limit=20（需会话 Cookie），
 *   详情 GET /api/email/{id}（content/html_content 平台恒为空），
 *   全文 GET /api/email/{id}/download 返回原始 EML 报文，
 *   本地拆分 text/plain 与 text/html；全文不可得时以
 *   verification_code + preview 合成占位正文。
 *
 * 会话隔离：会话 Cookie 从登录响应显式提取，读信时逐请求显式携带，
 *   避免依赖全局 Cookie 罐（与 Go 端 HTTPClientNoCookieJar 策略一致）。
 */

use crate::config::{block_on, get_current_ua, http_client_no_cookie_jar};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use base64::Engine;
use serde_json::{json, Value};
use wreq::Client;

const BASE_URL: &str = "https://tempmail.noxen.de5.net";
const LOGIN_USER: &str = "guest";
const LOGIN_PASS: &str = "123456";
/// 凭据串前缀（"基址|cookie=值"）
const TOKEN_PREFIX: &str = "noxen-de5-net|";

/// 邮件详情字段（与上游 /api/email/{id} 对齐）
struct DetailFields {
    content: String,
    html_content: String,
    to_addrs: String,
    download: String,
}

/// 登录取得会话 Cookie（iding-session=JWT）
async fn session_cookie(client: &Client) -> Result<String, String> {
    let resp = client
        .post(format!("{BASE_URL}/api/login"))
        .header("Content-Type", "application/json")
        .header("Accept", "application/json")
        .header("User-Agent", get_current_ua())
        .body(json!({"username": LOGIN_USER, "password": LOGIN_PASS}).to_string())
        .send()
        .await
        .map_err(|e| format!("noxen-de5-net login: 请求失败: {e}"))?;

    /* 先收集头部与会话 Cookie，再消费响应体（text 会移动响应） */
    let status = resp.status();
    let cookies: Vec<(String, String)> = resp
        .cookies()
        .map(|c| (c.name().to_string(), c.value().to_string()))
        .collect();
    let body = resp
        .text()
        .await
        .map_err(|e| format!("noxen-de5-net login: 读取响应失败: {e}"))?;
    if !status.is_success() {
        return Err(format!("noxen-de5-net login: http {status}"));
    }
    let data: Value = serde_json::from_str(&body)
        .map_err(|e| format!("noxen-de5-net login: 解析响应失败: {e}"))?;
    if data.get("success").and_then(|v| v.as_bool()) != Some(true) {
        return Err("noxen-de5-net login: 登录失败".into());
    }
    for (name, value) in cookies {
        if name == "iding-session" {
            return Ok(format!("iding-session={value}"));
        }
    }
    Err("noxen-de5-net login: 未下发会话 Cookie".into())
}

/// 校验会话 Cookie 是否仍有效（GET /api/session）
async fn cookie_still_valid(client: &Client, cookie: &str) -> bool {
    match client
        .get(format!("{BASE_URL}/api/session"))
        .header("Accept", "application/json")
        .header("User-Agent", get_current_ua())
        .header("Cookie", cookie)
        .send()
        .await
    {
        Ok(resp) if resp.status().is_success() => match resp.json::<Value>().await {
            Ok(data) => data
                .get("authenticated")
                .and_then(|v| v.as_bool())
                .unwrap_or(false),
            Err(_) => false,
        },
        _ => false,
    }
}

/// 创建 noxen-de5-net 临时邮箱
/// token 凭据串格式："noxen-de5-net|iding-session=<JWT>|base=<基址>"
pub fn generate_email() -> Result<EmailInfo, String> {
    let client = http_client_no_cookie_jar();

    block_on(async {
        let session = session_cookie(&client).await?;

        /* 建箱：GET /api/generate（访客会话，显式 Cookie） */
        let resp = client
            .get(format!("{BASE_URL}/api/generate"))
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .header("Cookie", &session)
            .send()
            .await
            .map_err(|e| format!("noxen-de5-net generate: 请求失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("noxen-de5-net generate: 读取响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!("noxen-de5-net generate: http {status}"));
        }
        let data: Value = serde_json::from_str(&body)
            .map_err(|e| format!("noxen-de5-net generate: 解析响应失败: {e}"))?;
        let email = data
            .get("email")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if email.is_empty() {
            return Err("noxen-de5-net generate: 响应缺少 email".into());
        }

        let token = format!(
            "{TOKEN_PREFIX}{}|base={BASE_URL}",
            urlencoding::encode(&session)
        );
        let expires_at = data
            .get("expires")
            .and_then(|v| v.as_i64())
            .filter(|v| *v > 0);

        Ok(EmailInfo {
            channel: Channel::NoxenDe5Net,
            email,
            token: Some(token),
            expires_at,
            created_at: None,
        })
    })
}

/// 拉取单封邮件详情
async fn fetch_detail(client: &Client, cookie: &str, id: &str) -> Result<DetailFields, String> {
    let resp = client
        .get(format!("{BASE_URL}/api/email/{}", urlencoding::encode(id)))
        .header("Accept", "application/json")
        .header("User-Agent", get_current_ua())
        .header("Cookie", cookie)
        .send()
        .await
        .map_err(|e| format!("noxen-de5-net detail: 请求失败: {e}"))?;
    let status = resp.status();
    if !status.is_success() {
        return Err(format!("noxen-de5-net detail: http {status}"));
    }
    let data: Value = resp
        .json()
        .await
        .map_err(|e| format!("noxen-de5-net detail: 解析失败: {e}"))?;
    Ok(DetailFields {
        content: value_str(&data, "content"),
        html_content: value_str(&data, "html_content"),
        to_addrs: value_str(&data, "to_addrs"),
        download: value_str(&data, "download"),
    })
}

/// 从 JSON Value 提取候选键的首个非空字符串
fn value_str(data: &Value, key: &str) -> String {
    data.get(key)
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string()
}

/// 拉取原始 EML 报文（详情 download 字段指向的下载端点）
async fn fetch_eml(client: &Client, cookie: &str, download: &str) -> Option<Vec<u8>> {
    let url = if download.starts_with("http://") || download.starts_with("https://") {
        download.to_string()
    } else {
        format!("{BASE_URL}{download}")
    };
    let resp = client
        .get(url)
        .header("Accept", "message/rfc822, */*")
        .header("User-Agent", get_current_ua())
        .header("Cookie", cookie)
        .send()
        .await
        .ok()?;
    if !resp.status().is_success() {
        return None;
    }
    resp.bytes().await.ok().map(|b| b.to_vec())
}

/// 读取 noxen-de5-net 收件箱
/// 列表只含 preview；正文获取优先级：
/// 1) 详情 + 下载端点拉取原始 EML，本地拆分 text/html；
/// 2) 全文不可得时用 verification_code + preview 合成占位正文。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    if !token.starts_with(TOKEN_PREFIX) {
        return Err("noxen-de5-net: token 格式错误".into());
    }
    let raw_cred = token.strip_prefix(TOKEN_PREFIX).unwrap_or_default();
    let cookie = match urlencoding::decode(raw_cred) {
        Ok(decoded) => decoded.split('|').next().unwrap_or("").to_string(),
        Err(_) => return Err("noxen-de5-net: token 解码失败".into()),
    };
    let client = http_client_no_cookie_jar();

    block_on(async {
        let mut cookie = cookie;
        if !cookie_still_valid(&client, &cookie).await {
            cookie = session_cookie(&client).await?;
        }

        /* 列表：GET /api/emails?mailbox=<地址>&limit=20 */
        let resp = client
            .get(format!(
                "{BASE_URL}/api/emails?mailbox={}&limit=20",
                urlencoding::encode(em)
            ))
            .header("Accept", "application/json")
            .header("User-Agent", get_current_ua())
            .header("Cookie", &cookie)
            .send()
            .await
            .map_err(|e| format!("noxen-de5-net 读信: 请求失败: {e}"))?;

        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("noxen-de5-net 读信: 读取响应失败: {e}"))?;
        if status == 401 {
            return Err("noxen-de5-net 读信: http 401（会话失效或非本会话邮箱）".into());
        }
        if !status.is_success() {
            return Err(format!("noxen-de5-net 读信: http {status}"));
        }
        let raw: Value = serde_json::from_str(&body)
            .map_err(|e| format!("noxen-de5-net 读信: 解析失败: {e}"))?;
        let list = raw
            .as_array()
            .cloned()
            .ok_or_else(|| "noxen-de5-net 读信: 响应非数组".to_string())?;

        let mut out = Vec::with_capacity(list.len());
        for m in &list {
            let id = m.get("id").map(|v| v.to_string()).unwrap_or_default();
            let preview = m
                .get("preview")
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string();
            let mut flat = m.clone();
            if let Some(obj) = flat.as_object_mut() {
                obj.insert(
                    "from".to_string(),
                    m.get("sender").cloned().unwrap_or(Value::Null),
                );
                obj.insert("to".to_string(), json!(em));
                obj.insert(
                    "date".to_string(),
                    m.get("received_at").cloned().unwrap_or(Value::Null),
                );
                obj.insert("text".to_string(), json!(preview));
                obj.insert(
                    "isRead".to_string(),
                    m.get("is_read").cloned().unwrap_or(Value::Null),
                );
            }
            let mut full = false;
            if !id.is_empty() && id != "0" {
                /* 详情 + 下载端点拉取原始 EML（失败按占位正文兜底） */
                if let Ok(detail) = fetch_detail(&client, &cookie, &id).await {
                    if let Some(obj) = flat.as_object_mut() {
                        obj.insert("content".to_string(), json!(detail.content));
                        obj.insert("html_content".to_string(), json!(detail.html_content));
                        obj.insert("to_addrs".to_string(), json!(detail.to_addrs));
                    }
                    if !detail.download.is_empty() {
                        if let Some(eml) = fetch_eml(&client, &cookie, &detail.download).await {
                            let (text, html) = parse_eml(&eml);
                            if !text.is_empty() || !html.is_empty() {
                                if let Some(obj) = flat.as_object_mut() {
                                    obj.insert("text".to_string(), json!(text));
                                    obj.insert("html_content".to_string(), json!(html));
                                }
                                full = true;
                            }
                        }
                    }
                }
                if !full {
                    let placeholder = compose_placeholder(m);
                    if let Some(obj) = flat.as_object_mut() {
                        obj.insert("text".to_string(), json!(placeholder));
                    }
                }
            }
            out.push(normalize_email(&flat, em));
        }
        Ok(out)
    })
}

/// 全文不可得时的合成占位正文：verification_code 置顶，preview 附后
fn compose_placeholder(m: &Value) -> String {
    let code = m
        .get("verification_code")
        .filter(|v| !v.is_null())
        .map(|v| match v {
            Value::String(s) => s.trim().to_string(),
            other => other.to_string(),
        })
        .unwrap_or_default();
    let preview = m
        .get("preview")
        .filter(|v| !v.is_null())
        .map(|v| match v {
            Value::String(s) => s.trim().to_string(),
            other => other.to_string(),
        })
        .unwrap_or_default();
    let mut parts: Vec<String> = Vec::new();
    if !code.is_empty() {
        parts.push(format!("验证码: {code}"));
    }
    if !preview.is_empty() {
        parts.push(preview);
    }
    parts.join("\n\n")
}

/// 从 Content-Type 头值提取 multipart boundary（引号可选）
fn boundary_of(content_type: &str) -> String {
    let re = regex::Regex::new(r#"(?i)boundary="?([^";\s]+)"?"#).expect("boundary 正则");
    re.captures(content_type)
        .and_then(|c| c.get(1))
        .map(|m| m.as_str().to_string())
        .unwrap_or_default()
}

/// 将原始报文切分为首部 map 与正文块（CRLF 已归一为 LF）
fn split_eml(payload: &str, offset: usize) -> (std::collections::HashMap<String, String>, String) {
    let lines: Vec<&str> = payload.split('\n').collect();
    let mut headers = std::collections::HashMap::new();
    let mut i = offset;
    if i < lines.len() && lines[i].starts_with("From ") {
        i += 1;
    }
    let mut current_key = String::new();
    while i < lines.len() {
        let line = lines[i];
        if line.is_empty() {
            i += 1;
            break;
        }
        /* 折行续行（RFC 5322）：以空白开头且已有当前头键时拼接到上一条 */
        if (line.starts_with(' ') || line.starts_with('\t')) && !current_key.is_empty() {
            if let Some(existing) = headers.get(&current_key) {
                let merged = format!("{existing} {}", line.trim());
                headers.insert(current_key.clone(), merged);
            }
            i += 1;
            continue;
        }
        if let Some(colon) = line.find(':') {
            if colon > 0 {
                current_key = line[..colon].trim().to_lowercase();
                headers.insert(current_key.clone(), line[colon + 1..].trim().to_string());
            }
        }
        i += 1;
    }
    if i > lines.len() {
        i = lines.len();
    }
    (headers, lines[i..].join("\n"))
}

/// 按 boundary 切出各 part（已剥离边界标记与首部空行）
fn split_multipart(body: &str, boundary: &str) -> Vec<String> {
    let mut parts = Vec::new();
    for segment in body.split(&format!("--{boundary}")) {
        let mut seg = segment.trim_start_matches('\n').to_string();
        seg = seg
            .trim_end_matches("--\n")
            .trim_end_matches("--")
            .to_string();
        if !seg.trim().is_empty() {
            parts.push(seg);
        }
    }
    parts
}

/// 按 Content-Transfer-Encoding 解码 part 内容
fn decode_part(data: &str, encoding: &str) -> String {
    match encoding.trim().to_lowercase().as_str() {
        "base64" => {
            let joined: String = data.chars().filter(|c| !c.is_whitespace()).collect();
            match base64::engine::general_purpose::STANDARD.decode(joined) {
                Ok(bytes) => String::from_utf8_lossy(&bytes).trim().to_string(),
                Err(_) => data.to_string(),
            }
        }
        "quoted-printable" => {
            /* 软换行先合并，再解 =XX 十六进制转义 */
            let merged = data.replace("=\r\n", "").replace("=\n", "");
            let re = regex::Regex::new(r"=([0-9A-Fa-f]{2})").expect("qp 正则");
            re.replace_all(&merged, |caps: &regex::Captures| {
                let byte = u8::from_str_radix(&caps[1], 16).unwrap_or(b'?');
                (byte as char).to_string()
            })
            .to_string()
        }
        /* 7bit/8bit/binary：原样返回 */
        _ => data.trim().to_string(),
    }
}

/// 从整体原文抓取 <html>…</html> 片段（HTML 未正确声明时兜底）
fn guess_html(body: &str) -> String {
    if body.is_empty() {
        return String::new();
    }
    let lower = body.to_lowercase();
    let mut start = lower.find("<html");
    if start.is_none() {
        start = lower.find("<!doctype html");
    }
    let start = match start {
        Some(s) => s,
        None => return String::new(),
    };
    let end = match lower.rfind("</html>") {
        Some(e) if e >= start => e,
        _ => return String::new(),
    };
    body[start..end + 7].to_string()
}

/// 递归解析单个 MIME 实体 → (纯文本正文, HTML 正文)
fn parse_entity(
    headers: &std::collections::HashMap<String, String>,
    body: &str,
) -> (String, String) {
    let content_type_raw = headers.get("content-type").cloned().unwrap_or_default();
    let content_type = content_type_raw.to_lowercase();
    let encoding = headers
        .get("content-transfer-encoding")
        .cloned()
        .unwrap_or_default()
        .to_lowercase();

    /* 单体：text/html 或 text/plain（无 Content-Type 时按纯文本处理） */
    if !content_type.starts_with("multipart/") {
        let decoded = decode_part(body, &encoding);
        if content_type.contains("text/html") {
            return (String::new(), decoded);
        }
        return (decoded, String::new());
    }

    /* 复合：递归拆分，text 槽与 html 槽各自取第一个非空命中 */
    let mut text = String::new();
    let mut html = String::new();
    let boundary = boundary_of(&content_type_raw);
    if !boundary.is_empty() {
        for part in split_multipart(body, &boundary) {
            let (part_headers, part_body) = split_eml(&format!("#participant\n{part}"), 1);
            let part_type = part_headers
                .get("content-type")
                .cloned()
                .unwrap_or_default()
                .to_lowercase();
            if part_type.starts_with("multipart/") {
                let (t, h) = parse_entity(&part_headers, &part_body);
                if text.is_empty() {
                    text = t;
                }
                if html.is_empty() {
                    html = h;
                }
            } else if part_type.starts_with("message/rfc822") {
                /* 转发的原始邮件整体作为 part：递归整封解析 */
                let (nested_headers, nested_body) = split_eml(&part_body, 0);
                let (t, h) = parse_entity(&nested_headers, &nested_body);
                if text.is_empty() {
                    text = t;
                }
                if html.is_empty() {
                    html = h;
                }
            } else if part_type.contains("rfc822-headers") {
                /* 纯头部 part 跳过，正文在后续 part 中抓取 */
                continue;
            } else {
                let (t, h) = parse_entity(&part_headers, &part_body);
                if text.is_empty() {
                    text = t;
                }
                if html.is_empty() {
                    html = h;
                }
            }
            if !text.is_empty() && !html.is_empty() {
                break;
            }
        }
    }
    /* 无 HTML 命中时从整体原文兜底抓取 HTML 片段 */
    if html.is_empty() {
        html = guess_html(body);
    }
    (text, html)
}

/// 解析 EML 原始报文 → (纯文本正文, HTML 正文)
fn parse_eml(raw: &[u8]) -> (String, String) {
    let payload = String::from_utf8_lossy(raw)
        .replace("\r\n", "\n")
        .replace('\r', "");
    let (headers, body) = split_eml(&payload, 0);
    parse_entity(&headers, &body)
}
