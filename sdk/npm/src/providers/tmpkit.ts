/**
 * Tmpkit 渠道实现（tmpkit.com，Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）
 *
 * 调研实证结论（与 Go 端 tmpkit.go 一致）：
 *   - POST /api/rpc/tempmail/initSession，body 为 tRPC 包裹 {"json":{}}，
 *     免凭据。响应 {"json":{"session":{"sessionId","email","createdAt",
 *     "expiresAt","extendedCount"},"availableDomains":["tmpkit.com"]}}，
 *     同时 Set-Cookie: tempmail_session=<sessionId>; Max-Age=3600（与
 *     sessionId 同值）。
 *   - POST /api/rpc/tempmail/getEmails，body {"json":{"offset":0,
 *     "limit":20}}，需带 tempmail_session Cookie。响应
 *     {"json":{"emails":[...],"total":N,"session":{"email","expiresIn"}}}；
 *     不带 Cookie 时同样 200 但 emails 为空、session 为 null。列表元素
 *     键为 mailId/from/subject/excerpt/date/timestamp/hasAttach/isRead
 *     （无 id 键）。
 *   - POST /api/rpc/tempmail/getEmailDetail，body
 *     {"json":{"mailId":<数字>}}（mailId 为数字，字符串会 zod 400）。
 *     响应为单封详情对象：mailId/from/to/subject/body/date/timestamp/
 *     contentType/sourceEmail；body 为含 <br> 的纯文本正文（归一化
 *     normalizeText 已含 body 候选）。
 *
 * 会话粘性：token 保存 sessionId（tempMailSession=<sid>），每次读信以
 *   显式 Cookie 请求头携带，防全局会话被并行会话覆盖后串箱。
 */

import { Email, InternalEmailInfo, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "tmpkit";
const BASE_URL = "https://tmpkit.com";
const RPC_PREFIX = `${BASE_URL}/api/rpc/tempmail`;

/* 与 Go 端 tls-client 同款形态的固定浏览器 UA（Chrome 154 / Linux） */
const USER_AGENT =
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

/** tRPC 调用请求头（Content-Type JSON、Accept 通配、同站 Origin/Referer） */
function rpcHeaders(): Record<string, string> {
  return {
    "User-Agent": USER_AGENT,
    Accept: "*/*",
    "Content-Type": "application/json",
    Origin: BASE_URL,
    Referer: `${BASE_URL}/en`,
  };
}

/**
 * 对 tmpkit 发起 rpc 调用（tRPC 包裹 {"json":<reqBody>}）
 * 响应外层 {"json":{...}} 解析为对象返回；cookie 非空时以显式
 * Cookie 请求头携带 tempmail_session（sessionId）。
 */
async function rpc(
  procedure: string,
  reqBody: unknown,
  cookie?: string,
): Promise<Record<string, unknown>> {
  const headers = rpcHeaders();
  if (cookie) headers.Cookie = `tempmail_session=${cookie}`;

  const response = await fetchWithTimeout(`${RPC_PREFIX}/${procedure}`, {
    method: "POST",
    headers,
    body: JSON.stringify({ json: reqBody }),
  });
  if (!response.ok) {
    throw new Error(`tmpkit: ${procedure} 失败 http ${response.status}`);
  }

  const outer = (await response.json()) as { json?: Record<string, unknown> };
  if (!outer || typeof outer.json !== "object" || outer.json === null) {
    throw new Error(`tmpkit: ${procedure} 响应缺 json 载荷`);
  }
  return outer.json;
}

/** 从 map 容错取字符串（与 Go 端 tmpkitMapGet 语义一致） */
function mapGet(m: Record<string, unknown> | undefined | null, key: string): string {
  if (!m) return "";
  const v = m[key];
  if (v === undefined || v === null) return "";
  return String(v);
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  const data = await rpc("initSession", {});
  const sess = data.session as Record<string, unknown> | undefined;
  if (!sess || typeof sess !== "object") {
    throw new Error("tmpkit: 创建会话响应缺 session 字段");
  }
  const email = mapGet(sess, "email").trim();
  const sessionId = mapGet(sess, "sessionId").trim();
  if (!email || !sessionId || !email.includes("@")) {
    throw new Error("tmpkit: 创建会话响应缺少必要字段（email/sessionId）");
  }
  return {
    channel: CHANNEL,
    email,
    token: `tempMailSession=${sessionId}`,
  };
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  const raw = String(token || "").trim();
  const mailbox = raw.startsWith("tempMailSession=")
    ? raw.slice("tempMailSession=".length)
    : raw;
  if (!mailbox) {
    throw new Error("tmpkit: 会话 token 为空");
  }

  const data = await rpc("getEmails", { offset: 0, limit: 20 }, mailbox);

  /* 会话指向校验：getEmails 响应自带 session.email（无 Cookie 时
     session 为 null，同样视为会话失效） */
  const sess = data.session as Record<string, unknown> | undefined;
  if (sess && typeof sess === "object") {
    const got = mapGet(sess, "email").trim();
    if (got && got !== email) {
      throw new Error(`tmpkit: 会话邮箱不匹配（响应 ${got}，请求 ${email}）`);
    }
  } else {
    throw new Error("tmpkit: 会话已失效（getEmails 返回空会话）");
  }

  const list = data.emails as Array<Record<string, unknown>> | undefined;
  if (!Array.isArray(list)) {
    throw new Error("tmpkit: 邮件列表响应缺 emails 字段");
  }

  const emails: Email[] = [];
  for (const item of list) {
    const m: Record<string, unknown> = { ...item };
    /* mailId 为数字：合法时逐封拉详情，键并入摘要（键名不猜测，
       归一化交给 normalize 候选字段）；详情失败时回退列表摘要 */
    const mailId = m.mailId as number | undefined;
    if (typeof mailId === "number" && mailId > 0) {
      try {
        const detail = await rpc("getEmailDetail", { mailId }, mailbox);
        for (const [k, v] of Object.entries(detail)) {
          if (k === "session" || k === "emails" || k === "total" || k === "error") {
            continue;
          }
          m[k] = v;
        }
      } catch {
        /* 详情失败回退列表摘要 */
      }
    }
    emails.push(normalizeEmail(m, email));
  }
  return emails;
}