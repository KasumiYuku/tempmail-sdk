/**
 * TempmailEE 渠道实现（tempmail.ee）
 *
 * 会话 Cookie 绑定型：平台读信端 /api/mails 的 403 Access denied 不是 TLS 指纹或
 * 浏览器真实性校验，而是「会话 Cookie 绑定校验」——change（换箱）返回的 Set-Cookie 中
 * temp_mail_session 与 temp_email 共同构成读信凭据，读信必须带齐两者。
 * 因此只要在同一个「事务」内完成 change → 提取 Set-Cookie → 读信即可读到邮件。
 *
 * 会话隔离：所有请求须禁用自动 Cookie 罐（fetch 默认不携带/保存 cookie），
 * 会话凭据由本渠道以显式 Cookie 请求头逐请求携带，杜绝会话串池与相互污染。
 *
 * 已验证通行配方：
 *   - POST /api/mailbox/change 若不携带 sec-ch-ua 头会被平台拒绝（403 Browser request required），
 *     带齐即 200 并下发会话 Cookie；
 *   - POST /api/mails 携带正确会话 Cookie 即可 200。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { htmlToText } from "../html";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "tempmail-ee";
const BASE_URL = "https://tempmail.ee";

/* 与 sec-ch-ua 品牌版本一致的固定 UA（Chrome 154 / Linux） */
const USER_AGENT =
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

/* Token 前缀，用于识别本渠道会话凭据串 */
const TOKEN_PREFIX = "tempmail-ee|";

/** 建箱提交的浏览器指纹（与官方前端一致） */
const BROWSER_INTEGRITY: Record<string, unknown> = {
  webdriver: false,
  languagesMissing: false,
  languageMissing: false,
  pluginsMissing: false,
  pluginsUndefined: false,
  outerSizeMissing: false,
  innerSizeMissing: false,
  screenMissing: false,
  screenDepthMissing: false,
  timezoneMissing: false,
  timezoneOffsetMissing: false,
  userAgentDataPresent: true,
  userAgentMissing: false,
  platformClass: "Linux",
  mobile: false,
  collectionFailed: false,
};

interface ChangeResponse {
  success?: boolean;
  newEmail?: string;
  expiresAt?: string;
  reason?: string;
  error?: string;
  challengeRequired?: boolean;
}

interface MailsResponse {
  timestamp?: number;
  mails?: Array<Record<string, unknown>>;
}

/** 浏览器特征安全头（同站 fetch 全套） */
function browserHeaders(withSecCh: boolean): Record<string, string> {
  const headers: Record<string, string> = {
    Accept: "application/json",
    "Content-Type": "application/json",
    "X-Requested-With": "XMLHttpRequest",
    Origin: BASE_URL,
    Referer: `${BASE_URL}/`,
    "Sec-Fetch-Site": "same-origin",
    "Sec-Fetch-Mode": "cors",
    "Sec-Fetch-Dest": "empty",
    "Accept-Language": "en-US,en;q=0.9",
    "User-Agent": USER_AGENT,
  };
  if (withSecCh) {
    headers["Sec-Ch-Ua"] =
      '"Chromium";v="154", "Google Chrome";v="154", "Not.A/Brand";v="99"';
    headers["Sec-Ch-Ua-Mobile"] = "?0";
    headers["Sec-Ch-Ua-Platform"] = '"Linux"';
  }
  return headers;
}

/** 从 change 响应 Set-Cookie 提取会话 Cookie（temp_email / temp_mail_session） */
function cookieFromResponse(response: Response): {
  email: string;
  session: string;
} {
  const cookies = response.headers.getSetCookie
    ? response.headers.getSetCookie()
    : [];
  let email = "";
  let session = "";
  for (const sc of cookies) {
    const kv = sc.split(";")[0];
    if (kv.startsWith("temp_email=")) {
      email = kv.slice("temp_email=".length);
    } else if (kv.startsWith("temp_mail_session=")) {
      session = kv.slice("temp_mail_session=".length);
    }
  }
  return { email, session };
}

/** 由邮箱与 temp_mail_session 组装渠道内部凭据串 */
function buildToken(email: string, session: string): string {
  return `${TOKEN_PREFIX}temp_email=${email}; temp_mail_session=${session}`;
}

/**
 * 解析读信凭据，返回显式 Cookie 头；凭据缺失或格式不符返回 null。
 * 会话绑定邮箱以请求邮箱为准重拼，防止凭据与邮箱错配。
 */
function parseCookie(token: string, email: string): string | null {
  if (!token.startsWith(TOKEN_PREFIX)) return null;
  const cred = token.slice(TOKEN_PREFIX.length);
  let session = "";
  for (const part of cred.split(";")) {
    const kv = part.trim();
    if (kv.startsWith("temp_mail_session=")) {
      session = kv.slice("temp_mail_session=".length);
    }
  }
  if (!session) return null;
  return `temp_email=${email}; temp_mail_session=${session}`;
}

/** 带浏览器特征头的 POST（显式 Cookie 头，禁用自动 Cookie 罐） */
async function postJSON(
  path: string,
  body: unknown,
  withSecCh: boolean,
  cookie: string,
): Promise<Response> {
  const headers = browserHeaders(withSecCh);
  if (cookie) headers.Cookie = cookie;
  const response = await fetchWithTimeout(`${BASE_URL}${path}`, {
    method: "POST",
    headers,
    body: JSON.stringify(body),
    /* 不复用任何跨请求凭据，杜绝会话串池 */
    credentials: "omit",
  });
  return response;
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  /*
   * 步骤 1：GET / 面熟首访（建立平台侧会话上下文）；
   * 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua）；
   * 步骤 3：从 Set-Cookie 接管会话凭据，随 EmailInfo.token 透传给读信。
   */
  const boot = await fetchWithTimeout(BASE_URL, {
    headers: {
      "User-Agent": USER_AGENT,
      Accept:
        "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
    },
  });
  /* 丢弃面熟响应体（fetch 不带 Cookie 罐，仅完成首访） */
  await boot.text();

  const changeResponse = await postJSON(
    "/api/mailbox/change",
    {
      turnstileToken: null,
      browserIntegrity: BROWSER_INTEGRITY,
    },
    true,
    "",
  );
  const changeRaw = await changeResponse.text();
  let change: ChangeResponse;
  try {
    change = JSON.parse(changeRaw) as ChangeResponse;
  } catch {
    throw new Error(`tempmail-ee: 建箱响应解析失败: ${changeRaw.slice(0, 160)}`);
  }

  if (!change.success || !change.newEmail) {
    throw new Error(
      `tempmail-ee: 建箱失败: ${changeRaw.slice(0, 160)}（status ${changeResponse.status}）`,
    );
  }

  const cookie = cookieFromResponse(changeResponse);
  const email = String(change.newEmail).trim();
  if (!cookie.session) {
    throw new Error("tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信");
  }

  /* 过期时间取 change 响应 expiresAt，缺省按 60 分钟兜底 */
  const expires =
    change.expiresAt ||
    new Date(Date.now() + 60 * 60 * 1000).toISOString();

  return {
    channel: CHANNEL,
    email,
    token: buildToken(email, cookie.session),
    expiresAt: expires,
  };
}

/*
 * —— 详情 content 解析 ——
 * content 为平台包装后的 MIME multipart 原文（HTML 实体已转义）：
 * 1) 平台已做 HTML 实体转义（= 写成 &#61;、+ 写成 &#43;、/ 写成 &#47;），先反转义，
 *    再补充标准 HTML 反转义（&lt; &gt; &amp; 等）；
 * 2) 按首个边界行切块，各 part 依据 Content-Transfer-Encoding 做 base64 /
 *    quoted-printable 解码，text part 入纯文本、html part 入 HTML。
 */

/** 反转义多层 HTML 实体（平台转义 &#61;/&#43;/&#47; 及标准实体） */
function unescapePayload(payload: string): string {
  let out = payload;
  const replaceAll = (s: string, from: string, to: string): string =>
    s.split(from).join(to);
  out = replaceAll(out, "&#61;", "=");
  out = replaceAll(out, "&#43;", "+");
  out = replaceAll(out, "&#47;", "/");
  out = out
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">")
    .replace(/&quot;/g, '"')
    .replace(/&apos;/g, "'")
    .replace(/&amp;/g, "&");
  return out.replace(/\r\n/g, "\n");
}

/** 扫描首块寻找边界行（默认 multipart 边界行处于块首） */
function boundaryOf(lines: string[]): string {
  const limit = Math.min(lines.length, 120);
  for (let i = 0; i < limit; i++) {
    const line = lines[i];
    if (line.startsWith("--") && line.length > 2) {
      return line.replace(/^\-\-/, "").replace(/\r$/, "");
    }
  }
  return "";
}

/** 按 Content-Transfer-Encoding 解码 part 内容 */
function decodePart(data: string, cte: string): string {
  const content = data.trim();
  const encoding = cte.toLowerCase();
  if (encoding === "base64") {
    const compact = content.replace(/[\n\r\t ]/g, "");
    try {
      const buf = Buffer.from(compact, "base64");
      return buf.toString("utf8").trim();
    } catch {
      return content;
    }
  }
  if (encoding === "quoted-printable") {
    /* 软换行（行尾 =）→ 硬换行（行尾 =xx）→ 十六进制解码 */
    let out = content
      .replace(/=\r?\n/g, "")
      .replace(/[\r\n]/g, "")
      .replace(/=([0-9A-Fa-f]{2})/g, (_m, hex: string) =>
        String.fromCharCode(parseInt(hex, 16)),
      );
    /* 剩余的孤立 = 属转义残留，直接保留 */
    out = out.replace(/=([^0-9A-Fa-f]|$)/g, "$1");
    return out.trim();
  }
  return content;
}

/** 按 boundary 拆分 multipart 并解码归并 text/html 两个 part */
function parseParts(lines: string[], boundary: string): {
  text: string;
  html: string;
} {
  let text = "";
  let html = "";
  for (let i = 0; i < lines.length; i++) {
    if (!lines[i].startsWith(`--${boundary}`)) continue;
    if (lines[i].startsWith(`--${boundary}--`)) break;

    /* part 头部：直到首个空行 */
    const headers: Record<string, string> = {};
    let j = i + 1;
    for (
      ;
      j < lines.length && lines[j] !== "" && !lines[j].startsWith(`--${boundary}`);
      j++
    ) {
      const line = lines[j];
      const k = line.indexOf(":");
      if (k > 0) {
        headers[line.slice(0, k).trim().toLowerCase()] = line.slice(k + 1).trim();
      }
    }
    if (j < lines.length && lines[j] === "") j++;

    /* part 正文：到下一个边界行为止，内部空行属于正文内容 */
    const body: string[] = [];
    for (
      ;
      j < lines.length && !lines[j].startsWith(`--${boundary}`);
      j++
    ) {
      body.push(lines[j]);
    }

    const contentType = (headers["content-type"] || "").split(";")[0].toLowerCase();
    const cte = (headers["content-transfer-encoding"] || "").toLowerCase();
    const content = decodePart(body.join("\n"), cte);

    if (contentType.includes("text/plain") && !text) {
      text = content;
    } else if (contentType.includes("text/html") && !html) {
      html = content;
    } else if (!text) {
      text = content;
    }
    i = j - 1;
  }
  return { text, html };
}

/**
 * 解析详情 content：按 multipart 拆块解码，text/html 互为兜底合成；
 * 无有效边界时降级为单 part 解析（首个空行后即正文）。
 */
function parseContent(raw: string): { text: string; html: string } {
  const payload = unescapePayload(raw);
  const lines = payload.split("\n");
  const boundary = boundaryOf(lines);

  let text = "";
  let html = "";
  if (boundary) {
    const parts = parseParts(lines, boundary);
    text = parts.text;
    html = parts.html;
  }
  if (!boundary || (!text && !html)) {
    /* 无 multipart 结构：首个空行后作为正文，并识别编码方式解码 */
    let body = payload.trim();
    if (body.includes("\n")) {
      const singleLines = payload.split("\n");
      for (let i = 0; i < singleLines.length; i++) {
        if (singleLines[i].trim() === "") {
          body = singleLines.slice(i + 1).join("\n");
          break;
        }
        if (i > 40 || (i >= 3 && !singleLines[i].includes(":"))) break;
      }
    }
    const lower = payload.toLowerCase();
    const cte = lower.includes("base64")
      ? "base64"
      : lower.includes("quoted-printable")
        ? "quoted-printable"
        : "";
    text = decodePart(body, cte);
    html = "";
  }

  if (text === "" && html !== "") text = htmlToText(html);
  if (html === "" && text !== "") {
    const escapeHtml = (s: string): string =>
      s
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;");
    html = `<html><body><pre>${escapeHtml(text)}</pre></body></html>`;
  }
  return { text, html };
}

/**
 * 列表行（仅元数据）→ 归一化骨架：
 * id/fromAddress/toAddress/subject/createdAt/isRead；id 缺失视为无效行返回 null。
 */
function rowToSkeleton(
  row: Record<string, unknown>,
  email: string,
): Record<string, unknown> | null {
  const id = row.id !== undefined && row.id !== null ? String(row.id).trim() : "";
  if (!id) return null;
  const to =
    row.toAddress || row.to || email;
  const created =
    row.createdAt || row.date || row.receivedAt || new Date().toISOString();
  return {
    id,
    from: row.fromAddress || row.from || row.sender || "",
    to,
    subject: row.subject || "",
    date: created,
    isRead: row.isRead,
  };
}

/**
 * 拉取单封详情（带显式会话 Cookie）并解析正文；
 * 详情缺失 id/from/to/subject/date 时以列表骨架兜底。
 */
async function fetchDetailRow(
  cookie: string,
  skeleton: Record<string, unknown>,
): Promise<Record<string, unknown>> {
  const messageId = String(skeleton.id);
  const headers = browserHeaders(false);
  headers.Cookie = cookie;
  const response = await fetchWithTimeout(
    `${BASE_URL}/api/mails/${encodeURIComponent(messageId)}`,
    { headers, credentials: "omit" },
  );
  if (!response.ok) {
    throw new Error(`tempmail-ee detail: http ${response.status}`);
  }
  const detail = (await response.json()) as Record<string, unknown>;
  const content =
    (detail.content || detail.text || detail.body || detail.html || "") as string;
  if (!content) {
    throw new Error("tempmail-ee detail: 无正文字段");
  }

  const { text, html } = parseContent(String(content));
  return {
    id: skeleton.id,
    from: detail.fromAddress || detail.from || skeleton.from || "",
    to: detail.toAddress || detail.to || skeleton.to || "",
    subject: detail.subject || skeleton.subject || "",
    text,
    html,
    date: detail.date || skeleton.date || "",
    isRead: detail.isRead ?? skeleton.isRead,
  };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address) {
    throw new Error("tempmail-ee: 邮箱为空");
  }
  if (!String(token || "").trim()) {
    throw new Error("tempmail-ee: token 为空");
  }

  /* 解析会话凭据；无有效 temp_mail_session 无法通过平台会话校验 */
  const cookie = parseCookie(String(token).trim(), address);
  if (!cookie) {
    throw new Error(
      "tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱",
    );
  }

  const response = await postJSON("/api/mails", { email: address }, true, cookie);
  if (response.status === 403) {
    throw new Error(
      "tempmail-ee inbox: http 403（会话 Cookie 校验失败，邮箱可能已过期，请重新 Generate）",
    );
  }
  if (!response.ok) {
    throw new Error(`tempmail-ee inbox: http ${response.status}`);
  }

  const data = (await response.json()) as MailsResponse;
  const rows = Array.isArray(data.mails) ? data.mails : [];

  const emails: Email[] = [];
  for (const row of rows) {
    const skeleton = rowToSkeleton(row, address);
    if (!skeleton) continue;
    try {
      const detail = await fetchDetailRow(cookie, skeleton);
      emails.push(normalizeEmail(detail, address));
    } catch {
      /* 单封详情拉取失败不阻塞列表其余邮件 */
    }
  }
  return emails;
}