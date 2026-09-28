/**
 * LinshiXYZ 渠道实现（linshi.xyz）
 * 无建箱请求：本地随机 6 位 hex 前缀（官网 client 即用短 id）+ @linshi.xyz。
 * 读信 GET https://linshi.xyz/api/mails/{前缀}，无邮件时返回空数组，
 * 有邮件时为邮件对象数组（元素字段 headers.from/to/subject/date/html）。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "linshi-xyz";
const BASE_URL = "https://linshi.xyz";
const DOMAIN = "linshi.xyz";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

/** 生成本地随机 6 位 hex 前缀（与官网 client 相同格式） */
function localName(): string {
  const hexDigits = "0123456789abcdef";
  let out = "";
  for (let i = 0; i < 6; i++) {
    out += hexDigits[Math.floor(Math.random() * hexDigits.length)];
  }
  return out;
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const address = `${localName()}@${DOMAIN}`;
  return {
    channel: CHANNEL,
    email: address,
    token: address,
  };
}

/** 归一化单封邮件：headers 对象平铺为顶层字段并注入收件人地址 */
function normalizeMail(
  raw: Record<string, unknown>,
  address: string,
): Email {
  const flat: Record<string, unknown> = { ...raw };
  const headers = raw.headers as Record<string, unknown> | undefined;
  if (headers && typeof headers === "object") {
    for (const [key, value] of Object.entries(headers)) {
      flat[key] = value;
    }
  }
  if (flat.to === undefined || flat.to === null) {
    flat.to = address;
  }
  return normalizeEmail(flat, address);
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const atIndex = address.indexOf("@");
  if (!address || atIndex < 0) {
    throw new Error(`linshi-xyz: 邮箱地址无效: ${JSON.stringify(email)}`);
  }
  const local = address.slice(0, atIndex);

  const response = await fetchWithTimeout(
    `${BASE_URL}/api/mails/${encodeURIComponent(local)}`,
    {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
      },
    },
  );
  if (!response.ok) {
    const body = await response.text();
    throw new Error(`linshi-xyz: 读取收件箱失败 http ${response.status}: ${body}`);
  }

  const data = (await response.json()) as Array<Record<string, unknown>>;
  if (!Array.isArray(data)) {
    throw new Error("linshi-xyz: 收件箱响应非数组骨架");
  }
  return data.map((raw) => normalizeMail(raw, address));
}