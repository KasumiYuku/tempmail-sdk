/**
 * Zerodrop 渠道实现（zerodrop.dev）
 * 无认证 REST：建箱无需请求，本地生成 "sdk"+8 位随机名，地址为 <名>@zerodrop-sandbox.online；
 * 读信 GET /api/inbox/{name}?source=sdk，响应形如 {"emails":[...],"count":N}。
 * 平台邮件对象只有 id/from/to/subject/receivedAt/raw/otp/magicLink：正文仅存在于
 * raw（完整 MIME 原文，头部与 body 以空行分隔），无 text/html 字段，
 * 故须从 raw 中剥离头部提取纯文本 body 填入 text，html 留空由归一化互转。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "zerodrop";
const BASE_URL = "https://zerodrop.dev";
const DOMAIN = "zerodrop-sandbox.online";

const LOCAL_CHARS = "abcdefghijklmnopqrstuvwxyz0123456789";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

/** 生成 "sdk"+8 位随机本地名 */
function localName(): string {
  let name = "sdk";
  for (let i = 0; i < 8; i++) {
    name += LOCAL_CHARS[Math.floor(Math.random() * LOCAL_CHARS.length)];
  }
  return name;
}

/**
 * 从 raw（完整 MIME 原文）提取纯文本正文：
 * 定位首个空行（RFC 5322 头部/正文分隔，\r\n\r\n 或 \n\n），其后部分即 body。
 */
function rawBody(raw: string): string {
  const crlfIndex = raw.indexOf("\r\n\r\n");
  if (crlfIndex >= 0) return raw.slice(crlfIndex + 4);
  const lfIndex = raw.indexOf("\n\n");
  if (lfIndex >= 0) return raw.slice(lfIndex + 2);
  return "";
}

interface InboxResponse {
  emails?: Array<Record<string, unknown>>;
  count?: number;
}

/** 建箱无需请求，本地生成随机名，token 复用完整地址以便收件箱回查 */
export async function generateEmail(): Promise<InternalEmailInfo> {
  const email = `${localName()}@${DOMAIN}`;
  return { channel: CHANNEL, email, token: email };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const parts = address.split("@");
  if (parts.length !== 2 || parts[1] !== DOMAIN) {
    throw new Error(`zerodrop 读信: 非 ${DOMAIN} 域邮箱地址`);
  }

  const response = await fetchWithTimeout(
    `${BASE_URL}/api/inbox/${encodeURIComponent(parts[0])}?source=sdk`,
    {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
      },
    },
  );
  if (!response.ok) {
    throw new Error(`zerodrop 读信: http ${response.status}`);
  }

  const data = (await response.json()) as InboxResponse;
  const rows = Array.isArray(data.emails) ? data.emails : [];

  return rows.map((raw) => {
    const rawText = rawBody(String(raw.raw || ""));
    return normalizeEmail(
      {
        id: raw.id,
        from: raw.from,
        to: raw.to || address,
        subject: raw.subject,
        /* 正文仅存在于 raw（完整 MIME 原文），无 text/html 字段：提取纯文本 body */
        text: rawText,
        html: "",
        date: raw.receivedAt,
      },
      address,
    );
  });
}