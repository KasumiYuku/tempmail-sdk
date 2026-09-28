/*!
 * TempmailEE 渠道实现（tempmail.ee）
 *
 * 平台读信端 /api/mails 的 403 是「会话 Cookie 绑定校验」：
 * change（换箱）返回的 Set-Cookie 中 temp_mail_session 与 temp_email
 * 共同构成读信凭据，带齐即 200。因此必须在同一个事务内完成
 * change → 提取 Set-Cookie → 读信，凭据以显式 Cookie 请求头逐请求携带。
 *
 * 会话隔离：建箱与读信全部使用 http_client_no_cookie_jar（无 Cookie 罐），
 * 杜绝与全局 Cookie 罐残留会话串池、也避免并发相互污染。
 *
 * 已验证通行配方（对应 Go 端 fhttp tls-client Chrome 指纹实测 200）：
 *   - POST /api/mailbox/change 必须带 sec-ch-ua 三件套头（否则 403 Browser request required）；
 *   - 读信 POST /api/mails 带完整 sec-ch-ua + 正确会话 Cookie；
 *   - 列表只含元数据（id/fromAddress/toAddress/subject/createdAt/isRead），
 *     正文须逐封 GET /api/mails/{id}，响应 content 为 HTML 实体转义后的
 *     MIME multipart 原文，需解码拆出 text / html。
 */

use crate::config::{block_on, http_client_no_cookie_jar};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use base64::{engine::general_purpose::STANDARD, Engine as _};
use regex::Regex;
use serde_json::{json, Value};
use std::collections::HashMap;
use std::sync::LazyLock;

const BASE_URL: &str = "https://tempmail.ee";

/* 与 sec-ch-ua 品牌版本一致的固定 UA（Chrome 154 / Linux） */
const EE_UA: &str = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

/* Token 前缀，用于识别本渠道会话凭据串 */
const TOKEN_PREFIX: &str = "tempmail-ee|";

/* HTML 实体匹配：命名实体与十进制/十六进制数字实体 */
static ENTITY_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"&(?:#([xX][0-9a-fA-F]+)|#([0-9]+)|([a-zA-Z]+));").expect("实体正则无效")
});

/// 建箱提交的浏览器指纹（与官方前端一致）
fn browser_integrity() -> Value {
    json!({
        "webdriver": false, "languagesMissing": false, "languageMissing": false,
        "pluginsMissing": false, "pluginsUndefined": false, "outerSizeMissing": false,
        "innerSizeMissing": false, "screenMissing": false, "screenDepthMissing": false,
        "timezoneMissing": false, "timezoneOffsetMissing": false,
        "userAgentDataPresent": true, "userAgentMissing": false, "platformClass": "Linux",
        "mobile": false, "collectionFailed": false,
    })
}

/* 设置浏览器特征安全头（同站 fetch 全套） */
fn set_browser_headers(req: wreq::RequestBuilder, with_sec_ch: bool) -> wreq::RequestBuilder {
    let mut req = req
        .header("Accept", "application/json")
        .header("Content-Type", "application/json")
        .header("X-Requested-With", "XMLHttpRequest")
        .header("Origin", BASE_URL)
        .header("Referer", format!("{BASE_URL}/"))
        .header("Sec-Fetch-Site", "same-origin")
        .header("Sec-Fetch-Mode", "cors")
        .header("Sec-Fetch-Dest", "empty")
        .header("Accept-Language", "en-US,en;q=0.9")
        .header("User-Agent", EE_UA);
    if with_sec_ch {
        req = req
            .header(
                "Sec-Ch-Ua",
                "\"Chromium\";v=\"154\", \"Google Chrome\";v=\"154\", \"Not.A/Brand\";v=\"99\"",
            )
            .header("Sec-Ch-Ua-Mobile", "?0")
            .header("Sec-Ch-Ua-Platform", "\"Linux\"");
    }
    req
}

/* 从 change 响应提取会话 Cookie 键值对（无 Cookie 罐模式下手动接管会话） */
fn cookie_from_response(resp: &wreq::Response) -> (String, String) {
    let mut email = String::new();
    let mut session = String::new();
    for hv in resp.headers().get_all("set-cookie") {
        if let Ok(s) = hv.to_str() {
            /* 仅取第一个分号前的键值对 */
            if let Some(kv) = s.split(';').next() {
                if let Some(v) = kv.strip_prefix("temp_email=") {
                    email = v.to_string();
                } else if let Some(v) = kv.strip_prefix("temp_mail_session=") {
                    session = v.to_string();
                }
            }
        }
    }
    (email, session)
}

/* 由邮箱与 temp_mail_session 组装渠道内部凭据串 */
fn build_token(email: &str, session: &str) -> String {
    format!("{TOKEN_PREFIX}temp_email={email}; temp_mail_session={session}")
}

/*
 * 解析读信凭据：防御 token 与 email 不一致，以请求邮箱为准重拼 cookie
 */
fn parse_token(token: &str, email: &str) -> Result<String, String> {
    let cred = token
        .strip_prefix(TOKEN_PREFIX)
        .ok_or("tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱")?;
    let mut session = "";
    for part in cred.split(';') {
        let kv = part.trim();
        if let Some(v) = kv.strip_prefix("temp_mail_session=") {
            session = v;
        }
    }
    if session.is_empty() {
        return Err("tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱".into());
    }
    /* 会话绑定邮箱：以请求邮箱为准重拼 cookie，防止凭据与邮箱错配 */
    Ok(format!("temp_email={email}; temp_mail_session={session}"))
}

/// 创建 tempmail.ee 临时邮箱
/// 事务会话：GET / 面熟 → POST /api/mailbox/change（sec-ch-ua 全套头）换新邮箱
/// → 提取 Set-Cookie 中的 temp_mail_session，随 EmailInfo.token 透传给读信。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let client = http_client_no_cookie_jar();

        /* 步骤 1：GET / 建立 Cookie 会话（面熟首访） */
        let boot = client
            .get(BASE_URL)
            .header("User-Agent", EE_UA)
            .header(
                "Accept",
                "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            )
            .send()
            .await
            .map_err(|e| format!("tempmail-ee: 建立会话失败: {e}"))?;
        drop(boot);

        /* 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua） */
        let body = json!({
            "turnstileToken": Value::Null,
            "browserIntegrity": browser_integrity(),
        });
        let ch_resp = set_browser_headers(
            client
                .post(format!("{BASE_URL}/api/mailbox/change"))
                .body(body.to_string()),
            true,
        )
        .send()
        .await
        .map_err(|e| format!("tempmail-ee: 换箱请求失败: {e}"))?;

        let (cookie_email, session) = cookie_from_response(&ch_resp);
        let status = ch_resp.status();
        let ch_raw = ch_resp
            .text()
            .await
            .map_err(|e| format!("tempmail-ee: 读取换箱响应失败: {e}"))?;

        let chg: Value = serde_json::from_str(&ch_raw)
            .map_err(|e| format!("tempmail-ee: 解析换箱响应失败: {e}"))?;
        let success = chg.get("success").and_then(|v| v.as_bool()).unwrap_or(false);
        let mut email = chg
            .get("newEmail")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if !success || email.is_empty() {
            return Err(format!(
                "tempmail-ee: 建箱失败: {}（status {status}）",
                ch_raw.trim()
            ));
        }

        /* 以防万一以响应体为准，Cookie 仅取 session */
        if !cookie_email.is_empty() && cookie_email != email {
            email = cookie_email;
        }
        if session.is_empty() {
            return Err("tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信".into());
        }

        /* expiresAt 缺失时以 60 分钟兜底 */
        let expires = chg
            .get("expiresAt")
            .and_then(|v| v.as_str())
            .filter(|s| !s.is_empty())
            .map(|s| s.to_string())
            .unwrap_or_else(|| {
                (chrono::Utc::now() + chrono::Duration::minutes(60)).to_rfc3339()
            });

        Ok(EmailInfo {
            channel: Channel::TempmailEe,
            token: Some(build_token(&email, &session)),
            email,
            expires_at: chrono::DateTime::parse_from_rfc3339(&expires)
                .map(|dt| dt.timestamp_millis())
                .ok(),
            created_at: None,
        })
    })
}

/// 读取 tempmail.ee 收件箱
/// 凭据来自 Generate 时从 change 响应提取的 temp_mail_session，
/// 逐请求以显式 Cookie 头携带（temp_email + temp_mail_session 缺一不可）。
/// 列表只含元数据，正文逐封二拉 GET /api/mails/{id} 并解析 MIME multipart。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    if em.is_empty() {
        return Err("tempmail-ee: 邮箱为空".into());
    }
    if token.trim().is_empty() {
        return Err("tempmail-ee: token 为空".into());
    }

    /* 解析会话凭据；无有效 temp_mail_session 无法通过平台会话校验 */
    let cookie = parse_token(token, em)?;

    block_on(async {
        let client = http_client_no_cookie_jar();
        let body = json!({ "email": em }).to_string();
        let resp = set_browser_headers(client.post(format!("{BASE_URL}/api/mails")).body(body), true)
            .header("Cookie", &cookie)
            .send()
            .await
            .map_err(|e| format!("tempmail-ee: 获取收件箱失败: {e}"))?;

        let status = resp.status();
        let raw = resp
            .text()
            .await
            .map_err(|e| format!("tempmail-ee: 读取收件箱响应失败: {e}"))?;
        if status.as_u16() == 403 {
            return Err("tempmail-ee inbox: http 403（会话 Cookie 校验失败，邮箱可能已过期，请重新 Generate）".into());
        }
        if !status.is_success() {
            return Err(format!("tempmail-ee inbox: http {status}: {}", raw.trim()));
        }

        let data: Value = serde_json::from_str(&raw)
            .map_err(|e| format!("tempmail-ee: 解析收件箱响应失败: {e}"))?;
        let mails = data
            .get("mails")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();

        let mut out = Vec::with_capacity(mails.len());
        for row in &mails {
            let id = get_str(row, &["id"]);
            if id.is_empty() {
                continue;
            }
            let to = {
                let t = get_str(row, &["toAddress", "to"]);
                if t.is_empty() {
                    em.to_string()
                } else {
                    t
                }
            };
            let created = {
                let c = get_str(row, &["createdAt", "date", "receivedAt"]);
                if c.is_empty() {
                    chrono::Utc::now().to_rfc3339()
                } else {
                    c
                }
            };
            let from = get_str(row, &["fromAddress", "from", "sender"]);
            let subject = get_str(row, &["subject"]);
            let is_read = get_bool(row, "isRead");

            /* 单封详情拉取失败不阻塞列表其余邮件（详情偶发 4xx/网络抖动） */
            let (text, html) = match fetch_detail(&client, &id, &cookie).await {
                Ok(v) => v,
                Err(_) => continue,
            };

            let flat = json!({
                "id": id,
                "from": from,
                "to": to,
                "subject": subject,
                "text": text,
                "html": html,
                "date": created,
                "isRead": is_read,
            });
            out.push(normalize_email(&flat, em));
        }
        Ok(out)
    })
}

/* 拉取单封详情并解析正文：GET /api/mails/{id}（带显式会话 Cookie） */
async fn fetch_detail(
    client: &wreq::Client,
    id: &str,
    cookie: &str,
) -> Result<(String, String), String> {
    let resp = set_browser_headers(client.get(format!("{BASE_URL}/api/mails/{id}")), false)
        .header("Accept", "application/json")
        .header("Cookie", cookie)
        .send()
        .await
        .map_err(|e| e.to_string())?;
    if !resp.status().is_success() {
        return Err(format!("tempmail-ee detail: http {}", resp.status()));
    }
    let data: Value = resp.json().await.map_err(|e| e.to_string())?;
    let mut content = get_str(&data, &["content"]);
    if content.is_empty() {
        /* content 缺失时回退兜底候选键，避免平台字段演进后正文丢失 */
        content = get_str(&data, &["text", "body", "html"]);
        if content.is_empty() {
            return Err("tempmail-ee detail: 无正文字段".into());
        }
    }
    Ok(parse_content(&content))
}

/*
 * 解析详情 content：
 * 1) 平台已做 HTML 实体转义（= 写成 &#61;、+ 写成 &#43;、/ 写成 &#47;），
 *    先统一反转义。
 * 2) content 是 MIME multipart 原文（含顶部边界装饰头），按首个边界行
 *    切块，各 part 依据 Content-Transfer-Encoding 做 base64 /
 *    quoted-printable 解码，text part 入文本、html part 入 HTML。
 * 3) 无有效 multipart 结构时整个 content 去壳后作为正文，避免丢信。
 *   （text/html 互转兜底由 normalize_email 完成）
 */
fn parse_content(raw: &str) -> (String, String) {
    let mut payload = html_unescape(raw);
    payload = payload.replace("\r\n", "\n");

    let lines: Vec<&str> = payload.lines().collect();
    let boundary = boundary_of(&lines);
    let mut text = String::new();
    let mut html = String::new();
    if !boundary.is_empty() {
        (text, html) = parts_of(&lines, &boundary);
    }
    if boundary.is_empty() || (text.is_empty() && html.is_empty()) {
        (text, html) = singleton(&payload);
    }
    (text, html)
}

/* 扫描首块寻找边界行（默认 multipart 边界行处于块首） */
fn boundary_of(lines: &[&str]) -> String {
    for ln in lines.iter().take(120) {
        let s = ln.trim_end_matches('\r');
        if s.starts_with("--") && s.len() > 2 {
            return s.trim_start_matches("--").to_string();
        }
    }
    String::new()
}

/* 按 boundary 拆分 multipart 并解码归并 text/html 两个 part */
fn parts_of(lines: &[&str], boundary: &str) -> (String, String) {
    let (mut text, mut html) = (String::new(), String::new());
    let mut i = 0;
    while i < lines.len() {
        let ln = lines[i].trim_end_matches('\r');
        if !ln.starts_with(&format!("--{boundary}")) {
            i += 1;
            continue;
        }
        if ln.starts_with(&format!("--{boundary}--")) {
            break;
        }

        /* part 头部：直到首个空行（RFC 空行在 Content-* 头之后） */
        let mut headers: HashMap<String, String> = HashMap::new();
        let mut j = i + 1;
        while j < lines.len() && !lines[j].is_empty() && !lines[j].starts_with(&format!("--{boundary}")) {
            if let Some(k) = lines[j].find(':') {
                headers.insert(
                    lines[j][..k].trim().to_lowercase(),
                    lines[j][k + 1..].trim().to_string(),
                );
            }
            j += 1;
        }
        if j < lines.len() && lines[j].is_empty() {
            j += 1;
        }

        /* part 正文：到下一个边界行为止，内部空行属于正文内容 */
        let mut body: Vec<&str> = Vec::new();
        while j < lines.len() && !lines[j].starts_with(&format!("--{boundary}")) {
            body.push(lines[j]);
            j += 1;
        }
        (text, html) = merge_part(&body, &headers, text, html);
        i = j.saturating_sub(1);
    }
    (text, html)
}

/* 单个 part 的解码归并：按 content-type 归类，缺省回退 text 槽 */
fn merge_part(
    body: &[&str],
    headers: &HashMap<String, String>,
    mut text: String,
    mut html: String,
) -> (String, String) {
    let mut ct = headers.get("content-type").cloned().unwrap_or_default();
    if let Some(idx) = ct.find(';') {
        ct.truncate(idx);
    }
    let ct = ct.to_lowercase();
    let cte = headers
        .get("content-transfer-encoding")
        .cloned()
        .unwrap_or_default()
        .to_lowercase();
    let content = body.join("\n");
    if ct.contains("text/plain") {
        if text.is_empty() {
            text = decode_part(&content, &cte);
        }
    } else if ct.contains("text/html") {
        if html.is_empty() {
            html = decode_part(&content, &cte);
        }
    } else if text.is_empty() {
        text = decode_part(&content, &cte);
    }
    (text, html)
}

/* 单 part（无边界或拆不出内容）降级解析：首个空行后即正文，按关键字识别编码 */
fn singleton(payload: &str) -> (String, String) {
    let mut body = payload.trim().to_string();
    if body.contains('\n') {
        let lines: Vec<&str> = payload.lines().collect();
        for (i, ln) in lines.iter().enumerate() {
            if ln.trim().is_empty() {
                body = lines[i + 1..].join("\n");
                break;
            }
            if i > 40 || (i >= 3 && !ln.contains(':')) {
                break;
            }
        }
    }
    let lower = payload.to_lowercase();
    let cte = if lower.contains("base64") {
        "base64"
    } else if lower.contains("quoted-printable") {
        "quoted-printable"
    } else {
        ""
    };
    (decode_part(&body, cte), String::new())
}

/* 按 Content-Transfer-Encoding 解码 part 内容 */
fn decode_part(data: &str, cte: &str) -> String {
    let data = data.trim();
    match cte {
        "base64" => {
            let joined: String = data
                .chars()
                .filter(|c| !matches!(c, '\n' | '\r' | '\t' | ' '))
                .collect();
            STANDARD
                .decode(joined.as_bytes())
                .map(|b| String::from_utf8_lossy(&b).trim().to_string())
                .unwrap_or_else(|_| data.to_string())
        }
        "quoted-printable" => qp_decode(data),
        _ => data.to_string(),
    }
}

/* 最小 quoted-printable 解码：=XX 十六进制、行尾 = 软换行 */
fn qp_decode(data: &str) -> String {
    let bytes = data.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        let b = bytes[i];
        if b == b'=' && i + 1 < bytes.len() {
            /* 行尾软换行：=\r\n 或 =\n */
            if bytes[i + 1] == b'\r' && i + 2 < bytes.len() && bytes[i + 2] == b'\n' {
                i += 3;
                continue;
            }
            if bytes[i + 1] == b'\n' {
                i += 2;
                continue;
            }
            if i + 2 < bytes.len() {
                if let (Some(h), Some(l)) = (hex_val(bytes[i + 1]), hex_val(bytes[i + 2])) {
                    out.push((h << 4) | l);
                    i += 3;
                    continue;
                }
            }
        }
        out.push(b);
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

/* 十六进制字符取值 */
fn hex_val(b: u8) -> Option<u8> {
    match b {
        b'0'..=b'9' => Some(b - b'0'),
        b'a'..=b'f' => Some(b - b'a' + 10),
        b'A'..=b'F' => Some(b - b'A' + 10),
        _ => None,
    }
}

/* HTML 实体反转义（命名实体 + 十进制/十六进制数字实体） */
fn html_unescape(s: &str) -> String {
    ENTITY_RE
        .replace_all(s, |caps: &regex::Captures| -> String {
            if let Some(hex) = caps.get(1) {
                let v = u32::from_str_radix(&hex.as_str()[1..], 16).unwrap_or(0);
                return char::from_u32(v).map(|c| c.to_string()).unwrap_or_default();
            }
            if let Some(dec) = caps.get(2) {
                let v = dec.as_str().parse::<u32>().unwrap_or(0);
                return char::from_u32(v).map(|c| c.to_string()).unwrap_or_default();
            }
            if let Some(name) = caps.get(3) {
                return match name.as_str() {
                    "lt" => "<".to_string(),
                    "gt" => ">".to_string(),
                    "amp" => "&".to_string(),
                    "quot" => "\"".to_string(),
                    "apos" => "'".to_string(),
                    "nbsp" => "\u{a0}".to_string(),
                    _ => caps[0].to_string(),
                };
            }
            caps[0].to_string()
        })
        .into_owned()
}

/* 按候选键顺序提取字符串值 */
fn get_str(m: &Value, keys: &[&str]) -> String {
    for key in keys {
        if let Some(v) = m.get(key) {
            if v.is_null() {
                continue;
            }
            if let Some(s) = v.as_str() {
                return s.trim().to_string();
            }
            return v.to_string().trim().to_string();
        }
    }
    String::new()
}

/* 提取布尔值（兼容 bool / 0|1 数值与字符串） */
fn get_bool(m: &Value, key: &str) -> bool {
    match m.get(key) {
        Some(Value::Bool(b)) => *b,
        Some(Value::Number(n)) => n.as_f64().map(|f| f != 0.0).unwrap_or(false),
        Some(Value::String(s)) => s == "1" || s.eq_ignore_ascii_case("true"),
        _ => false,
    }
}