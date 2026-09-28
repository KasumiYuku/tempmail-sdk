"""
Temporarymail 渠道实现（temporarymail.com）
无认证 REST（key 为空即随机建箱）：建箱 GET /api/?action=requestEmailAccess；
读信 GET /api/?action=checkInbox（空箱 []、有信 map[id]→实体 双形态）；
详情 POST /api/?action=getEmail 覆盖真实主题；全文 GET /view/?i=<id> 剥标签还原纯文本。

风控策略（与 Go 端对齐，2026-09-28 实测）：/api/ 三端点均铺浏览器形态头
（User-Agent + Referer + Origin + Sec-Fetch 系列），403 时换备用 UA 重试一次。
"""

import html
import json
import re
from typing import List, Optional
from urllib.parse import quote_plus

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "temporarymail-com"
BASE_URL = "https://temporarymail.com"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


ALT_USER_AGENT = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                  "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36")

_TAG_RE = re.compile(r"<script[\s\S]*?</script>|<style[\s\S]*?</style>|<[^>]+>")


def _api_headers(user_agent: str) -> dict:
    """构造 /api/ 请求头（浏览器形态头部集，规避脚本判别收紧）"""
    return {
        "Accept": "application/json, text/plain, */*",
        "Accept-Language": "en-US,en;q=0.9",
        "Sec-Fetch-Site": "same-origin",
        "Sec-Fetch-Mode": "cors",
        "Sec-Fetch-Dest": "empty",
        "Referer": BASE_URL + "/",
        "Origin": BASE_URL,
        "User-Agent": user_agent,
    }


def _full_api_headers(alt: bool = False) -> dict:
    """铺浏览器形态头部集；alt=True 时换备用 UA（403 重试用）"""
    return _api_headers(ALT_USER_AGENT if alt else _HEADERS["User-Agent"])


def _str_of(value) -> str:
    """任意 JSON 值规整为字符串（nil→空串）"""
    if value is None:
        return ""
    return str(value)


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 temporarymail.com 临时邮箱（GET requestEmailAccess，token 复用 secretKey）"""
    resp = tm_http.get(
        f"{BASE_URL}/api/?action=requestEmailAccess&key=&value=random",
        headers=_full_api_headers(),
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        if resp.status_code == 429:
            retry_after = resp.headers.get("Retry-After", "")
            raise RuntimeError(
                f"temporarymail: 创建邮箱平台限流(429 Retry-After={retry_after})，请稍后重试: {resp.text}")
        if resp.status_code == 403:
            resp = tm_http.get(
                f"{BASE_URL}/api/?action=requestEmailAccess&key=&value=random",
                headers=_full_api_headers(alt=True),
            )
        if resp.status_code < 200 or resp.status_code >= 300:
            raise RuntimeError(f"temporarymail: 创建邮箱失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    address = (data.get("address") or "").strip()
    secret_key = data.get("secretKey") or ""
    if not address or not secret_key:
        raise RuntimeError(f"temporarymail: 创建响应缺少 address 或 secretKey: {resp.text}")
    return EmailInfo(channel=channel, email=address, _token=secret_key)


def _check_inbox(token: str) -> str:
    """拉取 checkInbox 响应体（403 换备用 UA 重试一次，429 报平台限流）"""
    for alt in (False, True):
        resp = tm_http.get(
            f"{BASE_URL}/api/?action=checkInbox&value={quote_plus(token)}",
            headers=_full_api_headers(alt=alt),
        )
        if resp.status_code == 429:
            retry_after = resp.headers.get("Retry-After", "")
            raise RuntimeError(
                f"temporarymail: 读取收件箱平台限流(429 Retry-After={retry_after})，请拉大轮询间隔")
        if 200 <= resp.status_code < 300:
            return resp.text
        # 403/404 疑似 UA 键控风控，换备用 UA 重试一次
        if resp.status_code not in (403, 404):
            raise RuntimeError(f"temporarymail: 读取收件箱失败 http {resp.status_code}: {resp.text}")
    raise RuntimeError("temporarymail: 读取收件箱失败 http 403（两次尝试均被拒）")


def _fetch_detail(mid: str):
    """拉取单封邮件详情（POST getEmail），失败返回 None（列表元数据兜底）"""
    try:
        resp = tm_http.post(
            f"{BASE_URL}/api/?action=getEmail&value={quote_plus(mid)}",
            headers=_full_api_headers(),
        )
        if resp.status_code == 429 or resp.status_code < 200 or resp.status_code >= 300:
            return None
        data = resp.json()
        if isinstance(data, dict):
            for value in data.values():
                if isinstance(value, dict):
                    return (value.get("subject") or "", value.get("from") or "")
        return None
    except Exception:
        # 与 Go 端对齐：详情任何失败（含网络异常/429）一律降级为列表元数据兜底
        return None


def _view_to_text(source: str) -> str:
    """将 /view/ 响应剥标签还原为纯文本（<br>/<p> 换行保留）"""
    text = source
    for br in ("<br />", "<br/>", "<br>"):
        text = text.replace(br, "\n")
    for tag in ("<p>", "</p>"):
        text = text.replace(tag, "\n")
    text = text.replace("&nbsp;", " ")
    text = _TAG_RE.sub(" ", text)
    text = html.unescape(text)
    text = text.replace("&quot;", '"').replace("&apos;", "'")
    text = html.unescape(text)
    return "\n".join(line.strip() for line in text.split("\n")).strip()


def _fetch_view_text(mid: str) -> str:
    """抓取 /view/ 渲染端点全文并剥标签还原纯文本，失败返回空串"""
    try:
        resp = tm_http.get(
            f"{BASE_URL}/view/?i={quote_plus(mid)}&width=800",
            headers={**_HEADERS, "Accept": "text/html, */*", "Referer": BASE_URL + "/"},
        )
        if resp.status_code < 200 or resp.status_code >= 300:
            return ""
        return _view_to_text(resp.text)
    except Exception:
        return ""


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """读取 temporarymail 收件箱（双形态解析 + 详情主题覆盖 + /view/ 全文）"""
    address = (email or "").strip()
    raw_body = _check_inbox(token or "")
    data = json.loads(raw_body)
    # 平台响应两种合法形态：空箱 []，有信为 map[id]→对象
    if isinstance(data, list):
        rows = data
    elif isinstance(data, dict):
        rows = list(data.values())
    else:
        rows = []
    out = []
    for raw in rows:
        flat = dict(raw)
        # 列表元素无 to 字段，注入收件人地址以归一化
        flat.setdefault("to", address)
        mid = _str_of(raw.get("id"))
        if mid:
            detail = _fetch_detail(mid)
            if detail:
                subject, from_addr = detail
                if subject:
                    flat["subject"] = subject
                if from_addr:
                    flat["from"] = from_addr
            text = _fetch_view_text(mid)
            if text:
                flat["text"] = text
        out.append(normalize_email(flat, address))
    return out
