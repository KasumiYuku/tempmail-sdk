/**
 * Mailticking 渠道实现（www.mailticking.com，旧域名 temporary-mail.net 的更名站）
 *
 * 实测协议（JSON 交流、Cloudflare 前置、需浏览器 UA）：
 *   1. 建箱  POST /get-mailbox   body {"types":["4"]}（4=独立域名；排除
 *      Gmail 别名（type "2"））
 *      响应 {"success":true,"email":"xxx@domain","activate_token":"..."}
 *   2. 激活  POST /activate-email body {"email":..,"source":"homepage",
 *      "activate_token":..}      响应 {"success":true}，并下发
 *      active_mailbox / temp_mail_history Cookie
 *   3. 同步  GET /?email=..&activate_token=..  服务端把该邮箱写入活跃状态，
 *      页面 #active-mail 渲染 value="{email}" data-code="{64位列信码}"；
 *      列信码是"当前活跃邮箱"的派生凭据，与 activate_token 是两套值
 *   4. 列信  POST /get-emails?lang=en body {"email":..,"code":列信码}
 *      空箱实测响应 {"emails":[],"success":true}；`?lang=` 缺省直接 400；
 *      邮箱不活跃（code 失效）时回 {"error":"Invalid request","success":false}
 *
 * token 语义：单字符串 "{列信码}|{email}"，生成时已与服务器同步。
 *
 * 读信协议：官网没有可观察的独立读信端点，当前实现只提供列表；
 * 列表元素的字段名没有站点文档佐证，不做任何猜测，交给 normalizeEmail 的
 * 既有候选字段策略提取。待详情端点或字段结构有明确证据后再升级。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "mailticking";
const BASE_URL = "https://www.mailticking.com";

const USER_AGENT =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36";

/** 从首页 HTML 中抽取 #active-mail 输入框起始标签 */
const INPUT_TAG_RE = /<input\b[^>]*\bid=['"]active-mail['"][^>]*>/s;
/** 在输入框标签内抽取 value（邮箱）与 data-code（列信码） */
const VALUE_RE = /\bvalue=['"]([^'"]*)['"]/;
const DATA_CODE_RE = /\bdata-code=['"]([^'"]*)['"]/;

/** 候选发件人键：列表元素字段全名没有站点文档佐证，按常见字段多候选提取 */
const FROM_KEYS = [
  "mail_from",
  "from_mail",
  "from_email",
  "sender_address",
  "from_address",
  "send_addr",
  "mail_addr",
  "address_from",
  "ho_from",
  "fromname",
  "fromS",
];

interface ApiResponse {
  success?: boolean;
  error?: string;
  message?: string;
  email?: string;
  code?: string;
  activate_token?: string;
  needNewEmail?: boolean;
  emails?: Array<Record<string, unknown>>;
}

/** 从响应中提取 error 优先、message 次之的错误文案 */
function errorText(data: Partial<ApiResponse>, fallback: string): string {
  if (typeof data.error === "string" && data.error.trim()) {
    return data.error.trim();
  }
  if (typeof data.message === "string" && data.message.trim()) {
    return data.message.trim();
  }
  return fallback;
}

/** 执行 POST JSON 请求并解析响应；非 2xx 时优先使用响应体中的错误文案 */
async function post(path: string, payload: unknown): Promise<ApiResponse> {
  const response = await fetchWithTimeout(`${BASE_URL}${path}`, {
    method: "POST",
    headers: {
      Accept: "application/json",
      "Accept-Language": "en-US,en;q=0.9",
      "Content-Type": "application/json",
      "User-Agent": USER_AGENT,
      Referer: `${BASE_URL}/`,
      Origin: BASE_URL,
    },
    body: JSON.stringify(payload),
  });
  const body = await response.text();
  let data: ApiResponse = {};
  try {
    data = JSON.parse(body) as ApiResponse;
  } catch {
    /* 站点偶尔返回非 JSON 网关文案，保留原样供错误提示使用 */
    data = {};
  }
  if (!response.ok) {
    throw new Error(
      `mailticking: http ${response.status}: ${errorText(data, body.trim())}`,
    );
  }
  return data;
}

/** 带参 GET 首页文本，用于抽取列信码 */
async function getPage(url: string): Promise<string> {
  const response = await fetchWithTimeout(url, {
    headers: {
      Accept:
        "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
      "Accept-Language": "en-US,en;q=0.9",
      "User-Agent": USER_AGENT,
    },
  });
  if (!response.ok) {
    throw new Error(`mailticking: http ${response.status}`);
  }
  return response.text();
}

/** 从首页 HTML 解析活跃邮箱 value 与 data-code，失败返回 null */
function parseActiveMail(page: string): { email: string; code: string } | null {
  const tag = INPUT_TAG_RE.exec(page);
  if (!tag) {
    return null;
  }
  const value = VALUE_RE.exec(tag[0]);
  const code = DATA_CODE_RE.exec(tag[0]);
  if (!value || !code) {
    return null;
  }
  const pair = { email: value[1].trim(), code: code[1].trim() };
  if (!pair.email || !pair.code) {
    return null;
  }
  return pair;
}

/*
 * 执行激活后的会话同步：以查询参数加载首页，从 #active-mail 解析
 * value（邮箱）与 data-code（列信码）。失败抛出异常。
 */
async function syncCode(
  email: string,
  activateToken: string,
): Promise<{ email: string; code: string }> {
  const url =
    `${BASE_URL}/?email=${encodeURIComponent(email)}` +
    `&activate_token=${encodeURIComponent(activateToken)}`;
  const page = await getPage(url);
  const pair = parseActiveMail(page);
  if (!pair) {
    throw new Error("mailticking: sync inbox code failed");
  }
  return pair;
}

/** 从 token 拆出列信码与邮箱；无分隔符时整个 token 视为列信码 */
function splitToken(token: string, email: string): [string, string] {
  const at = token.indexOf("|");
  if (at >= 0) {
    return [token.slice(0, at).trim(), token.slice(at + 1).trim()];
  }
  return [token.trim(), email.trim()];
}

/** 尽量映射列表字段到统一字段名，未命中的字段留给 normalizeEmail 自己处理 */
function migratedFields(raw: Record<string, unknown>): Record<string, unknown> {
  const m: Record<string, unknown> = { ...raw };
  if (m.from === undefined || m.from === null) {
    for (const key of FROM_KEYS) {
      const v = m[key];
      if (typeof v === "string" && v.trim()) {
        m.from = v;
        break;
      }
    }
  }
  if (m.sender === undefined || m.sender === null) {
    const f = m.from;
    if (typeof f === "string" && f.trim()) {
      m.sender = f;
    }
  }
  if ((m.id === undefined || m.id === null) && m.mail_id !== undefined && m.mail_id !== null) {
    m.id = m.mail_id;
  }
  if ((m.date === undefined || m.date === null) && m.received_at !== undefined && m.received_at !== null) {
    m.date = m.received_at;
  }
  return m;
}

/**
 * 创建 mailticking 邮箱账号（type=4 独立域名）。
 * 流程：建箱 → 激活 → 带参加载首页同步出列信码；token 存 "{列信码}|{email}"。
 */
export async function generateEmail(): Promise<InternalEmailInfo> {
  const box = await post("/get-mailbox", { types: ["4"] });
  if (!box.success) {
    throw new Error(
      `mailticking: get-mailbox failed: ${errorText(box, "unknown error")}`,
    );
  }
  const boxEmail = String(box.email || "").trim();
  const activateToken = String(box.activate_token || "").trim();
  if (!boxEmail) {
    throw new Error("mailticking: get-mailbox returned empty email");
  }
  if (!activateToken) {
    throw new Error("mailticking: get-mailbox returned empty activate_token");
  }

  /* 激活邮箱，服务端据此下发 active_mailbox Cookie 并登记活跃状态 */
  const act = await post("/activate-email", {
    email: boxEmail,
    source: "homepage",
    activate_token: activateToken,
  });
  if (!act.success) {
    throw new Error(
      `mailticking: activate-email failed: ${errorText(act, "unknown error")}`,
    );
  }

  /* 会话同步：带参加载首页，取列信码（列信必需的最新派生凭据） */
  const pair = await syncCode(boxEmail, activateToken);
  if (pair.email !== boxEmail) {
    throw new Error(
      `mailticking: inbox session mismatch: want ${boxEmail} got ${pair.email}`,
    );
  }

  return {
    channel: CHANNEL,
    email: boxEmail,
    token: `${pair.code}|${boxEmail}`,
  };
}

/**
 * 获取 mailticking 邮箱的邮件列表。
 * 发送 {"email":邮箱, "code":token 中的列信码}，空箱返回空列表不报错。
 * 限制说明：读信正文无公开端点，仅提供列表；字段名候选提取，能识别
 * 多少字段取决于站点实际响应。
 */
export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address) {
    throw new Error("mailticking: empty email");
  }
  const tokenRaw = String(token || "").trim();
  if (!tokenRaw) {
    throw new Error("mailticking: empty token");
  }
  const [code, tokEmail] = splitToken(tokenRaw, address);

  const list = await post("/get-emails?lang=en", {
    email: tokEmail,
    code,
  });
  if (!list.success) {
    if (list.needNewEmail) {
      throw new Error("mailticking: mailbox expired, please renew");
    }
    if (tokEmail === address) {
      /* 无 activate_token 可重放：直接报错，避免误把邮箱判死 */
      throw new Error(
        "mailticking: get-emails rejected, refresh inbox in Generate",
      );
    }
    throw new Error("mailticking: get-emails failed");
  }
  if (tokEmail !== address) {
    throw new Error("mailticking: token email mismatch");
  }

  const rows = Array.isArray(list.emails) ? list.emails : [];
  const out: Email[] = [];
  for (const raw of rows) {
    if (!raw || typeof raw !== "object") {
      continue;
    }
    out.push(normalizeEmail(migratedFields(raw), address));
  }
  return out;
}