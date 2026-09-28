/**
 * Nullmail 渠道实现（nullmail.cc / maildock.store）
 * 无认证 REST：POST /api/emails（空 JSON body）建箱，响应
 * {"address":"...@maildock.store","expiry":"..."}；
 * 读信 GET /api/emails/{address}（URL 编码），响应 {"expiry":"...","emails":[...]}，
 * 列表项只有 id/sender/subject/delivered，正文须逐封二拉 GET /api/emails/{addr}/body/{id}
 * （响应 {"body":...}），二拉失败降级留空不阻断列表。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "nullmail";
const BASE_URL = "https://www.nullmail.cc";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

/** 站点防爬请求头（Origin/Referer 必带） */
const SITE_HEADERS: Record<string, string> = {
  Accept: "application/json",
  Origin: BASE_URL,
  Referer: `${BASE_URL}/`,
  "User-Agent": USER_AGENT,
};

interface GenerateResponse {
  address?: string;
  expiry?: string;
}

interface InboxResponse {
  expiry?: string;
  emails?: Array<Record<string, unknown>>;
}

interface BodyResponse {
  body?: string;
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const response = await fetchWithTimeout(`${BASE_URL}/api/emails`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...SITE_HEADERS,
    },
    body: "{}",
  });
  if (!response.ok) {
    throw new Error(`nullmail 建箱: http ${response.status}`);
  }

  const data = (await response.json()) as GenerateResponse;
  const address = String(data.address || "").trim();
  if (!address) {
    throw new Error("nullmail 建箱: 响应缺少 address 字段");
  }

  /* token 复用完整地址以便收件箱回查 */
  return {
    channel: CHANNEL,
    email: address,
    token: address,
    expiresAt: data.expiry,
  };
}

/** 单封正文二拉；失败返回空字符串 */
async function fetchBody(address: string, id: unknown): Promise<string> {
  try {
    const response = await fetchWithTimeout(
      `${BASE_URL}/api/emails/${encodeURIComponent(address)}` +
        `/body/${encodeURIComponent(String(id))}`,
      { headers: SITE_HEADERS },
    );
    if (!response.ok) return "";
    const data = (await response.json()) as BodyResponse;
    return data.body || "";
  } catch {
    return "";
  }
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address) {
    throw new Error("nullmail 读信: 邮箱地址为空");
  }

  const response = await fetchWithTimeout(
    `${BASE_URL}/api/emails/${encodeURIComponent(address)}`,
    { headers: SITE_HEADERS },
  );
  if (!response.ok) {
    throw new Error(`nullmail 读信: http ${response.status}`);
  }

  const data = (await response.json()) as InboxResponse;
  const rows = Array.isArray(data.emails) ? data.emails : [];

  const emails: Email[] = [];
  for (const raw of rows) {
    /* normalizeDate 候选键不含 delivered，显式映射为 date 后归一化 */
    const normalized: Record<string, unknown> = {
      id: raw.id,
      from: raw.sender,
      to: address,
      subject: raw.subject,
      date: raw.delivered,
    };
    if (raw.id !== undefined && raw.id !== null) {
      const body = await fetchBody(address, raw.id);
      if (body) normalized.text = body;
    }
    emails.push(normalizeEmail(normalized, address));
  }
  return emails;
}