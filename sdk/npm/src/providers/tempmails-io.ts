/**
 * TempmailsIo 渠道实现（tempmails.io）
 * 无认证 REST：POST /api/temp-mail/generate 建箱（响应 email/token/expires_at），
 * 读信必须先 POST /api/temp-mail/fetch-emails/{token} 触发平台对上游信箱的主动同步，
 * 再 GET /api/temp-mail/inbox/{token} 读取（messages[] 含 from_email/text_body/html_body/attachments）。
 * 邮箱借用 uberip.com 等公共域（10 分钟自动过期）。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "tempmails-io";
const BASE_URL = "https://tempmails.io";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface GenerateResponse {
  success?: boolean;
  data?: {
    email?: string;
    token?: string;
    expires_at?: string;
    minutes_remaining?: number;
  };
}

interface InboxResponse {
  success?: boolean;
  data?: {
    email?: string;
    expires_at?: string;
    message_count?: number;
    messages?: Array<Record<string, unknown>>;
  };
}

/** 从标准响应中提取原始字段（多候选键取名，缺失时透传） */
function pickStr(
  raw: Record<string, unknown>,
  ...keys: string[]
): string | undefined {
  for (const key of keys) {
    const value = raw[key];
    if (typeof value === "string" && value !== "") return value;
    if (value !== undefined && value !== null) return String(value);
  }
  return undefined;
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const response = await fetchWithTimeout(
    `${BASE_URL}/api/temp-mail/generate`,
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
    throw new Error(`tempmails-io generate: http ${response.status}`);
  }

  const data = (await response.json()) as GenerateResponse;
  const email = String(data.data?.email || "").trim();
  const token = String(data.data?.token || "").trim();
  if (!data.success || !email || !token) {
    throw new Error("tempmails-io generate: missing email or token");
  }

  return {
    channel: CHANNEL,
    email,
    token,
    expiresAt: data.data?.expires_at,
  };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const authToken = String(token || "").trim();

  /*
   * 必须先 POST fetch-emails 触发平台对上游信箱（uberip.com 域，mail.tm 别名）
   * 的主动同步；该请求失败不致命，仍继续尝试静态读取收件箱。
   */
  try {
    const syncResponse = await fetchWithTimeout(
      `${BASE_URL}/api/temp-mail/fetch-emails/${encodeURIComponent(authToken)}`,
      {
        method: "POST",
        headers: {
          Accept: "application/json",
          "User-Agent": USER_AGENT,
        },
      },
    );
    await syncResponse.text();
  } catch {
    /* 同步请求失败不阻断读信 */
  }

  const response = await fetchWithTimeout(
    `${BASE_URL}/api/temp-mail/inbox/${encodeURIComponent(authToken)}`,
    {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
      },
    },
  );
  if (!response.ok) {
    throw new Error(`tempmails-io inbox: http ${response.status}`);
  }

  const data = (await response.json()) as InboxResponse;
  const messages = Array.isArray(data.data?.messages) ? data.data.messages : [];

  return messages.map((raw) =>
    normalizeEmail(
      {
        id: raw.id,
        from: pickStr(raw, "from_email"),
        to: raw.to || address,
        subject: pickStr(raw, "subject"),
        text: pickStr(raw, "text_body"),
        html: pickStr(raw, "html_body"),
        date: pickStr(raw, "received_at"),
        attachments: raw.attachments,
      },
      address,
    ),
  );
}