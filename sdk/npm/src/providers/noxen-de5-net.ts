/**
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
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "noxen-de5-net";
const BASE_URL = "https://tempmail.noxen.de5.net";
const LOGIN_USER = "guest";
const LOGIN_PASS = "123456";
/** 凭据串前缀（"基址|cookie=值"） */
const TOKEN_PREFIX = "noxen-de5-net|";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface DetailFields {
  content: string;
  htmlContent: string;
  toAddrs: string;
  download: string;
}

/** 登录取得会话 Cookie（iding-session=JWT） */
async function login(): Promise<string> {
  const response = await fetchWithTimeout(`${BASE_URL}/api/login`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
    body: JSON.stringify({ username: LOGIN_USER, password: LOGIN_PASS }),
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`noxen-de5-net login: http ${response.status}`);
  }
  const data = JSON.parse(body) as { success?: boolean };
  if (!data.success) {
    throw new Error("noxen-de5-net login: 登录失败");
  }
  /* 从 Set-Cookie 头提取 iding-session 键值对（截断至首个分号） */
  const setCookies = response.headers.get("set-cookie") || "";
  for (const part of setCookies.split(/,(?=\s*iding-session=)/)) {
    const item = part.split(";")[0].trim();
    if (item.startsWith("iding-session=")) {
      return item;
    }
  }
  throw new Error("noxen-de5-net login: 未下发会话 Cookie");
}

/** 校验会话 Cookie 是否仍有效（GET /api/session） */
async function cookieStillValid(cookie: string): Promise<boolean> {
  try {
    const response = await fetchWithTimeout(`${BASE_URL}/api/session`, {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
        Cookie: cookie,
      },
    });
    if (!response.ok) {
      return false;
    }
    const data = (await response.json()) as { authenticated?: boolean };
    return data.authenticated === true;
  } catch {
    return false;
  }
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const cookie = await login();

  const response = await fetchWithTimeout(`${BASE_URL}/api/generate`, {
    headers: {
      Accept: "application/json",
      "User-Agent": USER_AGENT,
      Cookie: cookie,
    },
  });
  if (!response.ok) {
    throw new Error(`noxen-de5-net generate: http ${response.status}`);
  }
  const data = (await response.json()) as { email?: string; expires?: number };
  const email = String(data.email || "").trim();
  if (!email) {
    throw new Error("noxen-de5-net generate: 响应缺少 email");
  }

  /* 凭据串："noxen-de5-net|iding-session=<JWT>|base=<基址>" */
  const token = `${TOKEN_PREFIX}${encodeURIComponent(cookie)}|base=${BASE_URL}`;
  let expiresAt = "";
  if (typeof data.expires === "number" && data.expires > 0) {
    expiresAt = new Date(data.expires).toISOString();
  }
  return {
    channel: CHANNEL,
    email,
    token,
    expiresAt,
  };
}

/** 拉取单封邮件详情 */
async function fetchDetail(
  cookie: string,
  id: string,
): Promise<DetailFields> {
  const response = await fetchWithTimeout(
    `${BASE_URL}/api/email/${encodeURIComponent(id)}`,
    {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
        Cookie: cookie,
      },
    },
  );
  if (!response.ok) {
    throw new Error(`noxen-de5-net detail: http ${response.status}`);
  }
  const data = (await response.json()) as {
    content?: string;
    html_content?: string;
    to_addrs?: string;
    download?: string;
  };
  return {
    content: String(data.content || ""),
    htmlContent: String(data.html_content || ""),
    toAddrs: String(data.to_addrs || ""),
    download: String(data.download || ""),
  };
}

/** 拉取原始 EML 报文（详情 download 字段指向的下载端点） */
async function fetchEML(cookie: string, download: string): Promise<Uint8Array | null> {
  try {
    let url = download;
    if (!url.startsWith("http://") && !url.startsWith("https://")) {
      url = BASE_URL + url;
    }
    const response = await fetchWithTimeout(url, {
      headers: {
        Accept: "message/rfc822, */*",
        "User-Agent": USER_AGENT,
        Cookie: cookie,
      },
    });
    if (!response.ok) {
      return null;
    }
    return new Uint8Array(await response.arrayBuffer());
  } catch {
    return null;
  }
}

/** 从 Content-Type 头值提取分块 boundary（引号可选） */
function boundaryOf(contentType: string): string {
  const match = /boundary="?([^";\s]+)"?/i.exec(contentType);
  return match ? match[1] : "";
}

/** 将原始报文切分为首部映射与正文块（CRLF 已归一为 LF） */
function splitEML(
  payload: string,
  offset: number,
): { headers: Map<string, string>; body: string } {
  const lines = payload.split("\n");
  const headers = new Map<string, string>();
  let i = offset;
  if (i < lines.length && lines[i].startsWith("From ")) {
    i++;
  }
  let currentKey = "";
  for (; i < lines.length; i++) {
    const line = lines[i];
    if (line === "") {
      i++;
      break;
    }
    /* 折行续行（RFC 5322）：以空白开头且已有当前头键时拼接到上一条 */
    if ((line[0] === " " || line[0] === "\t") && currentKey) {
      headers.set(currentKey, `${headers.get(currentKey)} ${line.trim()}`);
      continue;
    }
    const colon = line.indexOf(":");
    if (colon > 0) {
      currentKey = line.slice(0, colon).trim().toLowerCase();
      headers.set(currentKey, line.slice(colon + 1).trim());
    }
  }
  if (i > lines.length) {
    i = lines.length;
  }
  return { headers, body: lines.slice(i).join("\n") };
}

/** 按 boundary 切出各 part（已剥离边界标记与首部空行） */
function splitMultipart(body: string, boundary: string): string[] {
  const parts: string[] = [];
  for (let segment of body.split(`--${boundary}`)) {
    segment = segment.replace(/^\n/, "");
    segment = segment.replace(/--\n?$/, "");
    if (segment.trim()) {
      parts.push(segment);
    }
  }
  return parts;
}

/** 按 Content-Transfer-Encoding 解码 part 内容（base64 → quoted-printable → 原样） */
function decodePart(data: string, encoding: string): string {
  const enc = encoding.trim().toLowerCase();
  if (enc === "base64") {
    const joined = data.replace(/[\r\n\t ]/g, "");
    try {
      const bytes = Uint8Array.from(atob(joined), (c) => c.charCodeAt(0));
      return new TextDecoder("utf-8").decode(bytes).trim();
    } catch {
      return data;
    }
  }
  if (enc === "quoted-printable") {
    /* 软换行先合并，再解 =XX 十六进制转义 */
    const merged = data.replace(/=\r?\n/g, "");
    return merged.replace(/=([0-9A-Fa-f]{2})/g, (_, hexcode: string) =>
      String.fromCharCode(parseInt(hexcode, 16)),
    );
  }
  /* 7bit/8bit/binary：原样返回 */
  return data.trim();
}

/** 从整体原文抓取 <html>…</html> 片段（HTML 未正确声明时兜底） */
function guessHTML(body: string): string {
  if (!body) {
    return "";
  }
  const lower = body.toLowerCase();
  let start = lower.indexOf("<html");
  if (start === -1) {
    start = lower.indexOf("<!doctype html");
  }
  if (start === -1) {
    return "";
  }
  const end = lower.lastIndexOf("</html>");
  if (end === -1 || end < start) {
    return "";
  }
  return body.slice(start, end + 7);
}

/** 递归解析单个 MIME 实体 → (纯文本正文, HTML 正文) */
function parseEntity(
  headers: Map<string, string>,
  body: string,
): { text: string; html: string } {
  const contentTypeRaw = headers.get("content-type") || "";
  const contentType = contentTypeRaw.toLowerCase();
  const encoding = (headers.get("content-transfer-encoding") || "").toLowerCase();

  /* 单体：text/html 或 text/plain（无 Content-Type 时按纯文本） */
  if (!contentType.startsWith("multipart/")) {
    const decoded = decodePart(body, encoding);
    if (contentType.includes("text/html")) {
      return { text: "", html: decoded };
    }
    return { text: decoded, html: "" };
  }

  /* 复合：递归拆分，text 槽与 html 槽各自取第一个非空命中 */
  let text = "";
  let html = "";
  const boundary = boundaryOf(contentTypeRaw);
  if (boundary) {
    for (const part of splitMultipart(body, boundary)) {
      const { headers: partHeaders, body: partBody } = splitEML(`#participant\n${part}`, 1);
      const partType = (partHeaders.get("content-type") || "").toLowerCase();
      if (partType.startsWith("multipart/")) {
        const parsed = parseEntity(partHeaders, partBody);
        if (!text) text = parsed.text;
        if (!html) html = parsed.html;
      } else if (partType.startsWith("message/rfc822")) {
        /* 转发的原始邮件整体作为 part：递归整封解析 */
        const { headers: nestedHeaders, body: nestedBody } = splitEML(partBody, 0);
        const parsed = parseEntity(nestedHeaders, nestedBody);
        if (!text) text = parsed.text;
        if (!html) html = parsed.html;
      } else if (partType.includes("rfc822-headers")) {
        /* 纯头部 part 跳过，正文在后续 part 中抓取 */
        continue;
      } else {
        const parsed = parseEntity(partHeaders, partBody);
        if (!text) text = parsed.text;
        if (!html) html = parsed.html;
      }
      if (text && html) {
        break;
      }
    }
  }
  /* 无 HTML 命中时从整体原文兜底抓取 HTML 片段 */
  if (!html) {
    html = guessHTML(body);
  }
  return { text, html };
}

/** 解析 EML 原始报文 → {text, html} */
function parseEML(rawBytes: Uint8Array): { text: string; html: string } {
  const payload = new TextDecoder("utf-8").decode(rawBytes)
    .replace(/\r\n/g, "\n")
    .replace(/\r/g, "");
  const { headers, body } = splitEML(payload, 0);
  return parseEntity(headers, body);
}

/** 全文不可得时以 verification_code + preview 合成占位正文 */
function composePlaceholder(
  raw: Record<string, unknown>,
): string {
  const code =
    raw.verification_code === undefined || raw.verification_code === null
      ? ""
      : String(raw.verification_code).trim();
  const preview =
    raw.preview === undefined || raw.preview === null
      ? ""
      : String(raw.preview).trim();
  const parts: string[] = [];
  if (code) {
    parts.push(`验证码: ${code}`);
  }
  if (preview) {
    parts.push(preview);
  }
  return parts.join("\n\n");
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!token.startsWith(TOKEN_PREFIX)) {
    throw new Error("noxen-de5-net: token 格式错误");
  }
  let cookie = decodeURIComponent(token.slice(TOKEN_PREFIX.length));
  /* 剥离尾缀 "|base=<基址>"，仅保留会话 Cookie 键值对 */
  cookie = cookie.replace(/\|base=.*$/, "");
  if (!(await cookieStillValid(cookie))) {
    cookie = await login();
  }

  const listUrl = `${BASE_URL}/api/emails?mailbox=${encodeURIComponent(address)}&limit=20`;
  const response = await fetchWithTimeout(listUrl, {
    headers: {
      Accept: "application/json",
      "User-Agent": USER_AGENT,
      Cookie: cookie,
    },
  });
  const body = await response.text();
  if (response.status === 401) {
    throw new Error("noxen-de5-net 读信: http 401（会话失效或非本会话邮箱）");
  }
  if (!response.ok) {
    throw new Error(`noxen-de5-net 读信: http ${response.status}`);
  }

  const list = JSON.parse(body) as Array<Record<string, unknown>>;
  if (!Array.isArray(list)) {
    throw new Error("noxen-de5-net 读信: 响应非数组");
  }

  const out: Email[] = [];
  for (const raw of list) {
    const id = raw.id === undefined || raw.id === null ? "" : String(raw.id);
    const flat: Record<string, unknown> = { ...raw };
    flat.from = raw.sender;
    flat.to = address;
    flat.date = raw.received_at;
    flat.text = raw.preview;
    flat.isRead = raw.is_read;
    let full = false;
    if (id && id !== "0") {
      try {
        const detail = await fetchDetail(cookie, id);
        flat.content = detail.content;
        flat.html_content = detail.htmlContent;
        flat.to_addrs = detail.toAddrs;
        if (detail.download) {
          const emlBytes = await fetchEML(cookie, detail.download);
          if (emlBytes) {
            const { text, html } = parseEML(emlBytes);
            if (text || html) {
              flat.text = text;
              flat.html_content = html;
              full = true;
            }
          }
        }
      } catch {
        /* 详情失败按占位正文兜底 */
      }
      if (!full) {
        flat.text = composePlaceholder(raw);
      }
    }
    out.push(normalizeEmail(flat, address));
  }
  return out;
}