"""
TempMailGG 渠道实现（temp-mail.gg，Laravel + Livewire v3）

调研实证结论（2026-09-27，与 Go 端 temp_mail_gg.go 一致）：
  - GET https://temp-mail.gg/ 返回 data-csrf 属性令牌、XSRF-TOKEN /
    tempmail_session cookies 与初始 wire:snapshot（JSON；data.email
    初始为空，邮箱必须由 Livewire generateEmail 显式生成）。
  - POST /livewire/update 是唯一边界（同站 fetch 协议）：JSON body
    顶层 _token（=data-csrf）+ components[0]：{snapshot, updates:{},
    calls:[{path:"",method:"<call>",params:[...]}]}。generateEmail
    生成邮箱并回传新 snapshot，响应 components[0].effects.html 含
    收件箱整块 UI。200 且 calls 为空的 update 即平台轮询刷信形态。
  - Livewire 会话轮换：Laravel 每次 livewire/update 都轮换会话
    Cookie；同值重放会 419，必须以响应 Set-Cookie 覆写后续请求。
  - 详情：update calls=[{method:"selectEmail",params:[<数字id>]}]，
    响应 effects.html 的邮件模态框含 From / 主题 / 正文全文
    （x-show="activeTab === 'text'" 区块）。

会话隔离：Cookie 轮换要求本渠道独立维护 Cookie 罐，若走 tm_http
  全局 Session 会污染其它渠道且丢粘性；本模块维护专属
  requests.Session（代理/TLS/自定义头从 ..config 读取应用），
  Cookie 由本渠道独占。

Token 语义：邮箱约 30 分钟过期，需重新 Generate。
"""

import html
import json
import re
from datetime import datetime, timedelta, timezone
from typing import List, Optional

import requests

from ..config import get_config
from ..html_utils import html_to_text
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "temp-mail-gg"
BASE_URL = "https://temp-mail.gg"

# 固定浏览器 UA（与 Go 端一致，平台无严格指纹校验）
_USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"
)

# Token 前缀，用于识别本渠道会话凭据串
TOKEN_PREFIX = "temp-mail-gg|"

_SNAPSHOT_RE = re.compile(r'wire:snapshot="([^"]*)"')
_DATA_CSRF_RE = re.compile(r'data-csrf="([^"]*)"')

# selectEmail(<数字id>) 列表条目容器
_LIST_ENTRY_RE = re.compile(
    r'<div\b[^>]*wire:click="selectEmail\((\d+)\)"[^>]*>'
)
# 相对时间（平台列表条目的 span 文本）
_REL_TIME_RE = re.compile(
    r"^\s*(\d+)\s+(second|minute|hour|day)s?\s+ago\s*$", re.I
)

# 模块级专属会话（Cookie 由本渠道独占，随 update 响应轮换）
_sess: Optional[requests.Session] = None


def _session() -> requests.Session:
    """
    模块级专属 requests.Session（单例复用，粘住 Cookie 会话）

    代理 / TLS 校验 / 自定义头从 ..config 读取应用，配置来源与
    http.get_session() 一致；不复用全局 Session 是因为 Livewire 每次
    update 都轮换会话 Cookie，本渠道必须独占一个 Cookie 罐。
    """
    global _sess
    if _sess is None:
        config = get_config()
        _sess = requests.Session()
        if config.proxy:
            _sess.proxies = {"http": config.proxy, "https": config.proxy}
        _sess.verify = not config.insecure
        if config.headers:
            _sess.headers.update(config.headers)
    return _sess


def _post_headers() -> dict:
    """设置 livewire/update 同步请求头（同站 fetch 全套，与 Go 端一致）"""
    return {
        "User-Agent": _USER_AGENT,
        "Accept": "text/html, application/xhtml+xml",
        "Accept-Language": "en-US,en;q=0.9",
        "Content-Type": "application/json",
        "X-Livewire": "",
        "X-Requested-With": "XMLHttpRequest",
        "Origin": BASE_URL,
        "Referer": BASE_URL + "/",
    }


def _page_lookup() -> tuple:
    """GET 首页并提取（data-csrf, 初始 wire:snapshot），快照先做一层 HTML 反转义"""
    config = get_config()
    resp = _session().get(
        BASE_URL,
        headers={
            "User-Agent": _USER_AGENT,
            "Accept": (
                "text/html,application/xhtml+xml,application/xml;q=0.9,"
                "image/avif,image/webp,*/*;q=0.8"
            ),
            "Accept-Language": "en-US,en;q=0.9",
        },
        timeout=config.timeout,
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"temp-mail-gg: 首页 http {resp.status_code}: {resp.text.strip()[:200]}"
        )
    page = resp.text
    m_csrf = _DATA_CSRF_RE.search(page)
    m_snap = _SNAPSHOT_RE.search(page)
    if not m_csrf or not m_snap:
        raise RuntimeError(
            "temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱"
        )
    # HTML 属性内的快照 JSON 是双转义态（&quot;），逐层反转义为原始 JSON
    return m_csrf.group(1), html.unescape(m_snap.group(1))


def _update(snapshot, csrf: str, method: str, params) -> dict:
    """
    调用 livewire/update（同一 Cookie 会话，响应轮换 Cookie 由 Session 自动接住）

    快照参数接收 str（协议原生形态）或 dict（token 内保存的快照对象），
    构造 payload 时统一序列化为 JSON 字符串，与 Livewire v3 协议一致。
    """
    snap_str = snapshot if isinstance(snapshot, str) else json.dumps(snapshot)
    calls = []
    if method:
        calls = [{"path": "", "method": method, "params": params or []}]
    payload = {
        "_token": csrf,
        "components": [
            {"snapshot": snap_str, "updates": {}, "calls": calls},
        ],
    }
    config = get_config()
    resp = _session().post(
        f"{BASE_URL}/livewire/update",
        headers=_post_headers(),
        data=json.dumps(payload),
        timeout=config.timeout,
    )
    if resp.status_code == 419:
        raise RuntimeError(
            "temp-mail-gg: livewire 会话过期（419），请重新 Generate"
        )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"temp-mail-gg: livewire/update http {resp.status_code}: "
            f"{resp.text.strip()[:200]}"
        )
    return resp.json()


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 temp-mail.gg 临时邮箱（首页取 CSRF + 快照 → generateEmail → 响应快照取 email）"""
    csrf, snap_raw = _page_lookup()

    update_resp = _update(snap_raw, csrf, "generateEmail", None)
    components = update_resp.get("components") or []
    if not components or not (components[0].get("snapshot") or ""):
        raise RuntimeError(
            "temp-mail-gg: generateEmail 响应异常（components 缺失）"
        )
    try:
        snap2 = json.loads(components[0]["snapshot"])
    except (ValueError, TypeError) as err:
        raise RuntimeError(f"temp-mail-gg: 解析响应快照失败: {err}") from err

    # 平台免费额度为免登录每时段 5 个，耗尽时响应快照无 email
    email = ((snap2.get("data") or {}).get("email") or "").strip()
    if not email:
        raise RuntimeError(
            "temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录配额（每时段 5 个）"
        )

    token = TOKEN_PREFIX + json.dumps(
        {"email": email, "csrf": csrf, "snapshot": snap2},
        ensure_ascii=False,
    )
    return EmailInfo(channel=channel, email=email, _token=token)


def _decode_session(token: str) -> dict:
    """解析凭据串 → 会话（email + csrf + snapshot），校验前缀与必需字段"""
    if not (token or "").startswith(TOKEN_PREFIX):
        raise ValueError("temp-mail-gg: 凭据串前缀不符，请重新 Generate")
    try:
        sess = json.loads(token[len(TOKEN_PREFIX):])
    except ValueError as err:
        raise ValueError(f"temp-mail-gg: 解析凭据串失败: {err}") from err
    if not (sess.get("email") or "").strip() or not sess.get("snapshot"):
        raise ValueError("temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate")
    return sess


def _parse_list(html_block: str) -> list:
    """
    用正则解析轮询响应 effects.html 的收件箱条目（无 lxml 依赖）

    条目容器为 <div wire:click="selectEmail(<数字id>)">，其内
    h3=发件人邮箱、p(text-zinc-300)=主题、p(line-clamp-2)=正文预览、
    span(text-xs)=相对时间。
    """
    rows = []
    for m in _LIST_ENTRY_RE.finditer(html_block):
        entry_id = m.group(1)
        start = m.start()
        rest = html_block[start:]
        # 块边界：下一个条目容器或结尾（正文最小化截断，正则受 HTML 嵌套限制）
        next_entry = _LIST_ENTRY_RE.search(html_block, start + 1)
        block = rest if not next_entry else rest[: next_entry.start() - start]
        row = {
            "id": entry_id,
            "from": "",
            "subject": "",
            "preview": "",
            "when": "",
        }
        for class_name, key, tag in (
            ("font-semibold", "from", "h3"),
            ("text-zinc-300", "subject", "p"),
            ("line-clamp-2", "preview", "p"),
            ("text-xs", "when", "span"),
        ):
            tag_m = re.search(
                rf'<\s*{tag}\b[^>]*class="[^"]*\b{re.escape(class_name)}'
                rf'\b[^"]*"[^>]*>(.*?)</\s*{tag}>',
                block,
                re.I | re.S,
            )
            if tag_m and not row[key]:
                row[key] = html_to_text(tag_m.group(1)).strip()
        rows.append(row)
    return rows


def _parse_detail(html_block: str):
    """
    解析 selectEmail 详情视图（模态框）的正文/发件人/主题

    返回 (from, subject, text)；h3(text-xl)=主题，From: 前缀 span=发件人，
    x-show 含 activeTab === 'text' 的 div=正文全文。
    """
    from_addr = ""
    subject = ""
    text = ""
    sub_m = re.search(
        r'<\s*h3\b[^>]*class="[^"]*\btext-xl\b[^"]*"[^>]*>(.*?)</\s*h3>',
        html_block,
        re.I | re.S,
    )
    if sub_m:
        subject = html_to_text(sub_m.group(1)).strip()
    for span_m in re.finditer(r"<\s*span\b[^>]*>(.*?)</\s*span>", html_block, re.I | re.S):
        span_text = html_to_text(span_m.group(1)).strip()
        if span_text.startswith("From:"):
            from_addr = span_text[len("From:"):].strip()
            break
    text_m = re.search(
        r'<\s*div\b[^>]*x-show="[^"]*activeTab === \'text\'[^"]*"[^>]*>'
        r"(.*?)</\s*div>",
        html_block,
        re.I | re.S,
    )
    if text_m:
        text = html_to_text(text_m.group(1)).strip()
    if not subject and not text and not from_addr:
        return "", "", "", False
    return from_addr, subject, text, True


def _parse_relative(value: str, now: datetime) -> str:
    """解析相对时间（"N seconds/minutes/hours/days ago"）为 UTC ISO 时间；失败用当前时间兜底"""
    m = _REL_TIME_RE.match(value.strip())
    if not m:
        return now.isoformat()
    try:
        n = int(m.group(1))
    except ValueError:
        return now.isoformat()
    unit = m.group(2).lower()
    if unit == "second":
        delta = timedelta(seconds=n)
    elif unit == "minute":
        delta = timedelta(minutes=n)
    elif unit == "hour":
        delta = timedelta(hours=n)
    else:
        delta = timedelta(days=n)
    return (now - delta).isoformat()


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """
    读取 temp-mail.gg 收件箱

    流程：轮询 update（calls 为空）取收件箱列表 → 对每封 selectEmail
    拉详情正文；轮询响应快照中的 data.email 与请求邮箱不一致时抛错
    （防本渠道专属 Cookie 罐被并发会话覆盖后串箱）。
    """
    addr = (email or "").strip()
    if not addr:
        raise ValueError("temp-mail-gg: 邮箱为空，请重新 Generate")
    sess = _decode_session(token or "")
    if (sess.get("email") or "").strip().lower() != addr.lower():
        raise ValueError(
            f"temp-mail-gg: 邮箱与凭据不匹配（{sess.get('email')} != {addr}），请重新 Generate"
        )

    # 轮询刷新（calls 为空 = 平台 20s 自动刷新形态）
    resp_poll = _update(sess["snapshot"], sess["csrf"], "", None)
    components = resp_poll.get("components") or []
    if not components:
        raise RuntimeError("temp-mail-gg: 轮询响应异常（components 缺失）")
    c0 = components[0]
    if c0.get("snapshot"):
        try:
            poll_snap = json.loads(c0["snapshot"])
            current = ((poll_snap.get("data") or {}).get("email") or "").strip()
            if current and current.lower() != addr.lower():
                raise RuntimeError(
                    f"temp-mail-gg: 会话已被切换至 {current}（与请求邮箱 {addr} 不一致），请重新 Generate"
                )
        except ValueError as err:
            raise RuntimeError(f"temp-mail-gg: 解析轮询响应快照失败: {err}") from err

    html_block = (c0.get("effects") or {}).get("html") or ""
    if not html_block.strip():
        raise RuntimeError("temp-mail-gg: 轮询响应无 effects.html，会话可能已失效")

    rows = _parse_list(html_block)
    if not rows:
        return []

    # 逐封拉详情；详情失败回退列表字段（不中断整批）
    latest_snap = c0.get("snapshot") or sess["snapshot"]
    now = datetime.now(timezone.utc)
    out: List[Email] = []
    for row in rows:
        flat = {
            "id": row["id"],
            "from": row["from"],
            "to": addr,
            "subject": row["subject"],
            "date": _parse_relative(row["when"], now),
            "text": row["preview"],
            "html": "",
        }
        try:
            resp_detail = _update(
                latest_snap, sess["csrf"], "selectEmail", [int(row["id"])]
            )
            d_components = resp_detail.get("components") or []
            if d_components and d_components[0].get("snapshot"):
                latest_snap = d_components[0]["snapshot"]
            if d_components:
                detail_from, detail_subject, detail_text, ok = _parse_detail(
                    (d_components[0].get("effects") or {}).get("html") or ""
                )
                if ok:
                    if detail_from:
                        flat["from"] = detail_from
                    if detail_subject:
                        flat["subject"] = detail_subject
                    if detail_text:
                        flat["text"] = detail_text
        except Exception:
            # 单封详情拉取失败不阻塞列表其余邮件
            pass
        if not flat["text"]:
            flat["text"] = flat["subject"]
        if not flat.get("html"):
            flat["html"] = (
                "<html><body><pre>" + html.escape(flat["text"]) + "</pre></body></html>"
            )
        out.append(normalize_email(flat, addr))
    return out