/**
 * DisposableMail.app 临时邮箱渠道
 * 网站: disposablemail.app
 * 域名: @disposablemail.dev
 * 纯 REST JSON API，无需认证
 *
 * 归因注记（2026-09-28 平台级归因，与 Go 端对齐）：
 * - disposablemail.dev 域可正常收信；mailmehere.cc 域存在收信丢失，
 *   故建箱强制指定 {"domain":"disposablemail.dev"}。
 * - 邮件对象按平台真实字段（fromAddress/bodyText 等）显式映射，
 *   真实结构与 SDK 设计文档映射表不同（如 from_address 实为 fromAddress）。
 */
import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "disposablemail-app";
const BASE = "https://disposablemail.app/api";

/** 请求头（与 Go 端对齐：浏览器形态头部集） */
const HEADERS: Record<string, string> = {
  "Content-Type": "application/json",
  Accept: "application/json, text/plain, */*",
  "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
  "User-Agent":
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
  Referer: "https://disposablemail.app/",
  Origin: "https://disposablemail.app",
};

/** disposablemail.app 邮件对象的真实响应结构 */
interface EmailMsg {
  id?: string;
  fromAddress?: string;
  fromName?: string;
  subject?: string;
  bodyText?: string;
  bodyHtml?: string;
  receivedAt?: string;
  isRead?: boolean;
  forwarded?: boolean;
  size?: number;
  attachments?: unknown;
}

/**
 * 创建 DisposableMail.app 临时邮箱
 *
 * 流程:
 * 1. POST /inbox 创建收件箱
 * 2. 从响应中提取 address 和 token
 * 3. token 直接存储 API 返回的字符串
 */
export async function generateEmail(): Promise<InternalEmailInfo> {
  const res = await fetchWithTimeout(`${BASE}/inbox`, {
    method: "POST",
    headers: HEADERS,
    body: JSON.stringify({ domain: "disposablemail.dev" }),
  });
  if (res.status === 429) {
    const retryAfter = res.headers.get("Retry-After") || "";
    throw new Error(`disposablemail-app: 创建邮箱平台限流(429 Retry-After=${retryAfter})，请稍后重试`);
  }
  if (!res.ok) {
    throw new Error(`disposablemail-app: 创建邮箱失败 HTTP ${res.status}`);
  }

  const json = (await res.json()) as {
    address?: string;
    token?: string;
    expiresAt?: string;
    createdAt?: string;
  };

  if (!json.address) {
    throw new Error("disposablemail-app: 创建邮箱返回数据缺少 address");
  }
  if (!json.token) {
    throw new Error("disposablemail-app: 创建邮箱返回数据缺少 token");
  }

  return {
    channel: CHANNEL,
    email: json.address,
    token: json.token,
    expiresAt: json.expiresAt,
    createdAt: json.createdAt,
  };
}

/**
 * 获取 DisposableMail.app 邮箱的邮件列表
 *
 * 流程:
 * 1. GET /inbox/emails?token={token} 获取邮件列表
 * 2. 标准化邮件数据
 */
export async function getEmails(
  token: string,
  email: string,
): Promise<Email[]> {
  if (!email?.trim()) {
    throw new Error("disposablemail-app: 邮箱地址为空");
  }
  if (!token?.trim()) {
    throw new Error("disposablemail-app: token 为空");
  }

  /* 平台对同一 URL 偶发返回陈旧空列表，附加 cachebust 强制回源；同时显式禁缓存 */
  const cacheBust =
    BigInt(Date.now()) * 1000000n + (process.hrtime.bigint() % 1000000n);
  const url = `${BASE}/inbox/emails?token=${encodeURIComponent(
    token.trim(),
  )}&cachebust=${cacheBust.toString()}`;
  /* GET 请求不需要 Content-Type，从浏览器形态头集中剔除 */
  const { "Content-Type": _contentType, ...getHeaders } = HEADERS;
  const res = await fetchWithTimeout(url, {
    method: "GET",
    headers: { ...getHeaders, "Cache-Control": "no-cache" },
  });
  if (res.status === 429) {
    const retryAfter = res.headers.get("Retry-After") || "";
    throw new Error(`disposablemail-app: 获取邮件平台限流(429 Retry-After=${retryAfter})，请稍后重试`);
  }
  if (!res.ok) {
    throw new Error(`disposablemail-app: 获取邮件失败 HTTP ${res.status}`);
  }

  const json = (await res.json()) as {
    emails?: EmailMsg[];
    total?: number;
  };

  if (!Array.isArray(json.emails) || json.emails.length === 0) {
    return [];
  }

  /* 按平台邮件对象真实字段显式映射后归一化 */
  return json.emails.map((msg: EmailMsg) =>
    normalizeEmail(
      {
        id: msg.id,
        from: msg.fromAddress,
        to: email,
        subject: msg.subject,
        text: msg.bodyText,
        html: msg.bodyHtml,
        date: msg.receivedAt,
        isRead: msg.isRead,
        name: msg.fromName,
        forwarded: msg.forwarded,
        size: msg.size,
        attachments: msg.attachments,
      },
      email,
    ),
  );
}
