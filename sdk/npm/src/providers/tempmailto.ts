/**
 * Tempmailto 渠道实现（tempmailto.com，Laravel）
 *
 * 会话模型：邮箱由服务端会话 Cookie 承载（首页服务端渲染 #mainEmail），
 * 无独立密钥。npm 端无全局 Cookie 罐，本渠道以模块级私有 Cookie 存储
 * （仅本文件内共享）模拟浏览器 Cookie 会话：每次响应吸收 Set-Cookie 键值
 * （含 XSRF-TOKEN、temp_mail_to_session、locale、email 等），后续请求携带
 * Cookie 头并在响应 Set-Cookie 时覆写。
 * 建箱 = GET 首页 + 提取服务端渲染邮箱；Token 约定为邮箱本身
 * （注册表以 token 非空作防御）。邮箱约 10 分钟无活动过期。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { htmlToText } from "../html";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "tempmailto";
const BASE_URL = "https://tempmailto.com";

/* 浏览器形态 UA（与本端 tempmail-ee 渠道一致） */
const USER_AGENT =
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

/* 首页文本 HTML Accept（浏览器导航形态） */
const TEXT_HTML_ACCEPT =
  "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8";

/* 首页 mainEmail 渲染正则与 CSRF meta 正则 */
const MAIN_EMAIL_RE = /id="mainEmail"[^>]*\bvalue="([^"]+)"/i;
const CSRF_RE = /<meta\s+name="csrf-token"\s+content="([^"]+)"/i;

/* —— 模块级私有 Cookie 存储（仅本渠道文件内共享，含 Set-Cookie 轮换覆写） —— */
const cookieJar = new Map<string, string>();

/** 当前 Cookie 存储序列化为请求 Cookie 头 */
function cookieHeader(): string {
  return [...cookieJar.entries()].map(([k, v]) => `${k}=${v}`).join("; ");
}

/** 从响应 Set-Cookie 提取键值并覆写本渠道 Cookie 存储 */
function absorbCookies(response: Response): void {
  const setCookies = response.headers.getSetCookie
    ? response.headers.getSetCookie()
    : [];
  for (const line of setCookies) {
    const first = (line.split(";")[0] ?? "").trim();
    const eq = first.indexOf("=");
    if (eq <= 0) continue;
    const key = first.slice(0, eq).trim();
    const value = first.slice(eq + 1).trim();
    /* 撤回态（空值或 1970 过期）按浏览器语义移除键 */
    if (/expires\s*=\s*thu,\s*01\s+jan\s+1970/i.test(line) || value === "") {
      cookieJar.delete(key);
      continue;
    }
    cookieJar.set(key, value);
  }
}

/** 错误信息中的响应体摘要（截断防超长） */
function bodySnippet(raw: string): string {
  return raw.trim().slice(0, 200);
}

/** GET 首页浏览器头（同源 Origin/Referer） */
function pageHeaders(): Record<string, string> {
  return {
    "User-Agent": USER_AGENT,
    Accept: TEXT_HTML_ACCEPT,
    "Accept-Language": "en-US,en;q=0.9",
    Origin: BASE_URL,
    Referer: `${BASE_URL}/`,
  };
}

/** POST 表单 AJAX 头（同站同步请求形态） */
function ajaxHeaders(): Record<string, string> {
  return {
    "User-Agent": USER_AGENT,
    Accept: "application/json, text/plain, */*",
    "Accept-Language": "en-US,en;q=0.9",
    Origin: BASE_URL,
    Referer: `${BASE_URL}/`,
    "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8",
    "X-Requested-With": "XMLHttpRequest",
  };
}

/** 带 Cookie 存储的本渠道请求封装：请求携带 Cookie，响应吸收 Set-Cookie */
async function request(
  path: string,
  headers: Record<string, string>,
  body?: string,
): Promise<Response> {
  const finalHeaders = { ...headers };
  const cookie = cookieHeader();
  if (cookie) finalHeaders.Cookie = cookie;
  const response = await fetchWithTimeout(`${BASE_URL}${path}`, {
    method: body === undefined ? "GET" : "POST",
    headers: finalHeaders,
    ...(body === undefined ? {} : { body }),
    credentials: "omit",
  });
  absorbCookies(response);
  return response;
}

/**
 * GET 首页（建立/复用 Cookie 会话），返回 HTML；非 2xx 抛错。
 */
async function getPage(): Promise<string> {
  const response = await request("", pageHeaders());
  const body = await response.text();
  if (response.status < 200 || response.status >= 300) {
    throw new Error(
      `tempmailto: 首页 http ${response.status}: ${bodySnippet(body)}`,
    );
  }
  return body;
}

/** 从首页 HTML 提取 #mainEmail 的 value（服务端渲染的当前邮箱） */
function extractMailbox(page: string): string {
  const m = MAIN_EMAIL_RE.exec(page);
  return m ? m[1].trim() : "";
}

/**
 * 创建 tempmailto.com 临时邮箱。
 * 该平台邮箱由服务端会话 Cookie 承载，建箱即 GET 首页建立会话并提取
 * 服务端渲染的当前邮箱；Token 约定为邮箱本身（注册表以 token 非空作防御）。
 */
export async function generateEmail(): Promise<InternalEmailInfo> {
  const page = await getPage();
  const email = extractMailbox(page);
  if (!email) {
    throw new Error("tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱");
  }
  return {
    channel: CHANNEL,
    email,
    token: email,
    expiresAt: new Date(Date.now() + 10 * 60 * 1000).toISOString(),
  };
}

/** 拉取首页并提取 CSRF _token（读信/换箱共用，提取失败抛错） */
async function fetchCSRF(): Promise<string> {
  const page = await getPage();
  const m = CSRF_RE.exec(page);
  if (!m) {
    throw new Error("tempmailto: 首页未找到 csrf-token");
  }
  return m[1];
}

/** POST 表单（_token 等），返回响应与响应体文本 */
async function postForm(
  path: string,
  fields: Record<string, string>,
): Promise<{ response: Response; body: string }> {
  const body = new URLSearchParams(fields).toString();
  const response = await request(path, ajaxHeaders(), body);
  return { response, body: await response.text() };
}

/** tempmailtoMessagesResponse POST /get_messages（与 /change 同构）响应 */
interface MessagesResponse {
  status?: boolean;
  mailbox?: string;
  email_token?: string;
  messages?: Array<Record<string, unknown>>;
  histories?: Array<Record<string, unknown>>;
}

/**
 * POST /get_messages 拉取当前会话邮箱收件箱
 * （表单 _token=<csrf>、captcha 留空；非 2xx 报错）。
 */
async function fetchMessages(): Promise<MessagesResponse> {
  const csrf = await fetchCSRF();
  const { response, body } = await postForm("/get_messages", {
    _token: csrf,
    captcha: "",
  });
  if (response.status < 200 || response.status >= 300) {
    throw new Error(
      `tempmailto 读信: http ${response.status}: ${bodySnippet(body)}`,
    );
  }
  let data: MessagesResponse;
  try {
    data = JSON.parse(body) as MessagesResponse;
  } catch (error: any) {
    throw new Error(
      `tempmailto: 解析读信响应失败: ${error?.message || error}`,
    );
  }
  return data;
}

/**
 * POST /change 将会话切换到目标邮箱（name=@前部、domain=@后部，
 * 缺省 TmSdk / tempmailto.com），返回变更后的当前邮箱。
 */
async function changeTo(email: string): Promise<string> {
  const csrf = await fetchCSRF();
  const at = email.indexOf("@");
  const name = at > 0 ? email.slice(0, at) : "TmSdk";
  const domain =
    at >= 0 && at + 1 < email.length ? email.slice(at + 1) : "tempmailto.com";
  const { response, body } = await postForm("/change", {
    _token: csrf,
    name,
    domain,
  });
  let data: MessagesResponse;
  try {
    data = JSON.parse(body) as MessagesResponse;
  } catch (error: any) {
    throw new Error(
      `tempmailto: 解析 change 响应失败: ${error?.message || error}`,
    );
  }
  if (!data.mailbox) {
    throw new Error(`tempmailto: change 响应异常: ${bodySnippet(body)}`);
  }
  return data.mailbox;
}

/**
 * GET /view/{id} 提取邮件正文 HTML（同 Cookie 会话）。
 * 以候选 class 依次尝试提取块，全失败回退 <main>/<article> 区块；
 * 仍失败返回空串（列表归一不因此中断）。
 */
async function viewDetail(id: string): Promise<string> {
  const response = await request(
    `/view/${encodeURIComponent(id)}`,
    pageHeaders(),
  );
  if (response.status < 200 || response.status >= 300) {
    await response.text().catch(() => undefined);
    return "";
  }
  const page = await response.text();
  const candidates = [
    "mail-body",
    "mail_content",
    "email-body",
    "content-body",
    "message-content",
    "mail-content",
  ];
  for (const cls of candidates) {
    const re = new RegExp(
      `<[^>]+class="[^"]*\\b${cls}\\b[^"]*"[^>]*>([\\s\\S]*?)<\\/(?:div|section|article)>`,
      "i",
    );
    const m = re.exec(page);
    if (m && m[1].trim() !== "") return m[1].trim();
  }
  const m = /<(main|article)[^>]*>([\s\S]*?)<\/\1>/i.exec(page);
  if (m && m[2].trim() !== "") return m[2].trim();
  return "";
}

/**
 * 将 messages 列表元素组装为归一化邮件。
 * 正文来源：详情页 /view/{id}（同 Cookie 会话），提取失败回退列表字段；
 * id 空的行返回 null（跳过输出）。
 */
async function buildRow(
  row: Record<string, unknown>,
  email: string,
): Promise<Email | null> {
  /* 候选键安全字符串转换（数字转十进制，trim 后非空返回） */
  const str = (...keys: string[]): string => {
    for (const k of keys) {
      const v = row[k];
      if (v === undefined || v === null) continue;
      const s = String(v).trim();
      if (s !== "" && s !== "<nil>") return s;
    }
    return "";
  };

  const id = str("id");
  if (!id) return null;
  const fromEmail = str("from_email", "from");
  const fromName = str("from_name") || fromEmail;
  const subject = str("subject");
  let date = str("receivedAt", "received_at", "createdAt");
  if (!date) date = new Date().toISOString();

  /* is_seen 兼容 bool / number / string("1"/"true") */
  let isRead = false;
  const seen = row["is_seen"];
  if (typeof seen === "boolean") {
    isRead = seen;
  } else if (typeof seen === "number") {
    isRead = seen !== 0;
  } else if (typeof seen === "string") {
    isRead = seen === "1" || seen.toLowerCase() === "true";
  }

  let text = "";
  let html = "";
  const detailHtml = await viewDetail(id);
  if (detailHtml !== "") {
    html = detailHtml;
    text = htmlToText(detailHtml);
  }
  if (!text) {
    text = str("body", "text", "snippet", "preview");
    if (!text) text = subject;
  }
  if (!html) {
    const escaped = text
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;");
    html = `<html><body><pre>${escaped}</pre></body></html>`;
  }

  return normalizeEmail(
    { id, from: fromName, to: email, subject, text, html, date, isRead },
    email,
  );
}

/**
 * 读取当前会话邮箱收件箱。
 * 会话由本渠道 Cookie 存储粘住；读到的当前邮箱与请求邮箱不一致时用
 * change 拉回（change 内置重新取 CSRF）。
 */
export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address) {
    throw new Error("tempmailto: 邮箱为空，请重新 Generate");
  }

  let data = await fetchMessages();

  /* 会话当前邮箱与请求目标不一致 -> change 拉回（change 内置重新取 CSRF） */
  if (data.mailbox && data.mailbox.toLowerCase() !== address.toLowerCase()) {
    let changed: string;
    try {
      changed = await changeTo(address);
    } catch (error: any) {
      throw new Error(
        `tempmailto: 会话邮箱与请求不一致且拉起失败: ${error?.message || error}`,
      );
    }
    if (changed.toLowerCase() !== address.toLowerCase()) {
      throw new Error(
        `tempmailto: 会话邮箱无法拉回请求邮箱（${changed} != ${address}），请重新 Generate`,
      );
    }
    data = await fetchMessages();
  }

  const emails: Email[] = [];
  for (const row of Array.isArray(data.messages) ? data.messages : []) {
    const built = await buildRow(row, address);
    if (built) emails.push(built);
  }
  return emails;
}