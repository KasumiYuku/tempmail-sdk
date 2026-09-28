/**
 * Shadowmail 渠道实现（shadowmail.win）
 * 契约（前端 JS 抓取 + curl 实测）：
 * - 注册：POST /api/register {"email":"<随机>@gmail.com","password":"Abcd1234!"}。
 * - 登录：POST /api/login（同 body）→ Set-Cookie: sessionId=<uuid>。
 * - 建箱：POST /api/new-address → {"address":"<id>@shadowmail.win","id":<id>}。
 * - 读信：POST /api/get-emails {"address":"<地址>"} → {"mails":[...]}。
 * 会话以显式 Cookie 头逐请求携带；凭据串持久化注册邮箱/密码/会话 id，
 * 会话过期（401/404）后自动重新登录重试一次。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "shadowmail";
const BASE_URL = "https://shadowmail.win";
/** 固定注册密码（平台无自选密码入口） */
const PASSWORD = "Abcd1234!";
/** 平台唯一收信域 */
const DOMAIN = "shadowmail.win";
/** 凭据串前缀 */
const TOKEN_PREFIX = "shadowmail|";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface AccountResponse {
  message?: string;
}

interface NewAddressResponse {
  message?: string;
  address?: string;
  id?: number;
}

interface GetEmailsResponse {
  message?: string;
  mails?: Array<Record<string, unknown>>;
}

/** 生成随机注册邮箱前缀（sdk+8 位小写字母） */
function randomAccount(): string {
  const chars = "abcdefghijklmnopqrstuvwxyz";
  let body = "";
  for (let i = 0; i < 8; i++) {
    body += chars[Math.floor(Math.random() * chars.length)];
  }
  return `sdk${body}`;
}

/** 注册或登录（POST /api/register、/api/login），返回会话 id（纯 uuid） */
async function registerOrLogin(
  account: string,
  password: string,
  isLogin: boolean,
): Promise<string> {
  const path = isLogin ? "/api/login" : "/api/register";
  const response = await fetchWithTimeout(`${BASE_URL}${path}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
    body: JSON.stringify({ email: account, password }),
  });
  const body = await response.text();
  if (!response.ok) {
    throw new Error(`shadowmail ${path}: http ${response.status}`);
  }
  const data = JSON.parse(body) as AccountResponse;
  if (isLogin && data.message !== "Successfull Login") {
    throw new Error(`shadowmail login: ${data.message}`);
  }
  /* 重复注册（幂等）：消息为 Email already in use 时视为账号已存在 */
  if (!isLogin && data.message !== "Successfully Registered" && data.message !== "Email already in use") {
    throw new Error(`shadowmail register: ${data.message}`);
  }

  /* 从 Set-Cookie 提取 sessionId 值（纯 uuid，不含键名） */
  const setCookies = response.headers.get("set-cookie") || "";
  for (const part of setCookies.split(/,(?=\s*sessionId=)/)) {
    const item = part.split(";")[0].trim();
    if (item.startsWith("sessionId=")) {
      return item.slice("sessionId=".length);
    }
  }
  if (isLogin) {
    throw new Error("shadowmail login: 未下发 sessionId Cookie");
  }
  return "";
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const account = `${randomAccount()}@gmail.com`;

  /* 1) 注册（幂等：已存在同名账号则跳过） */
  await registerOrLogin(account, PASSWORD, false);
  /* 2) 登录取得 sessionId（纯 uuid，不合成键值对） */
  const session = await registerOrLogin(account, PASSWORD, true);

  /* 3) 创建地址（每账号 12 槽）：SDK 层显式 Cookie 头传 sessionId */
  const response = await fetchWithTimeout(`${BASE_URL}/api/new-address`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "User-Agent": USER_AGENT,
      Cookie: `sessionId=${session}`,
    },
    body: "{}",
  });
  if (!response.ok) {
    throw new Error(`shadowmail new-address: http ${response.status}`);
  }
  const data = (await response.json()) as NewAddressResponse;
  const address = String(data.address || "").toLowerCase().trim();
  if (!address || !address.endsWith(`@${DOMAIN}`)) {
    throw new Error("shadowmail new-address: 响应缺少有效地址");
  }

  /* Token 持久化：account|password|sessionId */
  const token = `${TOKEN_PREFIX}${account}|${PASSWORD}|${session}`;
  return {
    channel: CHANNEL,
    email: address,
    token,
  };
}

/** 解析凭据串为 account/password/sessionId 三元组 */
function parseToken(token: string): {
  account: string;
  password: string;
  session: string;
} {
  if (!token.startsWith(TOKEN_PREFIX)) {
    throw new Error("shadowmail: token 格式错误");
  }
  const parts = token.slice(TOKEN_PREFIX.length).split("|");
  if (parts.length !== 3) {
    throw new Error("shadowmail: token 字段缺失");
  }
  const [account, password, session] = parts;
  if (!account || !password || !session) {
    throw new Error("shadowmail: token 凭据字段为空");
  }
  return { account, password, session };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const { account, password, session: initialSession } = parseToken(token);
  let session = initialSession;

  const doFetch = async (): Promise<{ body: string; status: number }> => {
    const response = await fetchWithTimeout(`${BASE_URL}/api/get-emails`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json",
        "User-Agent": USER_AGENT,
        Cookie: `sessionId=${session}`,
      },
      body: JSON.stringify({ address }),
    });
    return { body: await response.text(), status: response.status };
  };

  let result = await doFetch();
  /* sessionId 最长 1 小时（Max-Age 3600），过期后重登录重试一次 */
  if (result.status === 401 || result.status === 404) {
    session = await registerOrLogin(account, password, true);
    result = await doFetch();
  }
  if (result.status < 200 || result.status >= 300) {
    throw new Error(`shadowmail get-emails: http ${result.status}`);
  }

  const data = JSON.parse(result.body) as GetEmailsResponse;
  if (data.message !== "Emails read") {
    throw new Error(`shadowmail get-emails: ${data.message}`);
  }

  const mails = Array.isArray(data.mails) ? data.mails : [];
  return mails.map((raw) => {
    const flat: Record<string, unknown> = { ...raw };
    flat.from = raw.sender;
    flat.to = address;
    flat.date = raw.created_at;
    /* 平台无 text/html 区分，body 为正文（默认按纯文本处理） */
    flat.text = raw.body;
    return normalizeEmail(flat, address);
  });
}