/**
 * ClawdEmail 渠道实现（clawdemail.com，API 域 api.clawdemail.com）
 * POST /register 建箱（可选 name，响应 success/email/token），
 * GET /inbox?limit=50 读信列表（Header Authorization: Bearer <token>，响应 success/email/count/unread/emails[]），
 * GET /email/{id} 取单封详情（Bearer，响应含 email 嵌套对象 from_addr/subject/body_text/received_at）。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "clawdemail";
const BASE_URL = "https://api.clawdemail.com";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface RegisterResponse {
  success?: boolean;
  email?: string;
  token?: string;
  error?: string;
}

interface InboxResponse {
  success?: boolean;
  email?: string;
  count?: number;
  unread?: number;
  emails?: Array<Record<string, unknown>>;
  error?: string;
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
  const response = await fetchWithTimeout(`${BASE_URL}/register`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
    body: JSON.stringify({ name: "" }),
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`clawdemail: 创建邮箱失败 http ${response.status}: ${body}`);
  }
  const data = JSON.parse(body) as RegisterResponse;
  const email = String(data.email || "").trim();
  const token = String(data.token || "").trim();
  if (!email || !token || !email.includes("@")) {
    throw new Error(`clawdemail: 创建邮箱响应缺少必要字段: ${body}`);
  }

  return {
    channel: CHANNEL,
    email,
    token,
  };
}

/** 获取单封邮件详情（Bearer token），响应含 email 嵌套对象时提升之 */
async function fetchDetail(
  token: string,
  messageId: string,
): Promise<Record<string, unknown> | null> {
  const baseId = messageId.split("/").pop() || "";
  try {
    const response = await fetchWithTimeout(
      `${BASE_URL}/email/${encodeURIComponent(baseId)}`,
      {
        headers: {
          Accept: "application/json",
          "User-Agent": USER_AGENT,
          Authorization: `Bearer ${token}`,
        },
      },
    );
    if (!response.ok) {
      return null;
    }
    const detail = (await response.json()) as Record<string, unknown>;
    const nested = detail.email;
    if (nested && typeof nested === "object" && !Array.isArray(nested)) {
      return nested as Record<string, unknown>;
    }
    return detail;
  } catch {
    return null;
  }
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const authToken = String(token || "").trim();

  const response = await fetchWithTimeout(`${BASE_URL}/inbox?limit=50`, {
    headers: {
      Accept: "application/json",
      "User-Agent": USER_AGENT,
      Authorization: `Bearer ${authToken}`,
    },
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`clawdemail: 获取邮件列表失败 http ${response.status}: ${body}`);
  }
  const data = JSON.parse(body) as InboxResponse;
  if (!data.success) {
    throw new Error(`clawdemail: 读取收件箱失败: ${data.error}`);
  }
  const emails = Array.isArray(data.emails) ? data.emails : [];

  const out: Email[] = [];
  for (const raw of emails) {
    const id = messageIdOf(raw);
    if (!id) {
      out.push(normalizeEmail(raw, address));
      continue;
    }
    const detail = await fetchDetail(authToken, id);
    if (!detail) {
      /* 详情失败时回退为列表摘要 */
      out.push(normalizeEmail(raw, address));
      continue;
    }
    /* 详情仅补充列表缺失字段 */
    const merged: Record<string, unknown> = { ...raw };
    for (const [key, value] of Object.entries(detail)) {
      if (merged[key] === undefined) {
        merged[key] = value;
      }
    }
    out.push(normalizeEmail(merged, address));
  }
  return out;
}