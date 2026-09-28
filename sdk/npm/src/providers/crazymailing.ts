/**
 * Crazymailing 渠道实现（crazymailing.com）
 * Next.js 全栈站点，API 结构：
 * - 建箱 POST /api/mailbox（空 JSON body），响应 {"mailbox":{"id","address","expiresAt"}}。
 * - 读信 GET /api/messages?mailbox=<完整地址>，响应 {"messages":[...]}。
 * - 单封正文 GET /api/message/{id}/body，响应为完整 HTML 页面。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "crazymailing";
const BASE_URL = "https://crazymailing.com";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface MailboxResponse {
  mailbox?: {
    id?: string;
    address?: string;
    expiresAt?: string;
  };
}

interface MessagesResponse {
  messages?: Array<Record<string, unknown>>;
}

/** 提取列表元素的邮件 ID（多候选键） */
function messageIdOf(raw: Record<string, unknown>): string {
  for (const key of ["id", "_id", "messageId", "message_id", "slug"]) {
    const value = raw[key];
    if (value !== undefined && value !== null && String(value).trim() !== "") {
      return String(value).trim();
    }
  }
  return "";
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const response = await fetchWithTimeout(`${BASE_URL}/api/mailbox`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      Origin: BASE_URL,
      Referer: `${BASE_URL}/`,
      "User-Agent": USER_AGENT,
    },
    body: "{}",
  });
  if (!response.ok) {
    const body = await response.text();
    throw new Error(`crazymailing: 创建邮箱失败 http ${response.status}: ${body}`);
  }

  const data = (await response.json()) as MailboxResponse;
  const mailbox = data.mailbox;
  const email = String(mailbox?.address || "").trim();
  if (!mailbox || !email) {
    throw new Error("crazymailing: 创建响应缺少 mailbox.address");
  }

  return {
    channel: CHANNEL,
    email,
    token: String(mailbox.id || "").trim(),
    expiresAt: mailbox.expiresAt,
  };
}

/** 拉取单封正文（响应为完整 HTML 页面），失败返回空串 */
async function fetchBody(id: string): Promise<string> {
  try {
    const response = await fetchWithTimeout(
      `${BASE_URL}/api/message/${encodeURIComponent(id)}/body`,
      {
        headers: {
          Accept: "text/html,application/xhtml+xml,*/*;q=0.8",
          Origin: BASE_URL,
          Referer: `${BASE_URL}/`,
          "User-Agent": USER_AGENT,
        },
      },
    );
    if (!response.ok) {
      return "";
    }
    const text = await response.text();
    return text.trim();
  } catch {
    return "";
  }
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address) {
    throw new Error("crazymailing: 邮箱地址为空");
  }

  const response = await fetchWithTimeout(
    `${BASE_URL}/api/messages?mailbox=${encodeURIComponent(address)}`,
    {
      headers: {
        Accept: "application/json",
        Origin: BASE_URL,
        Referer: `${BASE_URL}/`,
        "User-Agent": USER_AGENT,
      },
    },
  );
  if (!response.ok) {
    const body = await response.text();
    throw new Error(`crazymailing: 读取收件箱失败 http ${response.status}: ${body}`);
  }

  const data = (await response.json()) as MessagesResponse;
  const messages = Array.isArray(data.messages) ? data.messages : [];

  const out: Email[] = [];
  for (const raw of messages) {
    const flat: Record<string, unknown> = { ...raw };
    if (flat.to === undefined || flat.to === null) {
      flat.to = address;
    }
    /* 正文须逐封二拉；详情失败时以列表摘要归一，不阻断列表 */
    const id = messageIdOf(raw);
    if (id) {
      const html = await fetchBody(id);
      if (html) {
        flat.html = html;
      }
    }
    out.push(normalizeEmail(flat, address));
  }
  return out;
}