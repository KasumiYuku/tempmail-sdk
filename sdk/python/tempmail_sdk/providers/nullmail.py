"""
Nullmail 渠道实现（nullmail.cc / maildock.store）
无认证 REST：POST /api/emails（空 JSON body）建箱，响应
{"address":"...@maildock.store","expiry":"2026-09-27T02:25:28.778Z"}；
读信 GET /api/emails/{address}（URL 编码），响应 {"expiry":"...","emails":[...]}，
列表项只有 id/sender/subject/delivered，正文须逐封二拉 GET /api/emails/{addr}/body/{id}；
续期 PUT /api/emails/{addr}/extend/1h 暂不接入。
"""

from typing import List
from urllib.parse import quote

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "nullmail"
BASE_URL = "https://www.nullmail.cc"

_HEADERS = {
    "Accept": "application/json",
    "Origin": BASE_URL,
    "Referer": f"{BASE_URL}/",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建临时邮箱，POST /api/emails（空 JSON body），token 复用完整地址以便收件箱回查"""
    resp = tm_http.post(
        f"{BASE_URL}/api/emails",
        headers={**_HEADERS, "Content-Type": "application/json"},
        json={},
    )
    resp.raise_for_status()
    data = resp.json()
    addr = (data.get("address") or "").strip()
    if not addr:
        raise RuntimeError("nullmail: 建箱响应缺少 address 字段")

    return EmailInfo(
        channel=channel, email=addr, _token=addr, expires_at=data.get("expiry")
    )


def get_emails(email: str, token: str = "") -> List[Email]:
    """
    读取收件箱，GET /api/emails/{address}（URL 编码）。
    列表项无正文，逐封二拉 body 端点取纯文本正文；失败降级留空不阻断列表。
    """
    addr = (email or "").strip()
    if not addr:
        raise ValueError("nullmail: 邮箱地址为空")

    resp = tm_http.get(
        f"{BASE_URL}/api/emails/{quote(addr)}", headers=_HEADERS
    )
    resp.raise_for_status()
    data = resp.json()
    emails_list = data.get("emails") if isinstance(data, dict) else None
    if not isinstance(emails_list, list):
        return []

    out: List[Email] = []
    for m in emails_list:
        if not isinstance(m, dict):
            continue
        raw = dict(m)
        raw["to"] = addr
        # normalize_email 日期候选键不含 delivered，显式映射为 date 后归一化
        if m.get("delivered"):
            raw["date"] = m["delivered"]
        # 列表只有 id/sender/subject/delivered，正文逐封二拉 body 端点
        mid = m.get("id")
        if mid is not None:
            body = _fetch_body(addr, mid)
            if body:
                raw["text"] = body
        out.append(normalize_email(raw, addr))
    return out


def _fetch_body(addr: str, mail_id) -> str:
    """单封正文二拉，GET /api/emails/{addr}/body/{id}，响应 {"body":"<完整纯文本正文>"}"""
    try:
        resp = tm_http.get(
            f"{BASE_URL}/api/emails/{quote(addr)}/body/{quote(str(mail_id))}",
            headers=_HEADERS,
        )
        resp.raise_for_status()
        data = resp.json()
        return data.get("body") if isinstance(data, dict) else ""
    except Exception:
        return ""