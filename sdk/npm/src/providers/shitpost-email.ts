/**
 * ShitpostEmail 渠道实现（shitpost.email 公共实例）
 * 无认证 REST：POST /api/create 建箱（username/domain/ttl → email/token/type/expires），
 * GET /api/inbox?email=&token= 读信（messages[] 含 from/fromName/subject/text/html/date）。
 * 域名池：shitpost.email / letsfuckingpiss.party（克隆自 shamu4life/throwaway-email 公共实例）。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "shitpost-email";
const BASE_URL = "https://shitpost.email";

const DOMAINS = ["shitpost.email", "letsfuckingpiss.party"];

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

const LOCAL_CHARS = "abcdefghijklmnopqrstuvwxyz0123456789";

/** 生成 "sdk"+10 位随机字符的建箱用户名 */
function localUsername(): string {
  let name = "sdk";
  for (let i = 0; i < 10; i++) {
    name += LOCAL_CHARS[Math.floor(Math.random() * LOCAL_CHARS.length)];
  }
  return name;
}

interface CreateResponse {
  email?: string;
  token?: string;
  type?: string;
  expires?: number;
}

interface InboxResponse {
  email?: string;
  messages?: Array<Record<string, unknown>>;
  count?: number;
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const domain = DOMAINS[Math.floor(Math.random() * DOMAINS.length)];
  const response = await fetchWithTimeout(`${BASE_URL}/api/create`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
    body: JSON.stringify({ username: localUsername(), domain, ttl: 3600 }),
  });
  if (!response.ok) {
    throw new Error(`shitpost-email create: http ${response.status}`);
  }

  const data = (await response.json()) as CreateResponse;
  const email = String(data.email || "").trim();
  const token = String(data.token || "").trim();
  if (!email || !token) {
    throw new Error("shitpost-email create: missing email or token");
  }

  return {
    channel: CHANNEL,
    email,
    token,
    expiresAt: data.expires !== undefined ? String(data.expires) : undefined,
  };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const authToken = String(token || "").trim();

  const url =
    `${BASE_URL}/api/inbox?email=${encodeURIComponent(address)}` +
    `&token=${encodeURIComponent(authToken)}`;
  const response = await fetchWithTimeout(url, {
    headers: {
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
  });
  if (!response.ok) {
    throw new Error(`shitpost-email inbox: http ${response.status}`);
  }

  const data = (await response.json()) as InboxResponse;
  const messages = Array.isArray(data.messages) ? data.messages : [];

  return messages.map((raw) =>
    normalizeEmail(
      {
        id: raw.id,
        from: raw.from,
        to: raw.to || address,
        subject: raw.subject,
        text: raw.text,
        html: raw.html,
        date: raw.date,
      },
      address,
    ),
  );
}