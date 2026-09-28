/**
 * Firetempmail 渠道实现（firetempmail.com）
 * 无认证 REST：建箱无需请求，本地生成 随机词+0-999@<域>（域池与官网一致：
 * offrework.click / service-today.click / jobsdeforyou.sa.com）；
 * 读信 GET https://mail.firetempmail.com/mail/get?address=<邮箱 URL 编码>，
 * 必须携带 Header Origin: https://firetempmail.com（否则 403 'Origin not allowed'）。
 * 响应形如 {"status":"ok","code":200,"msg":"...","stats":{},"mails":[...]}，
 * 邮件字段以 sender/subject/date/content-text/content-html 多候选归一化。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "firetempmail";
const API_BASE = "https://mail.firetempmail.com";
const ORIGIN = "https://firetempmail.com";

/** 官网 JS 中的完整平台域池，顺序与官网一致 */
const DOMAINS = [
  "offrework.click",
  "service-today.click",
  "jobsdeforyou.sa.com",
];

const LETTERS = "abcdefghijklmnopqrstuvwxyz";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

/** 多候选字段取值：返回第一个存在且非空的字符串键值 */
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

/** 随机 3-6 位小写字母单词 + 0-999（与官网 faker 词 + 1e3 取整一致） */
function localPart(): string {
  const n = 3 + Math.floor(Math.random() * 4);
  let word = "";
  for (let i = 0; i < n; i++) {
    word += LETTERS[Math.floor(Math.random() * LETTERS.length)];
  }
  return `${word}${Math.floor(Math.random() * 1000)}`;
}

interface InboxResponse {
  status?: string;
  code?: number;
  msg?: string;
  stats?: Record<string, unknown>;
  mails?: Array<Record<string, unknown>>;
}

/** 建箱无需请求，本地生成 随机词+0-999@域名 形式；token 复用完整地址 */
export async function generateEmail(): Promise<InternalEmailInfo> {
  const domain = DOMAINS[Math.floor(Math.random() * DOMAINS.length)];
  const email = `${localPart()}@${domain}`;
  return { channel: CHANNEL, email, token: email };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address) {
    throw new Error("firetempmail 读信: 邮箱地址为空");
  }

  const response = await fetchWithTimeout(
    `${API_BASE}/mail/get?address=${encodeURIComponent(address)}`,
    {
      headers: {
        Accept: "application/json",
        Origin: ORIGIN,
        Referer: `${ORIGIN}/`,
        "User-Agent": USER_AGENT,
      },
    },
  );
  if (!response.ok) {
    throw new Error(`firetempmail 读信: http ${response.status}`);
  }

  const data = (await response.json()) as InboxResponse;
  if (data.status && data.status !== "ok") {
    throw new Error(`firetempmail 读信: ${data.msg || data.status}`);
  }

  const rows = Array.isArray(data.mails) ? data.mails : [];
  return rows.map((raw) =>
    normalizeEmail(
      {
        from: pickStr(raw, "sender", "from", "from_address"),
        to: address,
        subject: pickStr(raw, "subject", "title"),
        /* 正文多候选：content-html 优先，其次 content-text / content-plain / text / html */
        html: pickStr(raw, "content-html", "html"),
        text: pickStr(raw, "content-text", "content-plain", "text"),
        date: pickStr(raw, "date", "received_at", "created_at"),
      },
      address,
    ),
  );
}