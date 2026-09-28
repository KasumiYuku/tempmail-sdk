/**
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
 * 地址最长周期固定为 4 小时。
 *
 * 403 风控说明（2026-09-28 实测）：
 *   /api/ 请求铺浏览器形态头（UA + Referer + Origin + Sec-Fetch 系列），
 *   403 时换备用浏览器 UA 重试一次（建箱/读信/详情均覆盖）；
 *   429 平台限流报错交给 SDK 外层退避重试（checkInbox 调用间隔≥15s）。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";
import { decodeHtmlEntitiesOnce } from "../html";

const CHANNEL: Channel = "temporarymail-com";
const BASE_URL = "https://temporarymail.com";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";
/** 备用浏览器 UA（403 重试用） */
const ALT_USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36";

interface GenerateResponse {
  address?: string;
  secretKey?: string;
}

interface DetailFields {
  subject?: string;
  from?: string;
  name?: string;
  date?: number;
  source?: string;
  sourceHash?: string;
}

/** 构造 /api/ 请求头（浏览器形态头部集，规避脚本判别收紧） */
function apiHeaders(userAgent: string): Record<string, string> {
  return {
    Accept: "application/json, text/plain, */*",
    "Accept-Language": "en-US,en;q=0.9",
    "Sec-Fetch-Site": "same-origin",
    "Sec-Fetch-Mode": "cors",
    "Sec-Fetch-Dest": "empty",
    Referer: `${BASE_URL}/`,
    Origin: BASE_URL,
    "User-Agent": userAgent,
  };
}

/** 任意 JSON 值规整为字符串（nil→空串） */
function strOf(value: unknown): string {
  if (value === undefined || value === null) {
    return "";
  }
  return String(value);
}

/** 拉取 checkInbox 响应体（403 换备用 UA 重试一次，429 报限流错误） */
async function checkInbox(token: string): Promise<string> {
  const userAgents = [USER_AGENT, ALT_USER_AGENT];
  for (const userAgent of userAgents) {
    const response = await fetchWithTimeout(
      `${BASE_URL}/api/?action=checkInbox&value=${encodeURIComponent(token)}`,
      {
        headers: apiHeaders(userAgent),
      },
    );
    const body = await response.text();
    if (response.status === 429) {
      const retryAfter = response.headers.get("Retry-After") || "";
      throw new Error(`temporarymail: 读取收件箱平台限流(429 Retry-After=${retryAfter})，请拉大轮询间隔`);
    }
    if (response.ok) {
      return body;
    }
    /* 403/404 疑似 UA 键控风控，换备用 UA 重试一次 */
    if (response.status !== 403 && response.status !== 404) {
      throw new Error(`temporarymail: 读取收件箱失败 http ${response.status}: ${body}`);
    }
  }
  throw new Error("temporarymail: 读取收件箱失败 http 403（两次尝试均被拒）");
}

/** 拉取单封邮件详情（POST getEmail），失败返回 null（列表元数据兜底） */
async function fetchDetail(
  id: string,
  userAgent: string,
): Promise<DetailFields | null> {
  try {
    const response = await fetchWithTimeout(
      `${BASE_URL}/api/?action=getEmail&value=${encodeURIComponent(id)}`,
      {
        method: "POST",
        headers: apiHeaders(userAgent),
      },
    );
    const detail = await fetchDetailParse(response).catch(() => null);
    if (!detail) {
      return null;
    }
    /* 429/非 2xx/非 JSON 均为风控或限流信号，返回 null 走降级兜底 */
    const flat = detail as DetailFields;
    if (
      flat.subject === undefined &&
      flat.from === undefined &&
      flat.name === undefined &&
      flat.date === undefined &&
      flat.source === undefined &&
      flat.sourceHash === undefined
    ) {
      return null;
    }
    return flat;
  } catch {
    return null;
  }
}

/** 解析单封邮件详情响应：2xx 且为 {id: {...}} 单元素对象时取第一元素 */
async function fetchDetailParse(
  response: Response,
): Promise<DetailFields | null> {
  const contentType = response.headers.get("content-type") || "";
  if (!response.ok || !contentType.includes("json")) {
    return null;
  }
  const data = (await response.json()) as Record<string, unknown>;
  for (const value of Object.values(data)) {
    if (value && typeof value === "object" && !Array.isArray(value)) {
      return value as DetailFields;
    }
  }
  return null;
}

/** 抓取 /view/ 渲染端点全文并剥标签还原纯文本 */
async function fetchViewText(id: string): Promise<string> {
  try {
    const response = await fetchWithTimeout(
      `${BASE_URL}/view/?i=${encodeURIComponent(id)}&width=800`,
      {
        headers: {
          Accept: "text/html, */*",
          "User-Agent": USER_AGENT,
          Referer: `${BASE_URL}/`,
        },
      },
    );
    if (!response.ok) {
      return "";
    }
    return viewToText(await response.text());
  } catch {
    return "";
  }
}

/** 反转义 HTML 实体（借用 html.ts 的单次解码，二次回转常见命名实体） */
function unescapeFull(source: string): string {
  return decodeHtmlEntitiesOnce(source)
    .replace(/&quot;/gi, '"')
    .replace(/&#39;|&apos;/gi, "'");
}

/** 将 /view/ 响应剥标签还原为纯文本（<br>/<p> 换行保留） */
function viewToText(source: string): string {
  let text = source
    .replace(/<br\s*\/?>/gi, "\n")
    .replace(/<\/?p[^>]*>/gi, "\n")
    .replace(/<script[\s\S]*?<\/script>/gi, " ")
    .replace(/<style[\s\S]*?<\/style>/gi, " ")
    .replace(/<[^>]+>/g, " ")
    .replace(/&nbsp;/gi, " ");
  text = unescapeFull(text);
  return text
    .split("\n")
    .map((line) => line.trim())
    .join("\n")
    .trim();
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  let lastErrorText = "";
  /* 403 疑似风控：换备用浏览器 UA 重试一次 */
  for (const userAgent of [USER_AGENT, ALT_USER_AGENT]) {
    const response = await fetchWithTimeout(
      `${BASE_URL}/api/?action=requestEmailAccess&key=&value=random`,
      {
        headers: apiHeaders(userAgent),
      },
    );
    const body = await response.text();
    if (response.status === 429) {
      const retryAfter = response.headers.get("Retry-After") || "";
      throw new Error(`temporarymail: 创建邮箱平台限流(429 Retry-After=${retryAfter})，请稍后重试: ${body}`);
    }
    if (!response.ok) {
      lastErrorText = body;
      if (response.status !== 403 && response.status !== 404) {
        throw new Error(`temporarymail: 创建邮箱失败 http ${response.status}: ${body}`);
      }
      continue;
    }
    const data = JSON.parse(body) as GenerateResponse;
    const address = String(data.address || "").trim();
    const secretKey = String(data.secretKey || "").trim();
    if (!address || !secretKey) {
      throw new Error(`temporarymail: 创建响应缺少 address 或 secretKey: ${body}`);
    }

    return {
      channel: CHANNEL,
      email: address,
      token: secretKey,
    };
  }
  throw new Error(`temporarymail: 创建邮箱失败 http 403（两次尝试均被拒）${lastErrorText ? `: ${lastErrorText}` : ""}`);
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const secretKey = String(token || "").trim();

  const rawBody = await checkInbox(secretKey);

  /* 平台响应两种合法形态：空箱 []，有信为 map[id]→对象 */
  let list: Array<Record<string, unknown>> = [];
  const parsed = JSON.parse(rawBody);
  if (Array.isArray(parsed)) {
    list = parsed as Array<Record<string, unknown>>;
  } else if (parsed && typeof parsed === "object") {
    list = Object.values(parsed) as Array<Record<string, unknown>>;
  }

  const out: Email[] = [];
  for (const raw of list) {
    const flat: Record<string, unknown> = { ...raw };
    /* 列表元素无 to 字段，注入收件人地址以归一化 */
    if (flat.to === undefined || flat.to === null) {
      flat.to = address;
    }
    const id = strOf(raw.id);
    if (id) {
      /* 详情覆盖真实主题（失败不致命：列表元数据兜底；403/429 换备用浏览器 UA 重试一次） */
      const detail =
        (await fetchDetail(id, USER_AGENT)) ??
        (await fetchDetail(id, ALT_USER_AGENT));
      if (detail) {
        if (detail.subject) {
          flat.subject = detail.subject;
        }
        if (detail.from) {
          flat.from = detail.from;
        }
      }
      /* /view/ 渲染端点全文（失败不致命：列表元数据兜底） */
      const text = await fetchViewText(id);
      if (text) {
        flat.text = text;
      }
    }
    out.push(normalizeEmail(flat, address));
  }
  return out;
}