/**
 * Flybymail 渠道实现（flybymail.com）
 * POST /api/recipients 建箱（空 JSON body，响应 id/email/createdAt/expiresAt），
 * GET /api/recipients/{email}/emails 读信（按邮箱地址查询，响应 {"emails":[...]}）。
 * 信件字段：id/from/to/subject/body（纯文本）/htmlBody（HTML 正文）/preview/time/read。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "flybymail";
const BASE_URL = "https://flybymail.com";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface GenerateResponse {
  id?: string;
  email?: string;
  createdAt?: number;
  expiresAt?: number;
}

interface InboxResponse {
  emails?: Array<Record<string, unknown>>;
}

/** 从邮件元素提取候选键的首个非空字符串（数字转十进制字符串） */
function anyString(raw: Record<string, unknown>, ...keys: string[]): string {
  for (const key of keys) {
    const value = raw[key];
    if (typeof value === "string" && value.trim()) {
      return value.trim();
    }
    if (typeof value === "number") {
      return String(Math.trunc(value));
    }
  }
  return "";
}

/** 归一邮件时间：候选 time/date 字段（time 为毫秒时间戳） */
function timestampOf(raw: Record<string, unknown>): unknown {
  if (raw.time !== undefined && raw.time !== null) {
    return raw.time;
  }
  if (raw.date !== undefined && raw.date !== null) {
    return raw.date;
  }
  return undefined;
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const response = await fetchWithTimeout(`${BASE_URL}/api/recipients`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
    body: "{}",
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`flybymail: 创建邮箱失败 http ${response.status}: ${body}`);
  }
  const data = JSON.parse(body) as GenerateResponse;
  if (!data.id || !data.email || !data.email.includes("@")) {
    throw new Error(`flybymail: 创建邮箱响应缺少必要字段: ${body}`);
  }

  /* expiresAt 为毫秒时间戳，转换为秒供统一展示 */
  const expiresAt = data.expiresAt && data.expiresAt > 0
    ? String(Math.trunc(data.expiresAt / 1000))
    : undefined;

  return {
    channel: CHANNEL,
    email: data.email,
    token: data.id,
    expiresAt,
  };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address || !address.includes("@")) {
    throw new Error("flybymail: 邮箱地址为空或格式错误");
  }

  const response = await fetchWithTimeout(
    `${BASE_URL}/api/recipients/${encodeURIComponent(address)}/emails`,
    {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
      },
    },
  );
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`flybymail: 获取邮件列表失败 http ${response.status}: ${body}`);
  }
  const data = JSON.parse(body) as InboxResponse;
  const emails = Array.isArray(data.emails) ? data.emails : [];

  return emails.map((raw) => {
    const entry: Record<string, unknown> = {
      from: raw.from,
      to: raw.to,
      subject: raw.subject,
      text: raw.body,
      html: raw.htmlBody,
      time: raw.time,
      read: raw.read,
      attachments: raw.attachments,
    };
    /* id 为数字时转字符串保持一致，time 为毫秒时间戳时按 timestamp 归一 */
    entry.id = anyString(raw, "id");
    const ts = timestampOf(raw);
    if (ts !== undefined) {
      entry.timestamp = ts;
    }
    return normalizeEmail(entry, address);
  });
}