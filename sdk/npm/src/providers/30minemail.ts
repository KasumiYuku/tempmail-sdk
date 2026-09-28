/**
 * 30minemail 渠道实现（30minemail.com）
 * 官网邮箱由服务端 16 位 hex 本地名识别：
 * - 建箱 GET /?generate 返回完整 HTML 页面（含 <地址>@30minemail.com），
 *   从其中解析邮箱地址。
 * - 读信 GET /messages.php?email=<完整地址>&_=<unix毫秒>，
 *   响应 {"ok":true,"expired":false,"count":0,"emails":[...],...}，
 *   emails 元素字段实测：{id,from,to,subject,date,html}。
 * 无认证、无 Cookie、无 CSRF。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "30minemail";
const BASE_URL = "https://30minemail.com";
const DOMAIN = "30minemail.com";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface InboxResponse {
  ok?: boolean;
  expired?: boolean;
  count?: number;
  emails?: Array<Record<string, unknown>>;
  expires_in?: number;
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const response = await fetchWithTimeout(`${BASE_URL}/?generate`, {
    headers: {
      Accept: "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
      "User-Agent": USER_AGENT,
    },
  });
  if (!response.ok) {
    throw new Error(`30minemail: 创建邮箱失败 http ${response.status}`);
  }
  const page = await response.text();

  const atIndex = page.indexOf(`@${DOMAIN}`);
  if (atIndex < 0) {
    throw new Error("30minemail: 创建页面未找到邮箱地址");
  }
  /* 向前查找本地名起点：空白或 > 之后 */
  let start = atIndex;
  while (start > 0) {
    const charCode = page[start - 1];
    if (
      charCode === " " ||
      charCode === "\n" ||
      charCode === "\t" ||
      charCode === ">" ||
      charCode === '"'
    ) {
      break;
    }
    start--;
  }
  const local = page.slice(start, atIndex).trim();
  if (local.length < 8) {
    throw new Error(`30minemail: 创建页面解析地址异常: ${page.slice(start, atIndex + DOMAIN.length + 1)}`);
  }
  const address = `${local}@${DOMAIN}`;

  return {
    channel: CHANNEL,
    email: address,
    token: address,
  };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address) {
    throw new Error("30minemail: 邮箱地址为空");
  }

  const cacheBuster = Date.now();
  const response = await fetchWithTimeout(
    `${BASE_URL}/messages.php?email=${encodeURIComponent(address)}&_=${cacheBuster}`,
    {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
      },
    },
  );
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`30minemail: 读取收件箱失败 http ${response.status}: ${body}`);
  }
  const data = JSON.parse(body) as InboxResponse;
  if (!data.ok || data.expired) {
    throw new Error("30minemail: 收件箱不可用或已过期");
  }
  const emails = Array.isArray(data.emails) ? data.emails : [];

  return emails.map((raw) => {
    const flat: Record<string, unknown> = { ...raw };
    /* 列表元素无 to 字段时注入收件人地址 */
    if (flat.to === undefined || flat.to === null) {
      flat.to = address;
    }
    return normalizeEmail(flat, address);
  });
}