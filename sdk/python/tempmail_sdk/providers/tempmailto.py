"""
Tempmailto 渠道实现（tempmailto.com，Laravel）

调研实证结论（2026-09-27，与 Go 端 tempmailto.go 一致）：
  - GET https://tempmailto.com/ 首页建立 Cookie 会话并渲染当前邮箱
    （<input id="mainEmail" value="...">，同一会话内不变），同时输出
    <meta name="csrf-token"> 供读信/换箱使用；建箱只需 GET + 提取。
  - POST /get_messages（表单：_token=<CSRF>、captcha 留空）返回 JSON：
    {status, mailbox, email_token, messages, histories}。
  - 会话粘性：邮箱由会话 Cookie 承载，无独立密钥，Token 约定为邮箱
    本身（注册表以 token 非空作防御）。读信响应的 mailbox 与请求邮箱
    不一致时用 POST /change（_token/name/domain）拉回目标邮箱。
  - 详情：GET /view/{id}（同 Cookie 会话），正文从候选 class 区块
    提取，全失败回退 <main>/<article>。

会话隔离：本渠道全部状态藏在 Laravel 加密 Cookie 中，必须依赖
  Cookie 罐粘性；若走 tm_http 的全局 Session 会与其它渠道串 Cookie，
  因此本模块维护专属 requests.Session（代理/TLS/自定义头从 ..config
  读取应用，与 get_session() 同源），Cookie 由本渠道独占。

Token 语义：邮箱约 10 分钟无活动过期，需重新 Generate。
"""

import html
import re
from datetime import datetime, timezone
from typing import List, Optional

import requests

from ..config import get_config
from ..html_utils import html_to_text
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "tempmailto"
BASE_URL = "https://tempmailto.com"

# 固定浏览器 UA（平台为普通 Laravel 模板，无浏览器指纹校验）
_USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"
)

_ACCEPT_HTML = (
    "text/html,application/xhtml+xml,application/xml;q=0.9,"
    "image/avif,image/webp,*/*;q=0.8"
)
_ACCEPT_AJAX = "application/json, text/plain, */*"

_CSRF_RE = re.compile(r'<meta\s+name="csrf-token"\s+content="([^"]+)"')
_MAIN_EMAIL_RE = re.compile(r'(?is)id="mainEmail"[^>]*\bvalue="([^"]+)"')
_LOCAL_PART_RE = re.compile(r"^[^@]+")

# 模块级专属会话（Cookie 由本渠道独占，防串箱/防污染全局会话）
_sess: Optional[requests.Session] = None


def _session() -> requests.Session:
    """
    模块级专属 requests.Session（单例复用，粘住 Cookie 会话）

    代理 / TLS 校验 / 自定义头从 ..config 读取应用，配置来源与
    http.get_session() 一致；不复用全局 Session 是因为本渠道的
    current 邮箱完全由 Laravel 加密 Cookie 承载，串用会污染其它渠道。
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


def _browser_headers(accept: str, ajax: bool = False) -> dict:
    """设置同站浏览器特征头（与前端同源调用一致）；ajax 时补表单与 XHR 头"""
    headers = {
        "User-Agent": _USER_AGENT,
        "Accept": accept,
        "Accept-Language": "en-US,en;q=0.9",
        "Origin": BASE_URL,
        "Referer": BASE_URL + "/",
    }
    if ajax:
        headers["Content-Type"] = "application/x-www-form-urlencoded; charset=UTF-8"
        headers["X-Requested-With"] = "XMLHttpRequest"
    return headers


def _str_of(value) -> str:
    """接口字段值安全转换为字符串（str trim；int/float 转十进制整数字符串；其余过滤为空串）"""
    if isinstance(value, str):
        return value.strip()
    if isinstance(value, (int, float)):
        return str(int(value))
    return ""


def _as_bool(value) -> bool:
    """is_seen 三态值归一为 bool（兼容 bool/number/string，与 Go 端一致）"""
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return value != 0
    if isinstance(value, str):
        return value.strip() == "1" or value.strip().lower() == "true"
    return False


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 tempmailto.com 临时邮箱（GET 首页建立 Cookie 会话并提取服务端渲染的当前邮箱）"""
    config = get_config()
    resp = _session().get(
        BASE_URL,
        headers=_browser_headers(_ACCEPT_HTML),
        timeout=config.timeout,
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"tempmailto: 首页 http {resp.status_code}: {resp.text.strip()[:200]}"
        )
    m = _MAIN_EMAIL_RE.search(resp.text)
    email = m.group(1).strip() if m else ""
    if not email:
        raise RuntimeError("tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱")
    # Token 约定为邮箱本身（无独立密钥，注册表以 token 非空作防御）
    return EmailInfo(channel=channel, email=email, _token=email)


def _csrf() -> str:
    """GET 首页并提取 CSRF _token（读信/换箱共用，同一 Cookie 会话）"""
    config = get_config()
    resp = _session().get(
        BASE_URL,
        headers=_browser_headers(_ACCEPT_HTML),
        timeout=config.timeout,
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"tempmailto: 首页 http {resp.status_code}: {resp.text.strip()[:200]}"
        )
    m = _CSRF_RE.search(resp.text)
    if not m:
        raise RuntimeError("tempmailto: 首页未找到 csrf-token")
    return m.group(1)


def _fetch_messages() -> dict:
    """POST /get_messages 拉取当前会话邮箱的邮件列表（_token + captcha 留空）"""
    config = get_config()
    resp = _session().post(
        f"{BASE_URL}/get_messages",
        headers=_browser_headers(_ACCEPT_AJAX, ajax=True),
        data={"_token": _csrf(), "captcha": ""},
        timeout=config.timeout,
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"tempmailto 读信: http {resp.status_code}: {resp.text.strip()[:200]}"
        )
    return resp.json()


def _change(email: str) -> str:
    """POST /change 将会话当前邮箱拉回目标邮箱（_token/name/domain），返回变更后的邮箱"""
    name = _LOCAL_PART_RE.match(email)
    name = name.group(0) if name else "TmSdk"
    domain = email.rsplit("@", 1)[1] if "@" in email else ""
    domain = domain or "tempmailto.com"
    config = get_config()
    resp = _session().post(
        f"{BASE_URL}/change",
        headers=_browser_headers(_ACCEPT_AJAX, ajax=True),
        data={"_token": _csrf(), "name": name, "domain": domain},
        timeout=config.timeout,
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"tempmailto: change http {resp.status_code}: {resp.text.strip()[:200]}"
        )
    data = resp.json()
    if not (data.get("mailbox") or "").strip():
        raise RuntimeError(f"tempmailto: change 响应异常: {resp.text.strip()[:200]}")
    return str(data["mailbox"]).strip()


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """
    读取 tempmailto.com 收件箱

    会话粘性：响应的 mailbox 与请求邮箱不一致时用 /change 拉回目标
    邮箱并重取列表。token 为 Generate 时约定的邮箱（防御性非空）。
    """
    addr = (email or "").strip()
    if not addr:
        raise ValueError("tempmailto: 邮箱为空，请重新 Generate")

    data = _fetch_messages()
    if (data.get("mailbox") or "").strip().lower() != addr.lower():
        changed = _change(addr)
        if changed.lower() != addr.lower():
            raise RuntimeError(
                f"tempmailto: 会话邮箱无法拉回请求邮箱（{changed} != {addr}），请重新 Generate"
            )
        data = _fetch_messages()

    out: List[Email] = []
    for row in data.get("messages") or []:
        ne = _build(row, addr)
        if ne is not None and ne.id:
            out.append(ne)
    return out


def _build(row, email: str) -> Optional[Email]:
    """
    将 messages 列表元素组装为统一 Email

    正文优先取详情页 /view/{id}（同 Cookie 会话），失败回退列表字段；
    from 展示用 from_name，缺失回退 from_email/from。
    """
    if not isinstance(row, dict):
        return None
    mail_id = _str_of(row.get("id"))
    if not mail_id:
        return None
    from_email = _str_of(row.get("from_email")) or _str_of(row.get("from"))
    from_name = _str_of(row.get("from_name"))
    if not from_name:
        from_name = from_email
    date = (
        _str_of(row.get("receivedAt"))
        or _str_of(row.get("received_at"))
        or _str_of(row.get("createdAt"))
    )
    if not date:
        date = datetime.now(timezone.utc).isoformat()

    html_body = _view_detail(mail_id)
    text = html_to_text(html_body) if html_body else ""
    if not text:
        text = (
            _str_of(row.get("body"))
            or _str_of(row.get("text"))
            or _str_of(row.get("snippet"))
            or _str_of(row.get("preview"))
        )
        if not text:
            text = _str_of(row.get("subject"))
    if not html_body:
        html_body = "<html><body><pre>" + html.escape(text) + "</pre></body></html>"

    flat = {
        "id": mail_id,
        "from": from_name,
        "to": email,
        "subject": _str_of(row.get("subject")),
        "date": date,
        "is_seen": _as_bool(row.get("is_seen")),
        "text": text,
        "html": html_body,
    }
    return normalize_email(flat, email)


def _view_detail(mail_id: str) -> str:
    """
    GET /view/{id} 提取邮件正文 HTML（同 Cookie 会话）

    平台视图页以候选 class 依次尝试，全失败回退 <main>/<article>
    区块；仍失败返回空串（列表归一不因此中断）。
    """
    config = get_config()
    resp = _session().get(
        f"{BASE_URL}/view/{mail_id}",
        headers=_browser_headers(_ACCEPT_HTML),
        timeout=config.timeout,
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        return ""
    page = resp.text

    for cls in (
        "mail-body",
        "mail_content",
        "email-body",
        "content-body",
        "message-content",
        "mail-content",
    ):
        m = re.search(
            r'(?is)<[^>]+class="[^"]*\b'
            + re.escape(cls)
            + r'\b[^"]*"[^>]*>([\s\S]*?)</(?:div|section|article)>',
            page,
        )
        if m and m.group(1).strip():
            return m.group(1).strip()
    m = re.search(r"(?is)<(main|article)[^>]*>([\s\S]*?)</\1>", page)
    if m and m.group(2).strip():
        return m.group(2).strip()
    return ""