/*!
 * Tempmailto 渠道实现（tempmailto.com，Laravel）
 *
 * 平台邮箱由服务端渲染在首页（#mainEmail value），会话状态整体承载于
 * Cookie，无独立 API 密钥：
 *   - GET https://tempmailto.com/ 建立 Cookie 会话并从首页提取当前邮箱
 *     （同一会话内不变，因此建箱只需 GET + 提取）；
 *   - POST /get_messages（表单 _token=<CSRF>&captcha= 留空）返回
 *     {status, mailbox, email_token, messages, histories}，200 即成功；
 *   - POST /change（表单 _token + name + domain）把会话拉回目标邮箱；
 *   - 单封详情 GET /view/{id}（同 Cookie 会话）。
 *
 * 会话隔离：本渠道使用 http_client_no_cookie_jar（无 Cookie 罐），
 * 以模块级 Cookie 存储手动接管会话（Set-Cookie 键值覆写、请求时拼
 * Cookie 头），避免污染全局 Cookie 罐（并发安全）。
 *
 * 会话粘性：邮箱由会话 Cookie 承载，无独立密钥。SDK 层约定
 * Token=邮箱（注册表以 token 非空作防御），读到的当前邮箱与请求邮箱
 * 不一致时用 change 以请求邮箱名拉回目标邮箱。
 *
 * 有效期：平台邮箱约 10 分钟无活动过期；EmailInfo 无精确 expires 字段
 * 可填（expires_at/created_at 均留 None）。
 */

use crate::config::{block_on, get_current_ua, http_client_no_cookie_jar};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use regex::Regex;
use serde_json::{json, Value};
use std::sync::{LazyLock, Mutex};

const BASE_URL: &str = "https://tempmailto.com";

/* 首页浏览器特征 Accept（同站同源调用一致） */
const ACCEPT_HTML: &str =
    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8";

/* 表单类请求（读信/换箱）Accept */
const ACCEPT_FETCH: &str = "application/json, text/plain, */*";

/* 详情正文容器候选 class（平台视图页结构，依次尝试） */
const BODY_CLASSES: [&str; 6] = [
    "mail-body",
    "mail_content",
    "email-body",
    "content-body",
    "message-content",
    "mail-content",
];

/* 首页服务端渲染邮箱：id="mainEmail" ... value="..." */
static MAIN_EMAIL_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?is)id="mainEmail"[^>]*\bvalue="([^"]+)""#).expect("mainEmail 正则无效")
});

/* <meta name="csrf-token" content="..."> */
static CSRF_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"<meta\s+name="csrf-token"\s+content="([^"]+)""#).expect("csrf 正则无效")
});

/* script/style 开标签捕获（宽松匹配词首，与旧反向引用模式的起点一致） */
static SCRIPT_STYLE_OPEN_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?is)<(script|style)").expect("script/style 开标签正则无效")
});

/* script/style 闭合标签（由开标签命中内容择一使用） */
static SCRIPT_CLOSE_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)</script>").expect("script 闭合正则无效"));
static STYLE_CLOSE_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?i)</style>").expect("style 闭合正则无效"));

/* 任意标签剔除 */
static TAG_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"(?s)<[^>]+>").expect("标签正则无效"));

/* HTML 实体匹配：命名实体与十进制/十六进制数字实体 */
static ENTITY_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"&(?:#([xX][0-9a-fA-F]+)|#([0-9]+)|([a-zA-Z]+));").expect("实体正则无效")
});

/* 模块级 Cookie 存储：key=value 键值对，合并 Set-Cookie 时覆写同键 */
static COOKIE_JAR: Mutex<Vec<(String, String)>> = Mutex::new(Vec::new());

/* 合并响应 Set-Cookie 头到模块级 Cookie 存储（取首个分号前的键值对） */
fn merge_set_cookies(resp: &wreq::Response) {
    for val in resp.headers().get_all("set-cookie") {
        let Ok(s) = val.to_str() else {
            continue;
        };
        let Some(first) = s.split(';').next() else {
            continue;
        };
        let Some(i) = first.find('=') else {
            continue;
        };
        let (k, v) = (first[..i].trim(), first[i + 1..].trim());
        if k.is_empty() {
            continue;
        }
        let mut jar = COOKIE_JAR.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(slot) = jar.iter_mut().find(|(key, _)| key == k) {
            *slot = (k.to_string(), v.to_string());
        } else {
            jar.push((k.to_string(), v.to_string()));
        }
    }
}

/* 取出模块级 Cookie 存储并拼为 Cookie 头 */
fn cookie_header() -> String {
    let jar = COOKIE_JAR.lock().unwrap_or_else(|e| e.into_inner());
    jar.iter()
        .map(|(k, v)| format!("{k}={v}"))
        .collect::<Vec<_>>()
        .join("; ")
}

/* 设置同站浏览器特征头（与前端同源调用一致，不含 Cookie） */
fn set_browser_headers(req: wreq::RequestBuilder, accept: &str) -> wreq::RequestBuilder {
    let cookie = cookie_header();
    let mut req = req
        .header("User-Agent", get_current_ua())
        .header("Accept", accept)
        .header("Accept-Language", "en-US,en;q=0.9")
        .header("Origin", BASE_URL)
        .header("Referer", format!("{BASE_URL}/"));
    if !cookie.is_empty() {
        req = req.header("Cookie", cookie);
    }
    req
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

/* script/style 整块剔除（regex 箱不支持反向引用：先捕获起始标签，再对齐匹配闭合标签） */
fn strip_script_style(src: &str) -> String {
    let mut out = String::with_capacity(src.len());
    let mut rest = src;
    loop {
        let Some(m) = SCRIPT_STYLE_OPEN_RE.find(rest) else {
            out.push_str(rest);
            break;
        };
        out.push_str(&rest[..m.start()]);
        out.push(' ');
        let after_open = &rest[m.end()..];
        let kind = m.as_str().trim_start_matches('<').to_ascii_lowercase();
        let close = if kind == "script" { &SCRIPT_CLOSE_RE } else { &STYLE_CLOSE_RE };
        match close.find(after_open) {
            Some(close_m) => rest = &after_open[close_m.end()..],
            None => {
                /* 无闭合标签时保留原块（开标签交由后续 TAG_RE 剔除），与旧无匹配行为一致 */
                out.push_str(&rest[m.start()..]);
                break;
            }
        }
    }
    out
}

/* HTML 转纯文本（去 script/style/标签 + 反转义 + 空白压缩） */
fn html_to_text(src: &str) -> String {
    let s = strip_script_style(src);
    let s = TAG_RE.replace_all(&s, " ");
    html_unescape(&s)
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
}

/* 纯文本转 HTML（<pre> 包裹 + 转义），与 Go 端兜底形态一致 */
fn text_to_html(src: &str) -> String {
    let escaped = src
        .replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&#39;");
    format!("<html><body><pre>{escaped}</pre></body></html>")
}

/* 按候选键顺序提取字符串值（数字转十进制字符串，其余返回空串） */
fn str_of(m: &Value, keys: &[&str]) -> String {
    for key in keys {
        let Some(v) = m.get(key) else {
            continue;
        };
        if v.is_null() {
            continue;
        }
        let s = match v {
            Value::String(s) => s.clone(),
            Value::Number(n) => n.to_string(),
            _ => v.to_string(),
        };
        let s = s.trim().to_string();
        if !s.is_empty() && s != "<nil>" {
            return s;
        }
    }
    String::new()
}

/* is_seen 三态转布尔（bool / 数值非 0 / 字符串 "1"|"true" 忽略大小写） */
fn seen_of(v: &Value) -> bool {
    match v {
        Value::Bool(b) => *b,
        Value::Number(n) => n.as_f64().map(|f| f != 0.0).unwrap_or(false),
        Value::String(s) => {
            let t = s.trim();
            t == "1" || t.eq_ignore_ascii_case("true")
        }
        _ => false,
    }
}

/*
 * GET 首页并吞下 Set-Cookie（CSRF 与主邮箱提取共用）
 * 返回非 2xx 时报错（body 截断用于诊断）
 */
async fn get_home(client: &wreq::Client) -> Result<String, String> {
    let resp = set_browser_headers(client.get(BASE_URL), ACCEPT_HTML)
        .send()
        .await
        .map_err(|e| format!("tempmailto: 建立会话失败: {e}"))?;
    merge_set_cookies(&resp);
    let status = resp.status();
    let body = resp
        .text()
        .await
        .map_err(|e| format!("tempmailto: 读取首页响应失败: {e}"))?;
    if !status.is_success() {
        return Err(format!(
            "tempmailto: 首页 http {}: {}",
            status,
            truncate(&body)
        ));
    }
    Ok(body)
}

/* 拉取首页并提取 CSRF _token（读信/换箱共用） */
async fn fetch_csrf(client: &wreq::Client) -> Result<String, String> {
    let body = get_home(client).await?;
    match CSRF_RE.captures(&body).and_then(|c| c.get(1)) {
        Some(m) if !m.as_str().trim().is_empty() => Ok(m.as_str().to_string()),
        _ => Err("tempmailto: 首页未找到 csrf-token".to_string()),
    }
}

/*
 * POST /get_messages（表单 _token=<CSRF>&captcha= 留空）
 * 返回完整响应（含 mailbox 与 messages 列表）
 */
async fn fetch_messages(client: &wreq::Client) -> Result<Value, String> {
    let csrf = fetch_csrf(client).await?;
    let form = format!("_token={}&captcha=", urlencoding::encode(&csrf));
    let resp = set_browser_headers(
        client
            .post(format!("{BASE_URL}/get_messages"))
            .header(
                "Content-Type",
                "application/x-www-form-urlencoded; charset=UTF-8",
            )
            .header("X-Requested-With", "XMLHttpRequest")
            .body(form),
        ACCEPT_FETCH,
    )
    .send()
    .await
    .map_err(|e| format!("tempmailto 读信: 请求失败: {e}"))?;
    merge_set_cookies(&resp);
    let status = resp.status();
    let body = resp
        .text()
        .await
        .map_err(|e| format!("tempmailto: 读取读信响应失败: {e}"))?;
    if !status.is_success() {
        return Err(format!(
            "tempmailto 读信: http {}: {}",
            status,
            truncate(&body)
        ));
    }
    serde_json::from_str(&body).map_err(|e| format!("tempmailto: 解析读信响应失败: {e}"))
}

/*
 * 换箱（POST /change：_token + name + domain）
 * @param email 目标邮箱（作为 name+domain 来源）
 * @returns 变更后的当前邮箱
 */
async fn change_mailbox(client: &wreq::Client, email: &str) -> Result<String, String> {
    let csrf = fetch_csrf(client).await?;
    let name = email.split('@').next().unwrap_or("").trim().to_string();
    let name = if name.is_empty() {
        "TmSdk".to_string()
    } else {
        name
    };
    let domain = email
        .rsplit_once('@')
        .map(|(_, d)| d.to_string())
        .unwrap_or_else(|| "tempmailto.com".to_string());

    let form = format!(
        "_token={}&name={}&domain={}",
        urlencoding::encode(&csrf),
        urlencoding::encode(&name),
        urlencoding::encode(&domain)
    );
    let resp = set_browser_headers(
        client
            .post(format!("{BASE_URL}/change"))
            .header(
                "Content-Type",
                "application/x-www-form-urlencoded; charset=UTF-8",
            )
            .header("X-Requested-With", "XMLHttpRequest")
            .body(form),
        ACCEPT_FETCH,
    )
    .send()
    .await
    .map_err(|e| format!("tempmailto: 请求 /change 失败: {e}"))?;
    merge_set_cookies(&resp);
    let body = resp
        .text()
        .await
        .map_err(|e| format!("tempmailto: 读取 change 响应失败: {e}"))?;
    let data: Value = serde_json::from_str(&body)
        .map_err(|e| format!("tempmailto: 解析 change 响应失败: {e}"))?;
    match data.get("mailbox").and_then(|v| v.as_str()) {
        Some(m) if !m.trim().is_empty() => Ok(m.trim().to_string()),
        _ => Err(format!("tempmailto: change 响应异常: {}", truncate(&body))),
    }
}

/*
 * GET /view/{id} 提取邮件正文 HTML（同 Cookie 会话）
 * 平台视图页结构以候选 class 依次尝试，全失败回退 <main>/<article> 区块；
 * 仍失败返回空串（列表归一不因此中断）。
 */
async fn view_detail(client: &wreq::Client, id: &str) -> String {
    let resp = match set_browser_headers(
        client.get(format!("{BASE_URL}/view/{}", urlencoding::encode(id))),
        ACCEPT_HTML,
    )
    .send()
    .await
    {
        Ok(r) if r.status().is_success() => r,
        _ => return String::new(),
    };
    merge_set_cookies(&resp);
    let page = resp.text().await.unwrap_or_default();

    for class in BODY_CLASSES {
        let re = match Regex::new(&format!(
            r#"(?is)<[^>]+class="[^"]*\b{}\b[^"]*"[^>]*>([\s\S]*?)</(?:div|section|article)>"#,
            regex::escape(class)
        )) {
            Ok(re) => re,
            Err(_) => continue,
        };
        if let Some(m) = re.captures(&page) {
            let inner = m.get(1).map(|x| x.as_str()).unwrap_or("").trim();
            if !inner.is_empty() {
                return inner.to_string();
            }
        }
    }
    if let Some(inner) = extract_main_or_article(&page) {
        let inner = inner.trim();
        if !inner.is_empty() {
            return inner.to_string();
        }
    }
    String::new()
}

/* <main>/<article> 开标签（详情回退用） */
static MAIN_OR_ARTICLE_OPEN_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?is)<(main|article)[^>]*>").expect("main/article 开标签正则无效")
});

/*
 * 提取 <main>/<article> 区块内容。
 * regex 箱不支持反向引用：先捕获标签名（main/article），再用捕获值拼接
 * 闭合标签正则做显式二次匹配，语义与原 "<(main|article)[^>]*>([\s\S]*?)</\1>" 等价
 * （非贪婪取第一个同名闭合标签）。
 */
fn extract_main_or_article(page: &str) -> Option<&str> {
    let cap = MAIN_OR_ARTICLE_OPEN_RE.captures(page)?;
    let tag = cap.get(1)?.as_str().to_ascii_lowercase();
    let after_open = &page[cap.get(0)?.end()..];
    let close_re = Regex::new(&format!(r"(?i)</{}>", regex::escape(&tag))).ok()?;
    let close_start = close_re.find(after_open)?.start();
    Some(&after_open[..close_start])
}

/* 诊断用截断：与 Go 端 strings.TrimSpace(body) 截断展示一致 */
fn truncate(s: &str) -> String {
    let t = s.trim();
    if t.len() > 200 {
        format!("{}...", &t[..200])
    } else {
        t.to_string()
    }
}

/* 将 messages 列表元素组装为标准化 Email（正文来自详情页 /view/{id}） */
async fn build_email(client: &wreq::Client, row: &Value, email: &str) -> Option<Email> {
    let id = str_of(row, &["id"]);
    if id.is_empty() {
        return None;
    }
    let from_email = str_of(row, &["from_email", "from"]);
    let from_name = str_of(row, &["from_name"]);
    let mut from = if !from_name.is_empty() && from_name != from_email {
        format!("{from_name} <{from_email}>")
    } else {
        from_name
    };
    if from.is_empty() {
        from = from_email;
    }
    let subject = str_of(row, &["subject"]);
    let date = str_of(row, &["receivedAt", "received_at", "createdAt"]);
    let date = if date.is_empty() {
        chrono::Utc::now().to_rfc3339()
    } else {
        date
    };

    /* 详情正文拉取失败不影响该封邮件其余字段 */
    let mut html = view_detail(client, &id).await;
    let mut text = String::new();
    if !html.is_empty() {
        text = html_to_text(&html);
    }
    if text.is_empty() {
        text = str_of(row, &["body", "text", "snippet", "preview"]);
        if text.is_empty() {
            text = subject.clone();
        }
    }
    if html.is_empty() {
        html = text_to_html(&text);
    }

    let flat = json!({
        "id": id,
        "from": from,
        "to": email,
        "subject": subject,
        "text": text,
        "html": html,
        "date": date,
        "is_seen": seen_of(row.get("is_seen").unwrap_or(&Value::Null)),
    });
    Some(normalize_email(&flat, email))
}

/// 创建 tempmailto.com 临时邮箱
/// GET 首页建立会话并提取服务端渲染的当前邮箱；Token 约定为邮箱本身
/// （注册表需 token 非空）。邮箱约 10 分钟无活动过期，EmailInfo 无
/// expires 字段，expires_at/created_at 均留 None。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let client = http_client_no_cookie_jar();
        let body = get_home(&client).await?;

        let email = MAIN_EMAIL_RE
            .captures(&body)
            .and_then(|c| c.get(1))
            .map(|m| m.as_str().trim().to_string())
            .filter(|s| !s.is_empty())
            .ok_or_else(|| {
                "tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱".to_string()
            })?;

        Ok(EmailInfo {
            channel: Channel::Tempmailto,
            token: Some(email.clone()),
            email,
            // 平台邮箱约 10 分钟无活动过期，EmailInfo 无 expires 字段，留 None
            expires_at: None,
            created_at: None,
        })
    })
}

/// 读取 tempmailto.com 当前邮箱的收件箱。
/// 模块级 Cookie 存储粘住会话；读到的当前邮箱与请求邮箱不一致时用
/// change 拉回。token 为 Generate 时约定的邮箱（防御性非空即可，无密钥用途）。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    if em.is_empty() {
        return Err("tempmailto: 邮箱为空，请重新 Generate".to_string());
    }
    /* token 与邮箱同值（Generate 约定），仅防御性非空，不参与会话 */
    let _ = token;

    block_on(async {
        let client = http_client_no_cookie_jar();
        let mut data = fetch_messages(&client).await?;

        /* 会话当前邮箱与请求目标不一致 -> change 拉回（change 内置重新取 CSRF） */
        let mailbox = data
            .get("mailbox")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .trim()
            .to_string();
        if !mailbox.is_empty() && !mailbox.eq_ignore_ascii_case(em) {
            let changed = change_mailbox(&client, em)
                .await
                .map_err(|e| format!("tempmailto: 会话邮箱与请求不一致且拉起失败: {e}"))?;
            if !changed.eq_ignore_ascii_case(em) {
                return Err(format!(
                    "tempmailto: 会话邮箱无法拉回请求邮箱（{changed} != {em}），请重新 Generate"
                ));
            }
            /* 重新 fetch 以获取目标邮箱的消息 */
            data = fetch_messages(&client).await?;
        }

        let msgs = data
            .get("messages")
            .and_then(|v| v.as_array())
            .cloned()
            .unwrap_or_default();
        let mut out = Vec::with_capacity(msgs.len());
        for row in &msgs {
            if let Some(e) = build_email(&client, row, em).await {
                out.push(e);
            }
        }
        Ok(out)
    })
}

#[cfg(test)]
mod strip_script_style_semantics {
    use super::*;

    #[test]
    fn strip_behaves_like_backreference_pattern() {
        let cases = [
            ("a<script>x();</script>b", "a b"),
            ("a<SCRIPT>x();</SCRIPT>b", "a b"),
            ("a<style>h1{}</style>b", "a b"),
            ("a<STYLE>h1{}</STYLE>b", "a b"),
            ("a<script>t='</style>';</script><style>.a{}</style>b", "a b"),
            ("<style>unterminated", "unterminated"),
            ("a<script x=1>p</script>b<style>q</style>c", "a b c"),
        ];
        for (src, want) in cases {
            assert_eq!(html_to_text(src), want, "输入: {src}");
        }
    }

    #[test]
    fn main_article_extract_behaves_like_backreference_pattern() {
        let page = r#"<div class="x">A<main><p>body &amp; text</p><span>inner</span></main><article>tail</article>B"#;
        assert_eq!(extract_main_or_article(page), Some(r#"<p>body &amp; text</p><span>inner</span>"#));
        let article_only = "<article><b>only</b></article>";
        assert_eq!(extract_main_or_article(article_only), Some("<b>only</b>"));
        assert_eq!(extract_main_or_article("<section>no</section>"), None);
    }
}
