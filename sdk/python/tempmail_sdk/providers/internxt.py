"""
Internxt 渠道实现（internxt.com/temporary-email，Next.js + OpenNext）

调研实证结论（2026-09-28，与 Go 端 internxt.go 一致）：
  - 建箱 GET /api/temp-mail/create-email，读信 GET
    /api/temp-mail/get-inbox?email=<e>&token=<t>，详情 GET
    /api/temp-mail/get-message?email=<e>&token=<t>&messageId=<id>。
    create-email 仅接受 GET（POST 返回 405 Method not allowed）。
  - CSRF：首次 GET /temporary-email 响应 Set-Cookie csrfSecret=...
    与 XSRF-TOKEN=...（均 HttpOnly）。数据接口校验请求头
    csrf-token，其值必须与 Cookie jar 中 XSRF-TOKEN 一致：
    头=XSRF-TOKEN 值时 200；头=csrfSecret 值时恒定 500。
  - 建箱响应：{"address":"<前缀>@uberip.com","token":"<十六进制>"}。
    健壮性提醒：每个 API 响应均会刷新 XSRF-TOKEN 的 Set-Cookie，
    故每次读信前都应从 Cookie 罐重取最新值作为 csrf-token 头
    （实测 csrfSecret 恒定，XSRF-TOKEN 每次刷新）。
  - get-inbox 正常返回顶层数组（空箱 []）；错误 token 返回 401
    {"message":"Email has expired"}。get-message 不存在的 messageId
    返回 404 {"message":"Message not found"}。

会话隔离：本渠道独立维护专属 requests.Session（Cookie 罐），
  csrf-token 头每次请求前从罐中重取最新 XSRF-TOKEN 值。
"""

import json
from typing import List, Optional

import requests

from ..config import get_config
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "internxt"
SITE = "https://internxt.com"
REF = SITE + "/temporary-email"
API_BASE = SITE + "/api/temp-mail"

# 固定浏览器 UA（与 Go 端 tls-client 同款形态）
_USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"
)

# 模块级专属会话（Cookie 由本渠道独占，防 XSRF-TOKEN 轮换污染其它渠道）
_sess: Optional[requests.Session] = None


def _session() -> requests.Session:
    """
    模块级专属 requests.Session（同 ..config 配置源）

    不复用全局 Session 是因为本渠道依赖 Cookie 罐内 XSRF-TOKEN 的最新值，
    独占罐可避免与其它渠道 Cookie 串池。
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


def _xsrf_from_jar() -> str:
    """从专属会话 Cookie 罐取当前 XSRF-TOKEN 值（无则空串）"""
    for cookie in _session().cookies:
        if cookie.name == "XSRF-TOKEN" and cookie.value:
            return cookie.value
    return ""


def _prepare_xsrf() -> str:
    """
    确保 Cookie 罐持有本域 csrfSecret 与 XSRF-TOKEN

    无则 GET /temporary-email 夺取；返回罐中最新 XSRF-TOKEN 值。
    """
    xsrf = _xsrf_from_jar()
    if xsrf:
        return xsrf
    config = get_config()
    try:
        _session().get(
            REF,
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
    except requests.RequestException:
        pass
    xsrf = _xsrf_from_jar()
    if not xsrf:
        raise RuntimeError("internxt: 未取得 XSRF-TOKEN Cookie")
    return xsrf


def _get(path: str, params: Optional[dict] = None) -> requests.Response:
    """
    带 CSRF 头请求 internxt 数据接口（GET）

    csrf-token 头取罐中 XSRF-TOKEN 最新值（每次请求前重取，因为
    每个 API 响应都会刷新该 Cookie）；响应沿用专属会话（Set-Cookie
    自动落罐）。
    """
    csrf_token = _prepare_xsrf()
    config = get_config()
    resp = _session().get(
        API_BASE + path,
        params=params or {},
        headers={
            "User-Agent": _USER_AGENT,
            "Accept": "application/json, text/plain, */*",
            "Origin": SITE,
            "Referer": REF,
            "csrf-token": csrf_token,
        },
        timeout=config.timeout,
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"internxt: {path} 失败 http {resp.status_code}: {resp.text[:200]}"
        )
    return resp


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """
    创建 internxt.com 临时邮箱

    _prepare_xsrf 确保罐中 XSRF-TOKEN，再 GET /api/temp-mail/create-email
    （带 csrf-token 头），响应 {"address","token"}（token 为收信令牌）。
    """
    try:
        _prepare_xsrf()
    except RuntimeError:
        raise
    resp = _get("/create-email")
    data = resp.json()
    address = (data.get("address") or "").strip()
    api_token = (data.get("token") or "").strip()
    if not address or not api_token or "@" not in address:
        raise RuntimeError("internxt: 创建邮箱响应缺少必要字段（address/token）")
    return EmailInfo(
        channel=channel,
        email=address,
        _token=json.dumps({"address": address, "token": api_token}),
    )


def _message_id_of(m: dict) -> str:
    """从列表元素中提取邮件 ID（字符串形态，与 Go 端一致）"""
    for key in ("id", "messageId", "message_id"):
        v = m.get(key)
        if v is not None:
            s = str(v).strip()
            if s:
                return s
    return ""


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """
    获取 internxt.com 收件箱

    GET /api/temp-mail/get-inbox?email=&token= 返回顶层数组（列表元素含
    id/from/subject/date/seen 等）；逐条 GET /api/temp-mail/get-message
    拉单封全文（响应为单封对象，含 html 渲染全文），详情失败回退列表摘要。

    @param email 邮箱地址
    @param token 会话凭据 JSON（address/token）
    """
    try:
        sess = json.loads(token or "")
    except (ValueError, TypeError) as err:
        raise ValueError(f"internxt: 会话凭据解析失败: {err}") from err
    if not isinstance(sess, dict):
        raise ValueError("internxt: 会话凭据解析失败（非对象）")
    address = (sess.get("address") or "").strip()
    api_token = (sess.get("token") or "").strip()
    if not address or not api_token:
        raise ValueError("internxt: 会话凭据缺少必要字段")
    if address != email:
        raise ValueError("internxt: 会话邮箱与查询邮箱不匹配")

    resp = _get(
        "/get-inbox", params={"email": address, "token": api_token}
    )
    body = resp.text
    if not body.strip():
        rows = []
    else:
        try:
            rows = resp.json()
        except ValueError as err:
            raise RuntimeError(f"internxt: 解析收件箱响应失败: {err}") from err
        if not isinstance(rows, list):
            raise RuntimeError("internxt: 收件箱响应不是顶层数组")

    out: List[Email] = []
    for m in rows:
        if not isinstance(m, dict):
            continue
        mid = _message_id_of(m)
        if not mid:
            out.append(normalize_email(m, email))
            continue
        try:
            dresp = _get(
                "/get-message",
                params={
                    "email": address,
                    "token": api_token,
                    "messageId": mid,
                },
            )
            detail = dresp.json()
            if isinstance(detail, dict):
                for k, v in detail.items():
                    if k not in m:
                        m[k] = v
        except Exception:
            # 详情失败回退列表摘要
            pass
        out.append(normalize_email(m, email))
    return out