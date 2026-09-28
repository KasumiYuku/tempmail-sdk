/**
 * TempMailGG 渠道实现（temp-mail.gg，Laravel + Livewire v3）
 *
 * 会话模型：Livewire v3 协议，POST /livewire/update 为唯一边界。
 * 顶层 _token = 首页 data-csrf 属性令牌；components[0] 携带
 * {snapshot, updates:{}, calls:[{path,method,params}]}。
 * generateEmail 生成邮箱并回传新快照；轮询（calls 为空）等价平台
 * 20 秒自动刷新形态；selectEmail(params:[<数字id>]) 拉取详情视图。
 *
 * 平台会话轮换：Laravel 每次 livewire/update 都轮换会话 Cookie，同值
 * 重放会 419。npm 端无全局 Cookie 罐，本渠道以模块级私有 Cookie 存储
 * （仅本文件内共享）模拟浏览器语义：请求携带 Cookie 头，响应
 * Set-Cookie（temp_email、tempmail_session、XSRF-TOKEN 等）就地覆写。
 *
 * 平台免费额度为免登录每时段 5 个，耗尽时响应快照无 email。
 */

import { InternalEmailInfo, Email, Channel } from "../types";
import { normalizeEmail } from "../normalize";
import { fetchWithTimeout } from "../retry";

const CHANNEL: Channel = "temp-mail-gg";
const BASE_URL = "https://temp-mail.gg";

/* 浏览器形态 UA（与本端 tempmail-ee 渠道一致） */
const USER_AGENT =
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

/* 首页文本 HTML Accept（浏览器导航形态） */
const TEXT_HTML_ACCEPT =
  "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8";

/* 首页 data-csrf / wire:snapshot 提取正则 */
const DATA_CSRF_RE = /data-csrf="([^"]*)"/i;
const SNAPSHOT_RE = /wire:snapshot="([^"]*)"/i;

/* Token 前缀，用于识别本渠道会话凭据串 */
const TOKEN_PREFIX = "temp-mail-gg|";

/* 相对时间解析："N second/minute/hour/day[s] ago" */
const RELATIVE_RE = /^\s*(\d+)\s+(second|minute|hour|day)s?\s+ago\s*$/i;

/** 凭据串内的会话结构 */
interface Session {
  email?: string;
  csrf?: string;
  snapshot?: string;
}

/** livewire/update 响应组件外壳 */
interface UpdateComponent {
  snapshot?: string;
  effects?: {
    returns?: unknown[];
    html?: string;
  };
}

interface UpdateResponse {
  components?: UpdateComponent[];
}

/** 列表条目（从轮询响应的 effects.html 解析） */
interface ListRow {
  id: string;
  from: string;
  subject: string;
  preview: string;
  when: string;
}

/* —— 模块级私有 Cookie 存储（仅本渠道文件内共享，含 Set-Cookie 轮换覆写） —— */
const cookieJar = new Map<string, string>();

/** 当前 Cookie 存储序列化为请求 Cookie 头 */
function cookieHeader(): string {
  return [...cookieJar.entries()].map(([k, v]) => `${k}=${v}`).join("; ");
}

/** 从响应 Set-Cookie 提取键值并覆写本渠道 Cookie 存储 */
function absorbCookies(response: Response): void {
  const setCookies = response.headers.getSetCookie
    ? response.headers.getSetCookie()
    : [];
  for (const line of setCookies) {
    const first = (line.split(";")[0] ?? "").trim();
    const eq = first.indexOf("=");
    if (eq <= 0) continue;
    const key = first.slice(0, eq).trim();
    const value = first.slice(eq + 1).trim();
    /* 撤回态（空值或 1970 过期）按浏览器语义移除键 */
    if (/expires\s*=\s*thu,\s*01\s+jan\s+1970/i.test(line) || value === "") {
      cookieJar.delete(key);
      continue;
    }
    cookieJar.set(key, value);
  }
}

/** 错误信息中的响应体摘要（截断防超长） */
function bodySnippet(raw: string): string {
  return raw.trim().slice(0, 160);
}

/**
 * 反转义 HTML 属性态文本（wire:snapshot 属性值为实体转义态，
 * 参照 Go 端 html.UnescapeString 逐层反转义后再 parse 使用）。
 */
function unescapeHtmlAttr(input: string): string {
  return input
    .replace(/&quot;/g, '"')
    .replace(/&#34;/g, '"')
    .replace(/&apos;|&#39;/g, "'")
    .replace(/&lt;|&#60;/g, "<")
    .replace(/&gt;|&#62;/g, ">")
    .replace(/&amp;/g, "&");
}

/** livewire/update 同步请求头（与 Go 端同站 fetch 全套一致） */
function postHeaders(): Record<string, string> {
  return {
    "User-Agent": USER_AGENT,
    Accept: "text/html, application/xhtml+xml",
    "Accept-Language": "en-US,en;q=0.9",
    "Content-Type": "application/json",
    "X-Livewire": "",
    "X-Requested-With": "XMLHttpRequest",
    Origin: BASE_URL,
    Referer: `${BASE_URL}/`,
  };
}

/** 带 Cookie 存储的本渠道请求封装：请求携带 Cookie，响应吸收 Set-Cookie */
async function request(
  path: string,
  headers: Record<string, string>,
  body?: string,
): Promise<Response> {
  const finalHeaders = { ...headers };
  const cookie = cookieHeader();
  if (cookie) finalHeaders.Cookie = cookie;
  const response = await fetchWithTimeout(`${BASE_URL}${path}`, {
    method: body === undefined ? "GET" : "POST",
    headers: finalHeaders,
    ...(body === undefined ? {} : { body }),
    credentials: "omit",
  });
  absorbCookies(response);
  return response;
}

/**
 * 调用 livewire/update（本渠道 Cookie 存储自动吞新会话 Cookie）。
 * @param snapshot 当前组件快照 JSON 串
 * @param csrf data-csrf 令牌（顶层 _token）
 * @param method 组件方法（"" 表示纯轮询，即平台 20s 自动刷形态）
 * @param params 方法参数
 */
async function update(
  snapshot: string,
  csrf: string,
  method: string,
  params: unknown[] | null,
): Promise<UpdateResponse> {
  const calls: Array<Record<string, unknown>> = [];
  if (method !== "") {
    calls.push({ path: "", method, params });
  }
  const payload = JSON.stringify({
    _token: csrf,
    components: [{ snapshot, updates: {}, calls }],
  });
  const response = await request("/livewire/update", postHeaders(), payload);
  const body = await response.text();
  if (response.status === 419) {
    throw new Error("temp-mail-gg: livewire 会话过期（419），请重新 Generate");
  }
  if (response.status < 200 || response.status >= 300) {
    throw new Error(
      `temp-mail-gg: livewire/update http ${response.status}: ${bodySnippet(body)}`,
    );
  }
  let out: UpdateResponse;
  try {
    out = JSON.parse(body) as UpdateResponse;
  } catch (error: any) {
    throw new Error(
      `temp-mail-gg: 解析 update 响应失败: ${error?.message || error}`,
    );
  }
  return out;
}

/** 解析快照 JSON，提取 data.email（缺失返回空串） */
function snapshotEmail(raw: string): string {
  let snap: { data?: Record<string, unknown> };
  try {
    snap = JSON.parse(raw) as { data?: Record<string, unknown> };
  } catch {
    return "";
  }
  const email = snap.data?.email;
  return typeof email === "string" ? email.trim() : "";
}

/**
 * 创建 temp-mail.gg 临时邮箱。
 * GET 首页取 CSRF + 初始快照 -> update calls=generateEmail ->
 * 从响应快照取 data.email，并把 {email, csrf, snapshot} 打成凭据串。
 */
export async function generateEmail(): Promise<InternalEmailInfo> {
  const boot = await request("", {
    "User-Agent": USER_AGENT,
    Accept: TEXT_HTML_ACCEPT,
    "Accept-Language": "en-US,en;q=0.9",
  });
  const page = await boot.text();
  if (boot.status < 200 || boot.status >= 300) {
    throw new Error(
      `temp-mail-gg: 首页 http ${boot.status}: ${bodySnippet(page)}`,
    );
  }

  const mCsrf = DATA_CSRF_RE.exec(page);
  const mSnap = SNAPSHOT_RE.exec(page);
  if (!mCsrf || !mSnap) {
    throw new Error(
      "temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱",
    );
  }
  const csrf = mCsrf[1];
  /* HTML 属性内的快照 JSON 是实体转义态，先反转义再使用 */
  const snapRaw = unescapeHtmlAttr(mSnap[1]);

  /* generateEmail：平台免费额度为免登录每时段 5 个，耗尽时响应无 email */
  const updateResp = await update(snapRaw, csrf, "generateEmail", []);
  const components = Array.isArray(updateResp.components)
    ? updateResp.components
    : [];
  const newSnapshot = components[0]?.snapshot;
  if (!newSnapshot) {
    throw new Error("temp-mail-gg: generateEmail 响应异常（components 缺失）");
  }
  const email = snapshotEmail(newSnapshot);
  if (!email) {
    throw new Error(
      "temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录配额（每时段 5 个）",
    );
  }

  const token =
    TOKEN_PREFIX + JSON.stringify({ email, csrf, snapshot: newSnapshot });
  return {
    channel: CHANNEL,
    email,
    token,
    expiresAt: new Date(Date.now() + 30 * 60 * 1000).toISOString(),
  };
}

/** 解析凭据串 -> 会话（email + csrf + 快照） */
function decodeSession(token: string): Session {
  if (!token.startsWith(TOKEN_PREFIX)) {
    throw new Error("temp-mail-gg: 凭据串前缀不符，请重新 Generate");
  }
  let sess: Session;
  try {
    sess = JSON.parse(token.slice(TOKEN_PREFIX.length)) as Session;
  } catch (error: any) {
    throw new Error(
      `temp-mail-gg: 解析凭据串失败: ${error?.message || error}`,
    );
  }
  if (!sess.email || !sess.snapshot) {
    throw new Error("temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate");
  }
  return sess;
}

/** 去除 HTML 标签并压缩空白，得到纯文本 */
function stripTags(html: string): string {
  return html.replace(/<[^>]*>/g, " ").replace(/\s+/g, " ").trim();
}

/** 从 start 起截取 div 块直至其配对闭合标签（平衡 div 开闭），失败返回 null */
function sliceDivBlock(html: string, start: number): string | null {
  let depth = 0;
  let i = start;
  while (i < html.length) {
    const open = html.indexOf("<div", i);
    const close = html.indexOf("</div>", i);
    if (open < 0 && close < 0) return null;
    if (open >= 0 && (close < 0 || open < close)) {
      depth++;
      i = open + 4;
    } else {
      depth--;
      i = close + "</div>".length;
      if (depth === 0) return html.slice(start, i);
    }
  }
  return null;
}

/**
 * 用正则解析轮询响应 effects.html 的 Inbox 条目。
 * 条目容器为 <div wire:click="selectEmail(<数字id>)">，
 * 其内 h3(font-semibold)=发件人、p(text-zinc-300)=主题、
 * p(line-clamp-2)=正文预览、span(text-xs)=相对时间。无条目返回空数组。
 */
function parseList(htmlBlock: string): ListRow[] {
  const rows: ListRow[] = [];
  const clickRe = /<div\b[^>]*wire:click="selectEmail\((\d+)\)"[^>]*>/gi;
  let m: RegExpExecArray | null;
  while ((m = clickRe.exec(htmlBlock)) !== null) {
    const id = m[1];
    const block = sliceDivBlock(htmlBlock, m.index);
    if (!block) continue;

    const row: ListRow = { id, from: "", subject: "", preview: "", when: "" };
    const first = (...res: RegExp[]): string => {
      for (const re of res) {
        const mt = re.exec(block);
        if (mt && stripTags(mt[1]) !== "") return stripTags(mt[1]);
      }
      return "";
    };

    /* 模板中条目块结构：h3/p(line-clamp-2)/p(text-zinc-300)/span(text-xs) */
    row.from = first(/<h3\b[^>]*>([\s\S]*?)<\/h3>/i);
    row.preview = first(
      /<p\b[^>]*class="[^"]*\bline-clamp-2\b[^"]*"[^>]*>([\s\S]*?)<\/p>/i,
    );
    row.subject = first(
      /<p\b[^>]*class="[^"]*\btext-zinc-300\b[^"]*"[^>]*>([\s\S]*?)<\/p>/i,
    );
    row.when = first(
      /<span\b[^>]*class="[^"]*\btext-xs\b[^"]*"[^>]*>([\s\S]*?)<\/span>/i,
    );
    rows.push(row);
  }
  return rows;
}

/** 解析 selectEmail 详情视图（模态框）：主题/发件人/纯文本正文 */
function parseDetail(
  htmlBlock: string,
): { from: string; subject: string; text: string } {
  /* x-show 含 activeTab === 'text' 的 div 即纯文本正文页签容器 */
  const textMatch = /<div\b[^>]*x-show="[^"]*activeTab\s*===\s*'text'[^"]*"[^>]*>([\s\S]*?)<\/div>/i.exec(
    htmlBlock,
  );
  const text = textMatch ? stripTags(textMatch[1]) : "";

  /* h3(class 含 text-xl)=主题；span 文本以 "From:" 开头=发件人（去掉前缀） */
  const subjectMatch = /<h3\b[^>]*class="[^"]*\btext-xl\b[^"]*"[^>]*>([\s\S]*?)<\/h3>/i.exec(
    htmlBlock,
  );
  const subject = subjectMatch ? stripTags(subjectMatch[1]) : "";
  let from = "";
  for (const m of htmlBlock.matchAll(/<span\b[^>]*>([\s\S]*?)<\/span>/gi)) {
    const textInside = stripTags(m[1]);
    if (textInside.startsWith("From:")) {
      from = textInside.slice("From:".length).trim();
      break;
    }
  }
  return { from, subject, text };
}

/** 解析相对时间（"N seconds/minutes/hours/days ago"）为 ISO 串，失败用当前时间 */
function parseRelative(when: string, now: Date): string {
  const m = RELATIVE_RE.exec(when.trim());
  if (!m) return now.toISOString();
  const n = parseInt(m[1], 10);
  if (Number.isNaN(n)) return now.toISOString();
  let ms = 0;
  switch (m[2].toLowerCase()) {
    case "second":
      ms = 1000;
      break;
    case "minute":
      ms = 60 * 1000;
      break;
    case "hour":
      ms = 60 * 60 * 1000;
      break;
    case "day":
      ms = 24 * 60 * 60 * 1000;
      break;
  }
  return new Date(now.getTime() - n * ms).toISOString();
}

/** 列表行转归一化骨架（详情前的基础归一，id 空返回 null） */
function listToNorm(
  row: ListRow,
  email: string,
  now: Date,
): Record<string, unknown> | null {
  if (!row.id) return null;
  return {
    id: row.id,
    from: row.from,
    to: email,
    subject: row.subject,
    text: row.preview,
    html: "",
    date: parseRelative(row.when, now),
    isRead: false,
  };
}

/**
 * 读取 temp-mail.gg 收件箱。
 * @param email 邮箱地址（与 token 内会话邮箱一致才继续）
 * @param token 凭据串（prefix | {email,csrf,snapshot}）
 * 流程：轮询 update(无 calls) 取 Inbox 列表 -> 对每封 selectEmail 提详情，
 * 详情失败回退列表字段（不中断整批）。轮询响应快照中的 data.email
 * 与请求邮箱不一致时报错（防 Cookie 存储被并行会话覆盖后串箱）。
 */
export async function getEmails(email: string, token: string): Promise<Email[]> {
  const address = String(email || "").trim();
  if (!address) {
    throw new Error("temp-mail-gg: 邮箱为空，请重新 Generate");
  }
  const sess = decodeSession(String(token || ""));
  if (!sess.email || !sess.snapshot) {
    throw new Error("temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate");
  }
  if (sess.email.toLowerCase() !== address.toLowerCase()) {
    throw new Error(
      `temp-mail-gg: 邮箱与凭据不匹配（${sess.email} != ${address}），请重新 Generate`,
    );
  }

  /* 轮询刷新（calls 为空 = 平台 20s 自动刷新形态） */
  const poll = await update(sess.snapshot, sess.csrf ?? "", "", []);
  const components = Array.isArray(poll.components) ? poll.components : [];
  if (components.length === 0) {
    throw new Error("temp-mail-gg: 轮询响应异常（components 缺失）");
  }
  const c0 = components[0];
  if (c0.snapshot) {
    const current = snapshotEmail(c0.snapshot);
    if (current !== "" && current.toLowerCase() !== address.toLowerCase()) {
      throw new Error(
        `temp-mail-gg: 会话已被切换至 ${current}（与请求邮箱 ${address} 不一致），请重新 Generate`,
      );
    }
  }
  const htmlBlock = c0.effects?.html ?? "";
  if (htmlBlock.trim() === "") {
    throw new Error("temp-mail-gg: 轮询响应无 effects.html，会话可能已失效");
  }

  const listRows = parseList(htmlBlock);
  if (listRows.length === 0) return [];

  /* 逐封拉详情正文；失败回退列表字段（不中断整批） */
  let latestSnap = c0.snapshot ?? sess.snapshot ?? "";
  const now = new Date();
  const emails: Email[] = [];
  for (const row of listRows) {
    const skeleton = listToNorm(row, address, now);
    if (!skeleton) continue;

    const idNum = parseInt(row.id, 10);
    if (!Number.isNaN(idNum)) {
      try {
        const respDetail = await update(
          latestSnap,
          sess.csrf ?? "",
          "selectEmail",
          [idNum],
        );
        const detailComponents = Array.isArray(respDetail.components)
          ? respDetail.components
          : [];
        if (detailComponents.length > 0) {
          if (detailComponents[0].snapshot) {
            latestSnap = detailComponents[0].snapshot;
          }
          const detail = parseDetail(
            detailComponents[0].effects?.html ?? "",
          );
          if (detail.from) skeleton.from = detail.from;
          if (detail.subject) skeleton.subject = detail.subject;
          if (detail.text) skeleton.text = detail.text;
        }
      } catch {
        /* 详情节选失败回退列表字段 */
      }
    }

    if (!skeleton.text) skeleton.text = skeleton.subject;
    if (!skeleton.html) {
      const escaped = String(skeleton.text)
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;");
      skeleton.html = `<html><body><pre>${escaped}</pre></body></html>`;
    }
    emails.push(normalizeEmail(skeleton, address));
  }
  return emails;
}