/**
 * TempMail100 渠道实现（tempmail100.com）
 * POST /init 建箱初始化（空 body，响应 code/data.token（JWT），无 Cookie），
 * POST /web/generate 创建随机地址（Header Authorization: <token>，响应 code/data.address），
 * GET /web/emails 读信列表（Header Authorization: <token>，响应 code/data.list[]/data.total）。
 *
 * 平台限制：列表元素 content 恒为空字符串（详情端点不存在，为平台限制），
 * 本渠道客观为「列表-only」：subject/fromAddress/fromName/timestamp/read 正确输出，
 * 正文如实留空。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "tempmail100";
const BASE_URL = "https://tempmail100.com";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface InitResponse {
  code?: number;
  message?: string;
  data?: {
    token?: string;
    address?: string;
    redirect?: boolean;
  };
}

interface GenerateResponse {
  code?: number;
  message?: string;
  data?: {
    address?: string;
  };
}

interface EmailsResponse {
  code?: number;
  message?: string;
  data?: {
    list?: Array<Record<string, unknown>>;
    total?: number;
  };
}

/** 接口字段值安全转换为字符串（非字符串数字转十进制，其余返回空串） */
function strOf(value: unknown): string {
  if (typeof value === "string") {
    return value;
  }
  if (typeof value === "number") {
    return String(Math.trunc(value));
  }
  return "";
}

/** read 字段归一为布尔已读标记（兼容 bool/number/string） */
function readOf(value: unknown): boolean {
  if (typeof value === "boolean") {
    return value;
  }
  if (typeof value === "number") {
    return value !== 0;
  }
  if (typeof value === "string") {
    return value.trim().toLowerCase() === "true" || value.trim() === "1";
  }
  return false;
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  /* 第一步：初始化取得 token */
  const initResponse = await fetchWithTimeout(`${BASE_URL}/init`, {
    method: "POST",
    headers: {
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
  });
  const initBody = await initResponse.text();
  if (!initResponse.ok) {
    throw new Error(`tempmail100: 初始化失败 http ${initResponse.status}: ${initBody}`);
  }
  const init = JSON.parse(initBody) as InitResponse;
  const token = String(init.data?.token || "").trim();
  if (init.code !== 0 || !token) {
    throw new Error(`tempmail100: 初始化响应异常: ${initBody}`);
  }

  /* 第二步：创建随机地址（前端使用 Authorization: <token> 不带 Bearer） */
  const response = await fetchWithTimeout(`${BASE_URL}/web/generate`, {
    method: "POST",
    headers: {
      Accept: "application/json",
      "User-Agent": USER_AGENT,
      Authorization: token,
    },
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`tempmail100: 创建地址失败 http ${response.status}: ${body}`);
  }
  const data = JSON.parse(body) as GenerateResponse;
  const address = String(data.data?.address || "").trim();
  if (data.code !== 0 || !address || !address.includes("@")) {
    throw new Error(`tempmail100: 创建地址响应异常: ${body}`);
  }

  return {
    channel: CHANNEL,
    email: address,
    token,
  };
}

/** 归一化 /web/emails 列表元素（content 恒空，如实留空） */
function normalizeItem(
  raw: Record<string, unknown>,
  address: string,
): Email {
  let fromAddress = strOf(raw.fromAddress);
  const fromName = strOf(raw.fromName);
  /* fromName+fromAddress 组合为 "Name <address>" 填入 from */
  if (
    fromName &&
    fromAddress &&
    fromName.toLowerCase() !== fromAddress.toLowerCase() &&
    fromAddress.includes("@")
  ) {
    fromAddress = `${fromName} <${fromAddress}>`;
  }

  const flat: Record<string, unknown> = {
    id: strOf(raw.uuid),
    from: fromAddress,
    to: strOf(raw.toAddress),
    subject: strOf(raw.subject),
    content: strOf(raw.content),
    timestamp: raw.timestamp,
    isRead: readOf(raw.read),
  };
  return normalizeEmail(flat, address);
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const authToken = String(token || "").trim();

  const response = await fetchWithTimeout(`${BASE_URL}/web/emails`, {
    headers: {
      Accept: "application/json",
      "User-Agent": USER_AGENT,
      Authorization: authToken,
    },
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`tempmail100: 获取邮件列表失败 http ${response.status}: ${body}`);
  }
  const data = JSON.parse(body) as EmailsResponse;
  if (data.code !== 0) {
    throw new Error(`tempmail100: 获取邮件列表响应异常: ${data.message}`);
  }
  const list = Array.isArray(data.data?.list) ? data.data.list : [];
  return list.map((raw) => normalizeItem(raw, address));
}