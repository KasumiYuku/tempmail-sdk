/**
 * NowtempMail 渠道实现（nowtempmail.com）
 * POST /mailbox 建箱（无 body，响应 token（JWT）/mailbox），
 * GET /messages 读信列表（Header Authorization: Bearer <token>，响应 {"messages":[...]}），
 * GET /message/{id} 取单封详情（Bearer），详情失败时以列表摘要归一。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "nowtempmail";
const BASE_URL = "https://nowtempmail.com";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface GenerateResponse {
  token?: string;
  mailbox?: string;
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
  const response = await fetchWithTimeout(`${BASE_URL}/mailbox`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`nowtempmail: 创建邮箱失败 http ${response.status}: ${body}`);
  }
  const data = JSON.parse(body) as GenerateResponse;
  const mailbox = String(data.mailbox || "").trim();
  const token = String(data.token || "").trim();
  if (!token || !mailbox || !mailbox.includes("@")) {
    throw new Error(`nowtempmail: 创建邮箱响应缺少必要字段: ${body}`);
  }

  return {
    channel: CHANNEL,
    email: mailbox,
    token,
  };
}

/** 获取单封邮件详情（Bearer token），响应为单封对象 */
async function fetchDetail(
  token: string,
  messageId: string,
): Promise<Record<string, unknown> | null> {
  const baseId = messageId.split("/").pop() || "";
  try {
    const response = await fetchWithTimeout(
      `${BASE_URL}/message/${encodeURIComponent(baseId)}`,
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
    return (await response.json()) as Record<string, unknown>;
  } catch {
    return null;
  }
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const authToken = String(token || "").trim();

  const response = await fetchWithTimeout(`${BASE_URL}/messages`, {
    headers: {
      Accept: "application/json",
      "User-Agent": USER_AGENT,
      Authorization: `Bearer ${authToken}`,
    },
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`nowtempmail: 获取邮件列表失败 http ${response.status}: ${body}`);
  }
  const data = JSON.parse(body) as MessagesResponse;
  const messages = Array.isArray(data.messages) ? data.messages : [];

  const out: Email[] = [];
  for (const raw of messages) {
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