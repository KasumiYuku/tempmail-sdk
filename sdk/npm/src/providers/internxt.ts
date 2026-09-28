/**
 * Internxt 渠道实现（internxt.com/temporary-email，Next.js + OpenNext）
 *
 * 调研实证结论（与 Go 端 internxt.go 一致）：
 *   - 建箱 GET /api/temp-mail/create-email，读信 GET
 *     /api/temp-mail/get-inbox?email=<e>&token=<t>，详情 GET
 *     /api/temp-mail/get-message?email=<e>&token=<t>&messageId=<id>。
 *     create-email 仅接受 GET（POST 返回 405 Method not allowed）。
 *   - CSRF：首次 GET /temporary-email 响应 Set-Cookie csrfSecret=...
 *     与 XSRF-TOKEN=...（均 HttpOnly）。数据接口校验请求头
 *     csrf-token，其值必须与 Cookie jar 中 XSRF-TOKEN 一致：
 *     头=XSRF-TOKEN 值时 200；头=csrfSecret 值时恒定 500。
 *   - 建箱响应：{"address":"<前缀>@uberip.com","token":"<十六进制>"}。
 *     健壮性提醒：每个 API 响应均会刷新 XSRF-TOKEN 的 Set-Cookie，
 *     故每次读信前都应从 Cookie 罐重取最新值作为 csrf-token 头
 *     （实测 csrfSecret 恒定，XSRF-TOKEN 每次刷新）。
 *   - get-inbox 正常返回顶层数组（空箱 []）；错误 token 返回 401
 *     {"message":"Email has expired"}。
 *
 * 会话粘性：module 级维护"XSRF-TOKEN 最新值"状态（每次 API 响应回刷）；
 *   建箱/读信全部携带显式 Cookie 与 csrf-token 头，SDK fetch 无自动
 *   Cookie 罐，故自管罐随本渠道独占，杜绝跨渠道串池。
 * token 语义：{address, token} JSON（收信凭据）。
 */

import { Email, InternalEmailInfo, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "internxt";
const SITE = "https://internxt.com";
const REF = `${SITE}/temporary-email`;
const API_BASE = `${SITE}/api/temp-mail`;

/* 与 Go 端 tls-client 同款形态的固定浏览器 UA（Chrome 154 / Linux） */
const USER_AGENT =
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

/* 页面夺取头（GET /temporary-email） */
const HTML_HEADERS: Record<string, string> = {
  "User-Agent": USER_AGENT,
  Accept:
    "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
  "Accept-Language": "en-US,en;q=0.9",
};

/* API 请求头模板（csrf-token 与 Cookie 每次请求前重取/回刷） */
const API_HEADERS: Record<string, string> = {
  "User-Agent": USER_AGENT,
  Accept: "application/json, text/plain, */*",
  Origin: SITE,
  Referer: REF,
};

/** 模块级简易 Cookie 罐：{csrfSecret?, XSRF-TOKEN?}（XSRF-TOKEN 每个 API 响应都会刷新） */
const jar: { csrfSecret: string; xsrf: string } = { csrfSecret: "", xsrf: "" };

/** 从 Set-Cookie 头列表解析 cookie 名值对 */
function parseSetCookies(response: Response): Record<string, string> {
  const out: Record<string, string> = {};
  let lines: string[] = [];
  try {
    const h = response.headers as Headers & { getSetCookie?: () => string[] };
    if (typeof h.getSetCookie === "function") {
      lines = h.getSetCookie();
    }
  } catch {
    lines = [];
  }
  if (lines.length === 0) {
    lines = (response.headers.get("set-cookie") || "").split(/,(?=[^;]*=)/);
  }
  for (const line of lines) {
    const kv = (line.split(";")[0] || "").trim();
    const eq = kv.indexOf("=");
    if (eq > 0) out[kv.slice(0, eq)] = kv.slice(eq + 1);
  }
  return out;
}

/** 将罐内 Cookie 组装为 Cookie 请求头（有则携带） */
function jarCookieHeader(): string {
  const parts: string[] = [];
  if (jar.csrfSecret) parts.push(`csrfSecret=${jar.csrfSecret}`);
  if (jar.xsrf) parts.push(`XSRF-TOKEN=${jar.xsrf}`);
  return parts.join("; ");
}

/** 收下响应的 Set-Cookie：同名覆写，XSRF-TOKEN 每次刷新回灌 */
function harvest(response: Response): void {
  const got = parseSetCookies(response);
  if (got.csrfSecret) jar.csrfSecret = got.csrfSecret;
  if (got["XSRF-TOKEN"]) jar.xsrf = got["XSRF-TOKEN"];
}

/**
 * 确保罐中持有本域 csrfSecret 与 XSRF-TOKEN 并返回当前 XSRF-TOKEN 值
 * 无则 GET /temporary-email 页面夺取（每次 API 响应都会刷新该 Cookie，
 * 故调用方每次请求前都应重新调用以取最新值）。
 */
async function prepareXsrf(): Promise<string> {
  if (jar.xsrf) return jar.xsrf;

  const response = await fetchWithTimeout(REF, { headers: HTML_HEADERS });
  await response.text();
  harvest(response);
  if (!jar.xsrf) {
    throw new Error("internxt: 未取得 XSRF-TOKEN Cookie");
  }
  return jar.xsrf;
}

/** 带 CSRF 头请求 internxt 数据接口（GET），返回解析后的响应 */
async function apiGet(
  path: string,
  query?: Record<string, string>,
): Promise<Response> {
  const csrfToken = await prepareXsrf();
  const params = new URLSearchParams(query || {});
  const suffix = params.toString() ? `?${params.toString()}` : "";

  const response = await fetchWithTimeout(`${API_BASE}${path}${suffix}`, {
    headers: {
      ...API_HEADERS,
      "csrf-token": csrfToken,
      Cookie: jarCookieHeader(),
    },
  });
  harvest(response);
  return response;
}

export async function generateEmail(): Promise<InternalEmailInfo> {
  await prepareXsrf();

  const response = await apiGet("/create-email");
  if (!response.ok) {
    throw new Error(`internxt: /create-email 失败 http ${response.status}`);
  }
  const data = (await response.json()) as { address?: string; token?: string };
  const address = String(data.address || "").trim();
  const apiToken = String(data.token || "").trim();
  if (!address || !apiToken || !address.includes("@")) {
    throw new Error("internxt: 创建邮箱响应缺少必要字段（address/token）");
  }
  return {
    channel: CHANNEL,
    email: address,
    token: JSON.stringify({ address, token: apiToken }),
  };
}

/** 从列表元素中提取邮件 ID（字符串形态，与 Go 端 messageIDOf 一致） */
function messageIdOf(m: Record<string, unknown>): string {
  for (const key of ["id", "messageId", "message_id"]) {
    const v = m[key];
    if (typeof v === "string" && v.trim() !== "") return v;
  }
  return "";
}

export async function getEmails(email: string, token: string): Promise<Email[]> {
  let sess: { address?: string; token?: string };
  try {
    sess = JSON.parse(String(token || ""));
  } catch (err) {
    throw new Error(`internxt: 会话凭据解析失败: ${err}`);
  }
  if (!sess || typeof sess !== "object") {
    throw new Error("internxt: 会话凭据解析失败（非对象）");
  }
  const address = String(sess.address || "").trim();
  const apiToken = String(sess.token || "").trim();
  if (!address || !apiToken) {
    throw new Error("internxt: 会话凭据缺少必要字段");
  }
  if (address !== email) {
    throw new Error("internxt: 会话邮箱与查询邮箱不匹配");
  }

  const inboxResponse = await apiGet("/get-inbox", {
    email: address,
    token: apiToken,
  });
  if (!inboxResponse.ok) {
    throw new Error(`internxt: /get-inbox 失败 http ${inboxResponse.status}`);
  }
  const rawText = await inboxResponse.text();
  let list: Array<Record<string, unknown>>;
  if (!rawText.trim()) {
    list = [];
  } else {
    let parsed: unknown;
    try {
      parsed = JSON.parse(rawText);
    } catch (err) {
      throw new Error(`internxt: 解析收件箱响应失败: ${err}`);
    }
    if (!Array.isArray(parsed)) {
      throw new Error("internxt: 收件箱响应不是顶层数组");
    }
    list = parsed;
  }

  const emails: Email[] = [];
  for (const item of list) {
    const m: Record<string, unknown> = { ...item };
    const mid = messageIdOf(m);
    if (mid) {
      try {
        const dresp = await apiGet("/get-message", {
          email: address,
          token: apiToken,
          messageId: mid,
        });
        if (dresp.ok) {
          const detail = (await dresp.json()) as Record<string, unknown>;
          if (detail && typeof detail === "object") {
            /* 列表字段优先，详情仅补齐缺失字段 */
            for (const [k, v] of Object.entries(detail)) {
              if (!(k in m)) m[k] = v;
            }
          }
        }
      } catch {
        /* 详情失败回退列表摘要 */
      }
    }
    emails.push(normalizeEmail(m, email));
  }
  return emails;
}