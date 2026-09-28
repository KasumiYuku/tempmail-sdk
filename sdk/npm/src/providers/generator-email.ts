/**
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
 * 会话粘性：邮箱由服务端依据 inbox_ctx Cookie 选择；SDK fetch 无自动
 *   Cookie 罐，本渠道模块级自管 inbox_ctx（Generate 首页夺取，读信
 *   回带），防污染其它渠道。
 * Token 语义：token 保存 {email, domain, user} JSON 快照。
 */

import { Email, InternalEmailInfo, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "generator-email";
const BASE_URL = "https://generator.email";
const INBOX_URL = `${BASE_URL}/inbox4/`;

/* 与 Go 端 tls-client 同款形态的固定浏览器 UA（Chrome 154 / Linux） */
const USER_AGENT =
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

const PAGE_HEADERS: Record<string, string> = {
  "User-Agent": USER_AGENT,
  Accept:
    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
  "Accept-Language": "en-US,en;q=0.9",
  Referer: `${BASE_URL}/`,
};

/** 模块级 inbox_ctx Cookie 状态（本渠道独占） */
let inboxCtx = "";

/* SITE_DATA 快照提取（cur_user / cur_domain） */
const USER_RE = /cur_user:"([^"]*)"/;
const DOMAIN_RE = /cur_domain:"([^"]*)"/;
/* 列表条目与三要素块（平台实测真实类为 list-group-item2，按类名后缀锚定） */
const ITEM_RE =
  /<div[^>]*class="[^"]*list-group-item2[^"]*"[^>]*>([\s\S]*?)(?:<\/div>\s*){3}/g;
const FROM_RE = /class="[^"]*from_div_45g45gg[^"]*"[^>]*>([\s\S]*?)<\/div>/;
const SUBJ_RE = /class="[^"]*subj_div_45g45gg[^"]*"[^>]*>([\s\S]*?)<\/div>/;
const TIME_RE = /class="[^"]*time_div_45g45gg[^"]*"[^>]*>([\s\S]*?)<\/div>/;
const SCRIPT_RE = /<(script|style)[\s\S]*?<\/\1>/gi;
const TAG_RE = /<[^>]+>/g;

/** 从 Set-Cookie 提取 inbox_ctx（页面 Set-Cookie: inbox_ctx=redproxies.com%2Fhripynok） */
function harvestInboxCtx(response: Response): string {
  let lines: string[] = [];
  try {
    const h = response.headers as Headers & { getSetCookie?: () => string[] };
    if (typeof h.getSetCookie === "function") {
      lines = h.getSetCookie();
    }
  } catch {
    lines = [];
  }
  if (lines.length === 0) {
    lines = (response.headers.get("set-cookie") || "").split(/,(?=[^;]*=)/);
  }
  for (const line of lines) {
    const kv = (line.split(";")[0] || "").trim();
    if (kv.startsWith("inbox_ctx=") && kv.length > "inbox_ctx=".length) {
      return kv.slice("inbox_ctx=".length);
    }
  }
  return "";
}

/** 请求收件箱渲染页并返回 HTML（邮箱由服务端依据 inbox_ctx Cookie 选择） */
async function fetchPage(): Promise<string> {
  const headers = { ...PAGE_HEADERS };
  if (inboxCtx) headers.Cookie = `inbox_ctx=${inboxCtx}`;
  const response = await fetchWithTimeout(INBOX_URL, { headers });
  if (!response.ok) {
    throw new Error(`generator-email: 请求失败 http ${response.status}`);
  }
  const ctx = harvestInboxCtx(response);
  if (ctx) inboxCtx = ctx;
  return await response.text();
}

/** 去标签与脚本/样式块，压缩空白 */
function stripTags(s: string): string {
  return s
    .replace(SCRIPT_RE, " ")
    .replace(TAG_RE, " ")
    .replace(/\s+/g, " ")
    .trim();
}

/** 取正则第一个捕获组，无匹配返回空串 */
function matchFirst(re: RegExp, src: string): string {
  const m = re.exec(src);
  return m ? m[1] : "";
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const src = await fetchPage();
  const user = matchFirst(USER_RE, src);
  const domain = matchFirst(DOMAIN_RE, src);
  if (!user || !domain) {
    throw new Error("generator-email: 首页未携带邮箱快照（cur_user/cur_domain）");
  }
  const email = `${user}@${domain}`;
  return {
    channel: CHANNEL,
    email,
    token: JSON.stringify({ email, domain, user }),
  };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  let sess: { email?: string; domain?: string; user?: string };
  try {
    sess = JSON.parse(String(token || ""));
  } catch (err) {
    throw new Error(`generator-email: 会话凭据解析失败: ${err}`);
  }
  if (!sess || typeof sess !== "object") {
    throw new Error("generator-email: 会话凭据解析失败（非对象）");
  }
  if (sess.email !== email) {
    throw new Error("generator-email: 会话邮箱与查询邮箱不匹配");
  }

  const src = await fetchPage();
  /* 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换 */
  const gotDomain = matchFirst(DOMAIN_RE, src);
  if (gotDomain !== sess.domain) {
    throw new Error(
      `generator-email: 会话域名已切换（token ${sess.domain}，服务端 ${gotDomain}）`,
    );
  }

  /* 列表区域锚定（#email-table ... #markodile 之间） */
  const listStart = src.indexOf('id="email-table"');
  const listEnd = src.indexOf('id="markodile"');
  const region =
    listStart >= 0 && listEnd > listStart ? src.slice(listStart, listEnd) : "";

  const emails: Email[] = [];
  for (const m of src.matchAll(ITEM_RE)) {
    const raw = m[1];
    /* 跳过列表容器外的候选：要求条目文本来自列表区域 */
    if (region && !region.includes(raw)) continue;
    const from = stripTags(matchFirst(FROM_RE, raw));
    const subject = stripTags(matchFirst(SUBJ_RE, raw));
    const when = stripTags(matchFirst(TIME_RE, raw));
    if (!from && !subject && !when) continue;
    emails.push(
      normalizeEmail(
        { from, to: email, subject, date: when },
        email,
      ),
    );
  }
  return emails;
}