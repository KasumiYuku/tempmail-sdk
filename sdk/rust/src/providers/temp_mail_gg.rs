/*!
 * TempMailGG 渠道实现（temp-mail.gg，Laravel + Livewire v3）
 *
 * 纯 HTTP 可实现（与 Go 端调研结论一致，全站无验证码）：
 *   - GET https://temp-mail.gg/ 返回 data-csrf 属性令牌、XSRF-TOKEN /
 *     tempmail_session cookies 与初始 wire:snapshot（JSON；data.email
 *     初始为空，邮箱必须由 Livewire generateEmail 显式生成）。
 *   - POST /livewire/update 是唯一边界（同站 fetch 协议）：JSON body 顶层
 *     _token（=data-csrf）+ components[0]：{snapshot, updates:{},
 *     calls:[{path:"",method:"<call>",params:[...]}]}。
 *     generateEmail 生成邮箱并回传新快照，响应 effects.html 含收件箱整块
 *     UI（列表条目：<div wire:click="selectEmail(<数字id>)">…h3=发件人邮箱
 *     …p=主题 …p=正文预览 …span="N seconds ago"）。200 且 calls 为空的
 *     update 即平台 20 秒轮询刷信形态。
 *   - 详情：update calls=[{method:"selectEmail",params:[<数字id>]}]，
 *     响应 effects.html 的邮件模态框含 From / 时间 / Plain Text 全文
 *     （x-show="activeTab === 'text'" 区块）。
 *   - Livewire 会话轮换：Laravel 每次 livewire/update 都轮换会话 Cookie；
 *     同值重放会 419。本端使用无 Cookie 罐客户端 + 模块级 Cookie 存储，
 *     每次响应以 Set-Cookie 覆写、每个请求手动拼 Cookie 头。
 *
 * 会话粘性：token 内保存 {email, csrf, snapshot} 凭据串（快照为快照
 * 对象的 JSON 串）；GetEmails 复用 token 中的快照轮询/点开详情，并以
 * 响应快照 data.email 断言会话仍指向本邮箱（防模块级 Cookie 被并行
 * 会话覆盖后串箱）。
 */

use crate::config::{block_on, get_current_ua, http_client_no_cookie_jar};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use regex::Regex;
use serde_json::{json, Value};
use std::sync::{LazyLock, Mutex};

const BASE_URL: &str = "https://temp-mail.gg";

/* 渠道凭据串前缀，用于识别会话接管 */
const TOKEN_PREFIX: &str = "temp-mail-gg|";

/* 首页浏览器特征 Accept（GET / 与轮询页面共用形态） */
const ACCEPT_HTML: &str =
    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8";

/* 首页 data-csrf 属性令牌 */
static DATA_CSRF_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r#"data-csrf="([^"]*)""#).expect("data-csrf 正则无效"));

/* 初始 wire:snapshot 属性（值为 HTML 属性转义态 JSON） */
static SNAPSHOT_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r#"wire:snapshot="([^"]*)""#).expect("wire:snapshot 正则无效"));

/* 列表条目容器：<div wire:click="selectEmail(<数字id>)">（整块匹配） */
static ITEM_NEEDLE: &str = r#"wire:click="selectEmail("#;

/* 列表条目内 h3（class 含 font-semibold）= 发件人 */
static H3_SEMIBOLD_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?is)<h3[^>]*class="[^"]*font-semibold[^"]*"[^>]*>(.*?)</h3>"#)
        .expect("h3 正则无效")
});

/* 列表条目内 p（class 含 text-zinc-300）= 主题 */
static P_ZINC_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?is)<p[^>]*class="[^"]*text-zinc-300[^"]*"[^>]*>(.*?)</p>"#)
        .expect("主题 p 正则无效")
});

/* 列表条目内 p（class 含 line-clamp-2）= 正文预览 */
static P_CLAMP_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?is)<p[^>]*class="[^"]*line-clamp-2[^"]*"[^>]*>(.*?)</p>"#)
        .expect("预览 p 正则无效")
});

/* 列表条目内 span（class 含 text-xs）= 相对时间 */
static SPAN_XS_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?is)<span[^>]*class="[^"]*text-xs[^"]*"[^>]*>(.*?)</span>"#)
        .expect("时间 span 正则无效")
});

/* 详情 h3（class 含 text-xl）= 主题 */
static H3_XL_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?is)<h3[^>]*class="[^"]*text-xl[^"]*"[^>]*>(.*?)</h3>"#)
        .expect("详情 h3 正则无效")
});

/* 详情发件人：span 文本前缀 "From:" */
static FROM_SPAN_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r#"(?is)<span[^>]*>(.*?)</span>"#).expect("span 正则无效"));

/* 详情纯文本页签：含 activeTab === 'text' 的 x-show 容器（div） */
static TAB_TEXT_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r#"(?is)<div[^>]*x-show="[^"]*activeTab\s*===\s*'text'[^"]*"[^>]*>(.*?)</div>"#)
        .expect("text 页签正则无效")
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

/* 相对时间："N seconds/minutes/hours/days ago" */
static RELATIVE_RE: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"^\s*(\d+)\s+(second|minute|hour|day)s?\s+ago\s*$").expect("相对时间正则无效")
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

/* 设置 livewire/update 同步请求头（同站 fetch 全套，不含 Cookie） */
fn set_post_headers(req: wreq::RequestBuilder) -> wreq::RequestBuilder {
    let cookie = cookie_header();
    let mut req = req
        .header("User-Agent", get_current_ua())
        .header("Accept", "text/html, application/xhtml+xml")
        .header("Accept-Language", "en-US,en;q=0.9")
        .header("Content-Type", "application/json")
        .header("X-Livewire", "")
        .header("X-Requested-With", "XMLHttpRequest")
        .header("Origin", BASE_URL)
        .header("Referer", format!("{BASE_URL}/"));
    if !cookie.is_empty() {
        req = req.header("Cookie", cookie);
    }
    req
}

/* 设置首页 GET 请求头（UA + Accept，不含 Cookie） */
fn set_home_headers(req: wreq::RequestBuilder) -> wreq::RequestBuilder {
    let cookie = cookie_header();
    let mut req = req
        .header("User-Agent", get_current_ua())
        .header("Accept", ACCEPT_HTML)
        .header("Accept-Language", "en-US,en;q=0.9");
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

/* HTML 片段转纯文本（去 script/style/标签 + 反转义 + 空白压缩） */
fn html_to_text(src: &str) -> String {
    let s = strip_script_style(src);
    let s = TAG_RE.replace_all(&s, " ");
    let s = html_unescape(&s);
    let s = s.replace('\u{0}', "");
    s.split_whitespace().collect::<Vec<_>>().join(" ")
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

/* 诊断用截断：错误信息中 body 截断展示 */
fn truncate(s: &str) -> String {
    let t = s.trim();
    if t.len() > 200 {
        format!("{}...", &t[..200])
    } else {
        t.to_string()
    }
}

/*
 * 调用 livewire/update（模块级 Cookie 存储自动覆写新会话 Cookie）
 * @param snapshot 当前组件快照 JSON 串
 * @param csrf     data-csrf 令牌（顶层 _token）
 * @param method   组件方法（"" 表示纯轮询，即平台 20s 自动刷形态）
 * @param params   方法参数
 */
async fn livewire_update(
    client: &wreq::Client,
    snapshot: &str,
    csrf: &str,
    method: &str,
    params: &[Value],
) -> Result<Value, String> {
    let calls: Vec<Value> = if method.is_empty() {
        Vec::new()
    } else {
        vec![json!({"path": "", "method": method, "params": params})]
    };
    let payload = json!({
        "_token": csrf,
        "components": [{
            "snapshot": snapshot,
            "updates": {},
            "calls": calls,
        }],
    });

    let resp = set_post_headers(client.post(format!("{BASE_URL}/livewire/update")))
        .body(payload.to_string())
        .send()
        .await
        .map_err(|e| format!("temp-mail-gg: livewire/update 请求失败: {e}"))?;
    merge_set_cookies(&resp);
    let status = resp.status().as_u16();
    let body = resp
        .text()
        .await
        .map_err(|e| format!("temp-mail-gg: 读取 livewire 响应失败: {e}"))?;
    if status == 419 {
        return Err("temp-mail-gg: livewire 会话过期（419），请重新 Generate".to_string());
    }
    if !(200..300).contains(&status) {
        return Err(format!(
            "temp-mail-gg: livewire/update http {status}: {}",
            truncate(&body)
        ));
    }
    serde_json::from_str(&body).map_err(|e| format!("temp-mail-gg: 解析 update 响应失败: {e}"))
}

/* 从快照 JSON 串解析出 data.email（无非空则返回空串） */
fn snapshot_email(snapshot_raw: &str) -> String {
    let Ok(snap) = serde_json::from_str::<Value>(snapshot_raw) else {
        return String::new();
    };
    snap.get("data")
        .and_then(|d| d.get("email"))
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .trim()
        .to_string()
}

/* 从 update 响应取首组件并校验非空；失败返回 None */
fn first_component(resp: &Value) -> Option<(String, String)> {
    let c0 = resp.get("components")?.get(0)?;
    let snapshot = c0.get("snapshot")?.as_str()?.to_string();
    let html = c0
        .get("effects")
        .and_then(|e| e.get("html"))
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    Some((snapshot, html))
}

/* 列表条目（从轮询 effects.html 解析） */
struct ListRow {
    id: String,
    from: String,
    subject: String,
    preview: String,
    when: String,
}

/*
 * 用正则解析轮询响应 effects.html 的 Inbox 条目。
 * 条目容器为 <div wire:click="selectEmail(<数字id>)">，其内
 * h3(class 含 font-semibold)=发件人邮箱、p(text-zinc-300)=主题、
 * p(line-clamp-2)=正文预览、span(text-xs)=相对时间。
 */
fn parse_list(html_block: &str) -> Vec<ListRow> {
    let mut rows: Vec<ListRow> = Vec::new();
    let mut pos = 0usize;
    while pos < html_block.len() {
        let Some(rel) = html_block[pos..].find(ITEM_NEEDLE) else {
            break;
        };
        let start = pos + rel;
        let Some(after) = html_block[start..].find('>') else {
            break;
        };
        /* 容器起始标签内取 selectEmail( 后紧跟的数字 id（截到非数字为止） */
        let open_tag = &html_block[start..start + after];
        let id: String = open_tag[ITEM_NEEDLE.len()..]
            .chars()
            .take_while(|c| c.is_ascii_digit())
            .collect();
        if id.is_empty() {
            pos = start + 1;
            continue;
        }
        /* 块终点：容器 div 的闭合 </div>（平台条目内无嵌套 div） */
        let Some(close_rel) = html_block[start..].find("</div>") else {
            break;
        };
        let close_abs = start + close_rel;
        let block = &html_block[start..close_abs];
        rows.push(ListRow {
            id,
            from: first_capture(&H3_SEMIBOLD_RE, block, 1),
            subject: first_capture(&P_ZINC_RE, block, 1),
            preview: first_capture(&P_CLAMP_RE, block, 1),
            when: first_capture(&SPAN_XS_RE, block, 1),
        });
        pos = close_abs + "</div>".len();
    }
    rows
}

/* 提取正则首个捕获组的纯文本 */
fn first_capture(re: &Regex, src: &str, group: usize) -> String {
    match re.captures(src) {
        Some(c) => html_to_text(c.get(group).map(|m| m.as_str()).unwrap_or("")),
        None => String::new(),
    }
}

/* 详情视图（from, subject, text）：
 * h3(class 含 text-xl)=主题；span 文本前缀 "From:" 取发件人；
 * div x-show 含 activeTab === 'text' 取纯文本 */
fn parse_detail(html_block: &str) -> (String, String, String) {
    let mut subject = String::new();
    let mut from = String::new();
    let mut text = String::new();

    if let Some(c) = H3_XL_RE.captures(html_block) {
        subject = html_to_text(c.get(1).map(|m| m.as_str()).unwrap_or(""));
    }
    if let Some(c) = FROM_SPAN_RE.captures(html_block) {
        let content = html_to_text(c.get(1).map(|m| m.as_str()).unwrap_or(""));
        if let Some(t) = content.strip_prefix("From:") {
            from = t.trim().to_string();
        }
    }
    if let Some(c) = TAB_TEXT_RE.captures(html_block) {
        text = html_to_text(c.get(1).map(|m| m.as_str()).unwrap_or(""));
    }

    (from, subject, text)
}

/* 相对时间解析为 UTC RFC3339（"N seconds/minutes/hours/days ago"）；失败回退 now */
fn parse_relative(s: &str, now: chrono::DateTime<chrono::Utc>) -> String {
    let Some(c) = RELATIVE_RE.captures(s.trim()) else {
        return now.to_rfc3339();
    };
    let n: i64 = match c.get(1).map(|m| m.as_str()).unwrap_or("").parse() {
        Ok(n) => n,
        Err(_) => return now.to_rfc3339(),
    };
    let seconds = match c.get(2).map(|m| m.as_str()).unwrap_or("") {
        "second" => 1i64,
        "minute" => 60i64,
        "hour" => 3600i64,
        "day" => 86400i64,
        _ => return now.to_rfc3339(),
    };
    (now - chrono::Duration::seconds(n * seconds)).to_rfc3339()
}

/* 凭据串解析：前缀 + JSON {"email","csrf","snapshot"} */
fn decode_session(token: &str) -> Result<(String, String, String), String> {
    let payload = token
        .strip_prefix(TOKEN_PREFIX)
        .ok_or_else(|| "temp-mail-gg: 凭据串前缀不符，请重新 Generate".to_string())?;
    let sess: Value =
        serde_json::from_str(payload).map_err(|e| format!("temp-mail-gg: 解析凭据串失败: {e}"))?;
    let email = sess
        .get("email")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .trim()
        .to_string();
    let csrf = sess
        .get("csrf")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    let snapshot = sess
        .get("snapshot")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();
    if email.is_empty() || snapshot.is_empty() {
        return Err("temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate".to_string());
    }
    Ok((email, csrf, snapshot))
}

/* 列表行转标准化 Email（详情覆盖前的基础归一） */
fn row_to_email(row: &ListRow, email: &str, now: chrono::DateTime<chrono::Utc>) -> Email {
    let flat = json!({
        "id": row.id,
        "from": row.from,
        "to": email,
        "subject": row.subject,
        "text": row.preview,
        "date": parse_relative(&row.when, now),
    });
    normalize_email(&flat, email)
}

/// 创建 temp-mail.gg 临时邮箱
/// GET 首页取 CSRF + 初始快照 -> update calls=generateEmail ->
/// 从响应快照取 data.email，并把 {email, csrf, snapshot} 打成凭据串。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let client = http_client_no_cookie_jar();

        /* 第一步：GET / 建立会话并取 data-csrf 与初始快照 */
        let resp = set_home_headers(client.get(BASE_URL))
            .send()
            .await
            .map_err(|e| format!("temp-mail-gg: 建立会话失败: {e}"))?;
        merge_set_cookies(&resp);
        let status = resp.status();
        let body = resp
            .text()
            .await
            .map_err(|e| format!("temp-mail-gg: 读取首页响应失败: {e}"))?;
        if !status.is_success() {
            return Err(format!(
                "temp-mail-gg: 首页 http {}: {}",
                status,
                truncate(&body)
            ));
        }

        let m_csrf = DATA_CSRF_RE.captures(&body);
        let m_snap = SNAPSHOT_RE.captures(&body);
        let (Some(m_csrf), Some(m_snap)) = (m_csrf, m_snap) else {
            return Err("temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱".to_string());
        };
        let csrf = m_csrf.get(1).map(|m| m.as_str()).unwrap_or("").to_string();
        /* HTML 属性内的快照 JSON 是双转义态（&quot;），逐层反转义为原始 JSON */
        let snap_raw = html_unescape(m_snap.get(1).map(|m| m.as_str()).unwrap_or(""));

        /* 第二步：generateEmail（平台免费额度为免登录每时段 5 个，耗尽时响应无 email） */
        let update_resp = livewire_update(&client, &snap_raw, &csrf, "generateEmail", &[]).await?;
        let Some((snap2, _)) = first_component(&update_resp) else {
            return Err("temp-mail-gg: generateEmail 响应异常（components 缺失）".to_string());
        };
        let email = snapshot_email(&snap2);
        if email.is_empty() {
            return Err(
                "temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录配额（每时段 5 个）"
                    .to_string(),
            );
        }

        let token = format!(
            "{TOKEN_PREFIX}{}",
            serde_json::to_string(&json!({
                "email": email,
                "csrf": csrf,
                "snapshot": snap2,
            }))
            .map_err(|e| format!("temp-mail-gg: 凭据序列化失败: {e}"))?
        );

        Ok(EmailInfo {
            channel: Channel::TempMailGg,
            email,
            token: Some(token),
            // 平台凭据约 30 分钟有效；EmailInfo 无 expires 字段，留 None
            expires_at: None,
            created_at: None,
        })
    })
}

/// 读取 temp-mail.gg 收件箱
/// @param email 邮箱地址（与 token 内会话邮箱一致才继续）
/// @param token 凭据串（prefix | {email,csrf,snapshot}）
/// 流程：轮询 update(无 calls) 取 Inbox 列表 -> 对每封 selectEmail
/// 提取详情正文。轮询响应快照中的 data.email 与请求邮箱不一致时返回
/// 错误（防模块级 Cookie 被并发会话覆盖后串箱）。
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let em = email.trim();
    if em.is_empty() {
        return Err("temp-mail-gg: 邮箱为空，请重新 Generate".to_string());
    }
    let (sess_email, sess_csrf, sess_snapshot) = decode_session(token)?;
    if !sess_email.eq_ignore_ascii_case(em) {
        return Err(format!(
            "temp-mail-gg: 邮箱与凭据不匹配（{} != {}），请重新 Generate",
            sess_email, em
        ));
    }

    block_on(async {
        let client = http_client_no_cookie_jar();

        /* 轮询刷新（calls 为空 = 平台 20s 自动刷新形态） */
        let resp_poll = livewire_update(&client, &sess_snapshot, &sess_csrf, "", &[]).await?;
        let Some((mut latest_snap, html_block)) = first_component(&resp_poll) else {
            return Err("temp-mail-gg: 轮询响应异常（components 缺失）".to_string());
        };
        if !latest_snap.is_empty() {
            let current = snapshot_email(&latest_snap);
            if !current.is_empty() && !current.eq_ignore_ascii_case(em) {
                return Err(format!(
                    "temp-mail-gg: 会话已被切换至 {}（与请求邮箱 {} 不一致），请重新 Generate",
                    current, em
                ));
            }
        } else {
            latest_snap = sess_snapshot.clone();
        }
        if html_block.trim().is_empty() {
            return Err("temp-mail-gg: 轮询响应无 effects.html，会话可能已失效".to_string());
        }

        let rows = parse_list(&html_block);
        if rows.is_empty() {
            return Ok(Vec::new());
        }

        let now = chrono::Utc::now();
        let mut out: Vec<Email> = Vec::with_capacity(rows.len());
        for row in &rows {
            let mut email_obj = row_to_email(row, em, now);

            /* 逐封拉详情正文；详情失败回退列表字段（不中断整批） */
            if let Ok(id_num) = row.id.parse::<i64>() {
                if let Ok(resp_detail) = livewire_update(
                    &client,
                    &latest_snap,
                    &sess_csrf,
                    "selectEmail",
                    &[json!(id_num)],
                )
                .await
                {
                    if let Some((snap, html)) = first_component(&resp_detail) {
                        if !snap.is_empty() {
                            latest_snap = snap;
                        }
                        let (from, subject, text) = parse_detail(&html);
                        if !from.is_empty() {
                            email_obj.from_addr = from;
                        }
                        if !subject.is_empty() {
                            email_obj.subject = subject;
                        }
                        if !text.is_empty() {
                            email_obj.text = text.clone();
                            email_obj.html = text_to_html(&text);
                        }
                    }
                }
            }
            if email_obj.text.is_empty() {
                email_obj.text = email_obj.subject.clone();
                email_obj.html = text_to_html(&email_obj.text);
            }
            out.push(email_obj);
        }
        Ok(out)
    })
}
