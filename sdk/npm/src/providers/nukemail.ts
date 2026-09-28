/**
 * Nukemail 渠道实现（nukemail.app）
 * 契约（抓前端 JS + curl/PoW 全流程实测）：
 * - PoW：GET /api/pow/challenge?difficulty=4 → {"id","challenge","difficulty"}；
 *   求 nonce 使 SHA-256(challenge+nonce) 十六进制前 difficulty 位为 0。
 * - 建箱：POST /api/inbox/create body {"address","domain","pow_id","pow_nonce"}
 *   → {"token":"NUKE-xxxxxxxx","email":"名@域名"}。
 * - 读信：GET /api/inbox（Cookie: nukemail_token=<token>），
 *   messages 元素字段 sender/sender_name/subject/body_html/body_text/received_at/read。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "nukemail";
const BASE_URL = "https://nukemail.app";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

interface ChallengeResponse {
  id?: string;
  challenge?: string;
  difficulty?: number;
}

interface CreateResponse {
  token?: string;
  email?: string;
  error?: string;
  suggestions?: string[];
}

interface InboxResponse {
  token?: string;
  state?: string;
  addresses?: Array<Record<string, unknown>>;
  messages?: Array<Record<string, unknown>>;
  is_premium?: boolean;
}

/** 求解 PoW：返回使 SHA-256(challenge+nonce) 前 difficulty 位为 0 的最小 nonce */
async function solvePow(challenge: string, difficulty: number): Promise<number> {
  const subtle = globalThis.crypto?.subtle;
  if (!subtle) {
    throw new Error("nukemail: 当前运行环境不支持 Web Crypto（需浏览器或 Node 19+）");
  }
  const prefix = "0".repeat(difficulty);
  const encoder = new TextEncoder();
  for (let nonce = 0; ; nonce++) {
    const data = encoder.encode(`${challenge}${nonce}`);
    const digest = new Uint8Array(await subtle.digest("SHA-256", data));
    const hexdigest = Array.from(digest)
      .map((byte) => byte.toString(16).padStart(2, "0"))
      .join("");
    if (hexdigest.startsWith(prefix)) {
      return nonce;
    }
  }
}

/** 生成本地随机名（与前端 generateRandomName 等价形态） */
function randomAddress(): string {
  const chars = "abcdefghijklmnopqrstuvwxyz0123456789";
  let body = "";
  for (let i = 0; i < 10; i++) {
    body += chars[Math.floor(Math.random() * chars.length)];
  }
  return `nuke${body}`;
}

/** 取第一个非 premium 的活跃域名 */
async function pickDomain(): Promise<string> {
  const response = await fetchWithTimeout(`${BASE_URL}/api/domains`, {
    headers: {
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
  });
  if (!response.ok) {
    throw new Error(`nukemail domains: http ${response.status}`);
  }
  const data = (await response.json()) as {
    domains?: Array<{ domain?: string; is_premium_only?: boolean }>;
  };
  for (const item of data.domains || []) {
    if (!item.is_premium_only && item.domain) {
      return item.domain;
    }
  }
  throw new Error("nukemail generate: 无可用非 premium 域名");
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  /* 1) 取 PoW 挑战 */
  const challengeResponse = await fetchWithTimeout(
    `${BASE_URL}/api/pow/challenge?difficulty=4`,
    {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
      },
    },
  );
  if (!challengeResponse.ok) {
    throw new Error(`nukemail generate: challenge http ${challengeResponse.status}`);
  }
  const challenge = (await challengeResponse.json()) as ChallengeResponse;
  if (!challenge.id || !challenge.challenge) {
    throw new Error("nukemail generate: challenge 响应缺少 id/challenge");
  }
  const difficulty = challenge.difficulty && challenge.difficulty > 0
    ? challenge.difficulty
    : 4;

  /* 2) 本地求 PoW 解（Web Crypto 原生 SHA-256） */
  const nonce = await solvePow(challenge.challenge, difficulty);

  /* 3) 取域名并建箱 */
  const domain = await pickDomain();
  const response = await fetchWithTimeout(`${BASE_URL}/api/inbox/create`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json",
      "User-Agent": USER_AGENT,
    },
    body: JSON.stringify({
      address: randomAddress(),
      domain,
      pow_id: challenge.id,
      pow_nonce: String(nonce),
    }),
  });
  const rawBody = (await response.text()).trim();
  if (!response.ok) {
    throw new Error(`nukemail generate: create http ${response.status}: ${rawBody}`);
  }
  const data = JSON.parse(rawBody) as CreateResponse;
  if (!data.token || !data.email) {
    throw new Error("nukemail generate: create 响应缺少 token/email");
  }

  return {
    channel: CHANNEL,
    email: data.email,
    token: data.token,
  };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  const accessCode = String(token || "").trim();
  if (!accessCode) {
    throw new Error("nukemail: token 为空");
  }
  const cookie = `nukemail_token=${accessCode}`;

  const doFetch = async (): Promise<InboxResponse> => {
    const response = await fetchWithTimeout(`${BASE_URL}/api/inbox`, {
      headers: {
        Accept: "application/json",
        "User-Agent": USER_AGENT,
        Cookie: cookie,
      },
    });
    if (!response.ok) {
      throw new Error(`nukemail 读信: http ${response.status}`);
    }
    return (await response.json()) as InboxResponse;
  };

  let data: InboxResponse;
  try {
    data = await doFetch();
  } catch {
    throw new Error("nukemail 读信: 请求失败");
  }
  if (data.state === "expired" || !data.state) {
    /* 会话可能已过期：经 resume 重设会话后重试 */
    try {
      const resumeResponse = await fetchWithTimeout(`${BASE_URL}/api/inbox/resume`, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "User-Agent": USER_AGENT,
        },
        body: JSON.stringify({ accessCode: accessCode }),
      });
      await resumeResponse.text();
    } catch {
      /* resume 失败不阻断，保留首次错误 */
    }
    data = await doFetch();
  }

  const messages = Array.isArray(data.messages) ? data.messages : [];
  return messages.map((raw) => {
    const flat: Record<string, unknown> = { ...raw };
    flat.to = address;
    /* 平台消息字段为 body_html/body_text：缺失 text/html 时补齐候选 */
    if ((flat.text === undefined || flat.text === null) && raw.body_text !== undefined) {
      flat.text = raw.body_text;
    }
    if ((flat.html === undefined || flat.html === null) && raw.body_html !== undefined) {
      flat.html = raw.body_html;
    }
    flat.date = raw.received_at;
    flat.read = raw.read;
    flat.sender_email = raw.sender;
    return normalizeEmail(flat, address);
  });
}