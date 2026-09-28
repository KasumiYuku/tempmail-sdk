/**
 * Smails 渠道实现（smails.dev）
 * POST /api/mailbox 建箱（空 JSON body，响应 address/token），
 * GET /api/mailbox/messages 读信（Header Authorization: Bearer <token>，数组响应），
 * GET /api/mailbox/messages/{id} 取单封详情（Bearer）。
 * 列表元素按 from/subject/text/html/date 多候选归一，详情失败时回退为列表摘要。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "smails";
const BASE_URL = "https://smails.dev";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

/** 认证请求头（token 非空时附带 Bearer） */
function authHeaders(token: string): Record<string, string> {
  const headers: Record<string, string> = {
    Accept: "application/json",
    "User-Agent": USER_AGENT,
  };
  if (token) headers.Authorization = `Bearer ${token}`;
  return headers;
}

interface GenerateResponse {
  address?: string;
  token?: string;
}

/** 从列表/详情对象中提取邮件 ID，候选字段 id/Id/slug/messageId/message_id */
function messageIDOf(raw: Record<string, unknown>): string {
  for (const key of ["id", "Id", "slug", "messageId", "message_id"]) {
    const value = raw[key];
    if (typeof value === "string" && value.trim() !== "") return value;
  }
  return "";
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const response = await fetchWithTimeout(`${BASE_URL}/api/mailbox`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...authHeaders(""),
    },
    body: "{}",
  });
  if (!response.ok) {
    throw new Error(`smails: 创建邮箱失败 http ${response.status}`);
  }

  const data = (await response.json()) as GenerateResponse;
  const email = String(data.address || "").trim();
  const token = String(data.token || "").trim();
  if (!email || !token) {
    throw new Error("smails: 创建邮箱响应缺少必要字段");
  }

  return { channel: CHANNEL, email, token };
}

/** 拉取单封详情；详情失败时返回 null（由调用方回退为列表摘要） */
async function fetchDetail(
  token: string,
  messageId: string,
): Promise<Record<string, unknown> | null> {
  try {
    const response = await fetchWithTimeout(
      `${BASE_URL}/api/mailbox/messages/${encodeURIComponent(messageId)}`,
      { headers: authHeaders(token) },
    );
    if (!response.ok) return null;
    return (await response.json()) as Record<string, unknown>;
  } catch {
    return null;
  }
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const authToken = String(token || "").trim();

  const response = await fetchWithTimeout(`${BASE_URL}/api/mailbox/messages`, {
    headers: authHeaders(authToken),
  });
  if (!response.ok) {
    throw new Error(`smails: 获取邮件列表失败 http ${response.status}`);
  }

  const list = (await response.json()) as Array<Record<string, unknown>>;
  if (!Array.isArray(list)) {
    throw new Error("smails: 邮件列表响应格式异常");
  }

  const emails: Email[] = [];
  for (const raw of list) {
    const id = messageIDOf(raw);
    let merged = raw;
    if (id) {
      const detail = await fetchDetail(authToken, id);
      if (detail) {
        /* 列表字段优先，详情补齐缺失字段 */
        merged = { ...detail, ...raw };
      }
    }
    emails.push(normalizeEmail(merged, address));
  }
  return emails;
}