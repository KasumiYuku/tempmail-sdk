/*!
 * GeneratorEmail 渠道实现（generator.email，PHP SSR 网页型）
 *
 * 调研实证结论（与 Go 端 generator_email.go 一致）：
 *   - 无独立建箱 API：服务器端渲染直接生成随机邮箱并写进页面内联
 *     window.SITE_DATA：cur_user:"hripynok"、cur_domain:"redproxies.com"
 *     （邮箱=user@domain），同页 Set-Cookie:
 *     inbox_ctx=redproxies.com%2Fhripynok（URL 编码）选中该邮箱会话。
 *   - 读信同为 SSR：GET /inbox4/ 带 inbox_ctx Cookie 返回该邮箱渲染页。
 *     信件列表渲染在 #email-table，每条为 class 含 list-group-item2 的
 *     行（平台实测真实类为 list-group-item2，非 list-group-item）内
 *     三个子 div：from_div_45g45gg（From）、subj_div_45g45gg（Subject）、
 *     time_div_45g45gg（Time (UTC)）。空箱时容器留空、num_mess=0。
 *   - 限制：本站邮件正文不提供纯文本/HTML 原文（默认渲染摘要），
 *     SDK 按列表三要素归一。
 *
 * 会话粘性：邮箱由服务端依据 inbox_ctx Cookie 选择；SDK 全局客户端无
 *   共享 Cookie 池负担——本渠道用无 Cookie 罐客户端 + 模块级自管
 *   inbox_ctx（Generate 首页夺取，读信回带），防污染其它渠道。
 * Token 语义：token 保存 {email, domain, user} JSON 快照。
 */

use std::sync::RwLock;

use crate::config::{block_on, get_current_ua, http_client_no_cookie_jar};
use crate::normalize::normalize_email;
use crate::types::{Channel, Email, EmailInfo};
use regex::Regex;
use serde_json::{json, Value};

const SITE: &str = "https://generator.email";
const INBOX_URL: &str = "https://generator.email/inbox4/";

/// 模块级 inbox_ctx Cookie 状态（本渠道独占，Generate 夺取，读信回带）
static INBOX_CTX: RwLock<String> = RwLock::new(String::new());

/// 正则编译（惰性一次性初始化，与 Go 端 regexp.MustCompile 对齐）
fn user_re() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| Regex::new(r#"cur_user:"([^"]*)""#).unwrap())
}

fn domain_re() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| Regex::new(r#"cur_domain:"([^"]*)""#).unwrap())
}

fn item_re() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(
            r#"(?s)<div[^>]*class="[^"]*list-group-item2[^"]*"[^>]*>(.*?)(?:</div>\s*){3}"#,
        )
        .unwrap()
    })
}

fn from_re() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(r#"(?s)class="[^"]*from_div_45g45gg[^"]*"[^>]*>(.*?)</div>"#).unwrap()
    })
}

fn subj_re() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(r#"(?s)class="[^"]*subj_div_45g45gg[^"]*"[^>]*>(.*?)</div>"#).unwrap()
    })
}

fn time_re() -> &'static Regex {
    static RE: std::sync::OnceLock<Regex> = std::sync::OnceLock::new();
    RE.get_or_init(|| {
        Regex::new(r#"(?s)class="[^"]*time_div_45g45gg[^"]*"[^>]*>(.*?)</div>"#).unwrap()
    })
}

/// 取正则第一个捕获组，无匹配返回空串
fn match_first(re: &Regex, src: &str) -> String {
    re.captures(src)
        .and_then(|m| m.get(1))
        .map(|m| m.as_str().to_string())
        .unwrap_or_default()
}

/// 去标签与脚本/样式块，压缩空白
fn strip_tags(s: &str) -> String {
    let script_re = Regex::new(r"(?is)<script[\s\S]*?</script>").unwrap();
    let style_re = Regex::new(r"(?is)<style[\s\S]*?</style>").unwrap();
    let tag_re = Regex::new(r"<[^>]+>").unwrap();
    let naked_script = script_re.replace_all(s, " ");
    let stripped = style_re.replace_all(&naked_script, " ");
    let out = tag_re.replace_all(&stripped, " ");
    out.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// 收下响应 Set-Cookie 中 inbox_ctx（页面 Set-Cookie: inbox_ctx=域%2F用户）
fn merge_inbox_ctx(resp: &wreq::Response) {
    for val in resp.headers().get_all("set-cookie").iter() {
        let raw = val.to_str().unwrap_or("");
        if let Some(kv) = raw.split(';').next() {
            if let Some(value) = kv.trim().strip_prefix("inbox_ctx=") {
                if !value.is_empty() {
                    *INBOX_CTX.write().unwrap() = value.to_string();
                }
            }
        }
    }
}

/// 请求收件箱渲染页并返回 HTML（邮箱由服务端依据 inbox_ctx Cookie 选择）
async fn fetch_page(client: &wreq::Client) -> Result<String, String> {
    let ctx = INBOX_CTX.read().unwrap().clone();
    let mut req = client
        .get(INBOX_URL)
        .header("User-Agent", get_current_ua())
        .header(
            "Accept",
            "text/html,application/xhtml+xml,application/xml;q=0.9,\
             image/avif,image/webp,*/*;q=0.8",
        )
        .header("Accept-Language", "en-US,en;q=0.9")
        .header("Referer", format!("{SITE}/"));
    if !ctx.is_empty() {
        req = req.header("Cookie", format!("inbox_ctx={ctx}"));
    }

    let resp = req
        .send()
        .await
        .map_err(|e| format!("generator-email: 请求失败: {e}"))?;
    let status = resp.status();
    merge_inbox_ctx(&resp);
    let body = resp
        .text()
        .await
        .map_err(|e| format!("generator-email: 读取响应失败: {e}"))?;
    if !status.is_success() {
        return Err(format!("generator-email: 请求失败 http {status}: {body}"));
    }
    Ok(body)
}

/// 创建 generator.email 临时邮箱
/// 解析首页 SITE_DATA 快照（cur_user/cur_domain）得到邮箱地址；
/// token 保存 {email, domain, user} JSON 快照。
pub fn generate_email() -> Result<EmailInfo, String> {
    block_on(async {
        let client = http_client_no_cookie_jar();
        let src = fetch_page(&client).await?;
        let user = match_first(user_re(), &src);
        let domain = match_first(domain_re(), &src);
        if user.is_empty() || domain.is_empty() {
            return Err("generator-email: 首页未携带邮箱快照（cur_user/cur_domain）".into());
        }
        let email = format!("{user}@{domain}");
        Ok(EmailInfo {
            channel: Channel::GeneratorEmail,
            email: email.clone(),
            token: Some(
                json!({"email": email, "domain": domain, "user": user}).to_string(),
            ),
            expires_at: None,
            created_at: None,
        })
    })
}

/// 获取 generator.email 收件箱
/// 解析收件箱渲染页的列表条目（from/subj/time 三要素）；本站不提供
/// 原文正文，SDK 按摘要归一。
///
/// @param email 邮箱地址
/// @param token 会话凭据 JSON（{email, domain, user}）
pub fn get_emails(email: &str, token: &str) -> Result<Vec<Email>, String> {
    let sess: Value = serde_json::from_str(token)
        .map_err(|e| format!("generator-email: 会话凭据解析失败: {e}"))?;
    if sess.get("email").and_then(|v| v.as_str()) != Some(email) {
        return Err("generator-email: 会话邮箱与查询邮箱不匹配".into());
    }
    let sess_domain = sess
        .get("domain")
        .and_then(|v| v.as_str())
        .unwrap_or("")
        .to_string();

    block_on(async {
        let client = http_client_no_cookie_jar();
        let src = fetch_page(&client).await?;
        /* 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换 */
        let got = match_first(domain_re(), &src);
        if got != sess_domain {
            return Err(format!(
                "generator-email: 会话域名已切换（token {sess_domain}，服务端 {got}）"
            ));
        }

        /* 列表区域锚定（#email-table ... #markodile 之间） */
        let list_start = src.find(r#"id="email-table""#).unwrap_or(0);
        let list_end = src.find(r#"id="markodile""#).unwrap_or(src.len());
        let region = if list_end > list_start {
            &src[list_start..list_end]
        } else {
            ""
        };

        let mut out = Vec::new();
        for caps in item_re().captures_iter(&src) {
            let raw = caps.get(1).map(|m| m.as_str()).unwrap_or("");
            /* 跳过列表容器外的候选：要求条目文本来自列表区域 */
            if !region.is_empty() && !region.contains(raw) {
                continue;
            }
            let from = strip_tags(&match_first(from_re(), raw));
            let subject = strip_tags(&match_first(subj_re(), raw));
            let when = strip_tags(&match_first(time_re(), raw));
            if from.is_empty() && subject.is_empty() && when.is_empty() {
                continue;
            }
            let row = json!({
                "from": from,
                "to": email,
                "subject": subject,
                "date": when,
            });
            out.push(normalize_email(&row, email));
        }
        Ok(out)
    })
}