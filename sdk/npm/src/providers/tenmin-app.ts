/**
 * TenminApp 渠道实现（tenmin.app，真实 API 域 api.tenmin.app）
 * 建箱+读信共用 GET /api/inbox/{localpart}（Accept: application/json）。
 * localpart 为随机 6 位小写十六进制串（首访即建箱，平台无显式创建接口）；
 * 响应：{"inboxId":..,"address":"..@tenmin.app","ttl":600,"count":0,"messages":[]}，
 * messages[] 元素字段：id/from/subject/text/html/receivedAt（from 为 {name,address} 对象）。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "tenmin-app";
const BASE_URL = "https://api.tenmin.app";

const HEX_CHARS = "0123456789abcdef";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

/** 生成 6 位小写十六进制随机 localpart */
function localPart(): string {
  let part = "";
  for (let i = 0; i < 6; i++) {
    part += HEX_CHARS[Math.floor(Math.random() * HEX_CHARS.length)];
  }
  return part;
}

interface InboxResponse {
  inboxId?: string;
  address?: string;
  ttl?: number;
  count?: number;
  messages?: Array<Record<string, unknown>>;
}

/** 请求 /api/inbox/{localpart}（建箱与读信共用） */
async function fetchInbox(localpart: string): Promise<InboxResponse> {
  const response = await fetchWithTimeout(
    `${BASE_URL}/api/inbox/${encodeURIComponent(localpart)}`,
    {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
      },
    },
  );
  if (!response.ok) {
    throw new Error(`tenmin-app inbox: http ${response.status}`);
  }
  return (await response.json()) as InboxResponse;
}

/** 展开 from 对象（{name,address}）为字符串 */
function flattenFrom(raw: Record<string, unknown>): string {
  const from = raw.from;
  if (from && typeof from === "object") {
    const obj = from as Record<string, unknown>;
    const address = String(obj.address || "");
    const name = String(obj.name || "");
    if (address && name) return `${name} <${address}>`;
    if (address) return address;
    return name;
  }
  return from !== undefined ? String(from) : "";
}

/** 首次 GET 随机 localpart 即自动建箱（10 分钟 TTL），token 存储 localpart */
export async function generateEmail(): Promise<InternalEmailInfo> {
  const local = localPart();
  const data = await fetchInbox(local);
  const email = String(data.address || "") || `${local}@tenmin.app`;
  const ttlMs =
    typeof data.ttl === "number" && data.ttl > 0 ? data.ttl * 1000 : 0;

  return {
    channel: CHANNEL,
    email,
    token: local,
    expiresAt:
      ttlMs > 0 ? new Date(Date.now() + ttlMs).toISOString() : undefined,
  };
}

/** 复用建箱同一 localpart 轮询；from 为对象时展开后交归一化处理 */
export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const localpart = String(token || "").trim();
  if (!localpart) {
    throw new Error("tenmin-app: token 为空");
  }

  const data = await fetchInbox(localpart);
  const rows = Array.isArray(data.messages) ? data.messages : [];

  return rows.map((raw) =>
    normalizeEmail(
      {
        id: raw.id,
        from: flattenFrom(raw),
        to: address,
        subject: raw.subject,
        text: raw.text,
        html: raw.html,
        date: raw.receivedAt,
      },
      address,
    ),
  );
}