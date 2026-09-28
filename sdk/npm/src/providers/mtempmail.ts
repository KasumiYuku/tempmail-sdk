/**
 * Mtempmail 渠道实现（mtempmail.com，公共 key 认证）
 * 建箱: POST /api/emails/{apiKey}（body {}）→
 *   {"status":true,"data":{"email":"xxx@domain","domain":"..","expire_at":"..",
 *    "created_at":"..","id":213000,"email_token":"..."}}
 * 读信: GET /api/messages/{apiKey}/{email} →
 *   {"status":true,"mailbox":"..","email_token":"..","messages":[]}
 *   消息列表元素为 mailgun 入站 webhook 风格：
 *   {"to":[...],"body":[{content_type:"text/html",value:".."}],
 *    "created_at":"..","id":123,"from":[{"full":"Sender <a@b.com>"}],"subject":".."}。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "mtempmail";
const BASE_URL = "https://mtempmail.com";

/** 公共固定 API key（mtempmail.com 官方提供） */
const PUBLIC_KEY = "pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface CreateResponse {
  status?: boolean;
  data?: {
    email?: string;
    domain?: string;
    expire_at?: string;
    created_at?: string;
    id?: number;
    email_token?: string;
  };
}

interface MessagesResponse {
  status?: boolean;
  mailbox?: string;
  email_token?: string;
  messages?: Array<Record<string, unknown>>;
}

/** 剥离后台拼接的 "• " 前缀（或实收分隔符），注意首字符为 U+2022 或 U+00B7 */
function cleanSubject(subject: string): string {
  return subject
    .trim()
    .replace(/^[•·]+\s*/, "");
}

/** 拼接正文纯文本（body[].value 按序） */
function bodyText(parts: Array<Record<string, unknown>>): string {
  let text = "";
  for (const part of parts) {
    if (typeof part.value === "string") {
      text += part.value + "\n";
    }
  }
  return text.replace(/\n+$/, "");
}

/** 提取首个 text/html 段 */
function bodyHtml(parts: Array<Record<string, unknown>>): string {
  for (const part of parts) {
    if (part.content_type === "text/html" && typeof part.value === "string") {
      return part.value;
    }
  }
  return "";
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const response = await fetchWithTimeout(
    `${BASE_URL}/api/emails/${encodeURIComponent(PUBLIC_KEY)}`,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json",
        "User-Agent": USER_AGENT,
      },
      body: "{}",
    },
  );
  if (!response.ok) {
    throw new Error(`mtempmail create: http ${response.status}`);
  }

  const data = (await response.json()) as CreateResponse;
  const email = String(data.data?.email || "").trim();
  if (!data.status || !email) {
    throw new Error("mtempmail create: 响应缺少邮箱");
  }

  return {
    channel: CHANNEL,
    email,
    token: data.data?.email_token,
    expiresAt: data.data?.expire_at,
    createdAt: data.data?.created_at,
  };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address) {
    throw new Error("mtempmail: 邮箱为空");
  }
  if (!String(token || "").trim()) {
    throw new Error("mtempmail: token 为空");
  }

  const response = await fetchWithTimeout(
    `${BASE_URL}/api/messages/${encodeURIComponent(PUBLIC_KEY)}` +
      `/${encodeURIComponent(address)}`,
    {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
      },
    },
  );
  if (!response.ok) {
    throw new Error(`mtempmail inbox: http ${response.status}`);
  }

  const data = (await response.json()) as MessagesResponse;
  const rows = Array.isArray(data.messages) ? data.messages : [];

  return rows.map((raw) => {
    const bodyParts = Array.isArray(raw.body)
      ? (raw.body as Array<Record<string, unknown>>)
      : [];
    return normalizeEmail(
      {
        id: raw.id,
        from: raw.from,
        to: address,
        subject:
          typeof raw.subject === "string" ? cleanSubject(raw.subject) : raw.subject,
        text: bodyText(bodyParts),
        html: bodyHtml(bodyParts),
        date: raw.created_at,
      },
      address,
    );
  });
}