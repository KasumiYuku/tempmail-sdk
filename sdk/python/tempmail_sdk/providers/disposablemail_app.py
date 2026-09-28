"""
DisposableMail.app 渠道 — https://disposablemail.app
纯 REST JSON API，无需认证。
创建收件箱: POST /api/inbox，body 强制指定 {"domain": "disposablemail.dev"}。
获取邮件: GET /api/inbox/emails?token={token}&cachebust=<纳秒时间戳>。
域名: @disposablemail.dev, @mailmehere.cc

归因注记（2026-09-28 平台级归因，同步自 Go 端）：
mailmehere.cc 域存在收信丢失，SDK 建箱强制 disposablemail.dev 域；
平台对同一 URL 偶发返回陈旧空列表，列表请求附加 cachebust 参数并显式禁缓存。
邮件字段为平台真实响应结构（fromAddress/bodyText 等），按真实字段显式映射，
不依赖字段候选策略避免错配。
"""

import time
from typing import List

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "disposablemail-app"
BASE = "https://disposablemail.app"
DEFAULT_DOMAIN = "disposablemail.dev"

# 公共请求头
_HEADERS = {
    "Content-Type": "application/json",
    "Accept": "application/json, text/plain, */*",
    "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
    "Referer": f"{BASE}/",
    "Origin": BASE,
    "User-Agent": (
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
    ),
}

_GET_HEADERS = {
    key: value for key, value in _HEADERS.items() if key != "Content-Type"
}
_GET_HEADERS["Cache-Control"] = "no-cache"


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """
    创建 disposablemail.app 临时邮箱

    流程:
    1. POST /api/inbox 创建收件箱，body 强制指定 disposablemail.dev 域
       （mailmehere.cc 域存在收信丢失，不随服务器随机分配）
    2. 从响应中提取 address 和 token
    3. token 直接存储 API 返回的字符串
    """
    resp = tm_http.post(
        f"{BASE}/api/inbox",
        headers=_HEADERS,
        json={"domain": DEFAULT_DOMAIN},
    )
    resp.raise_for_status()

    body = resp.json()
    address = body.get("address", "")
    token = body.get("token", "")

    if not address or "@" not in address:
        raise RuntimeError(f"disposablemail-app: 返回的邮箱地址无效: {address!r}")

    if not token:
        raise RuntimeError("disposablemail-app: 返回的 token 为空")

    return EmailInfo(channel=channel, email=address, _token=token)


def get_emails(email: str, token: str) -> List[Email]:
    """
    获取 disposablemail.app 邮件列表

    流程:
    1. GET /api/inbox/emails?token={token}&cachebust={纳秒时间戳} 获取邮件，
       cachebust 强制回源规避平台陈旧空列表，并携带 Cache-Control: no-cache
    2. 按平台真实字段结构（fromAddress/bodyText 等）显式映射后 normalize_email
    """
    if not token:
        raise ValueError("disposablemail-app: token 不能为空")

    addr = (email or "").strip()
    if not addr:
        raise ValueError("disposablemail-app: 邮箱地址不能为空")

    cachebust = time.time_ns()
    resp = tm_http.get(
        f"{BASE}/api/inbox/emails",
        headers=_GET_HEADERS,
        params={"token": token, "cachebust": cachebust},
    )
    resp.raise_for_status()

    body = resp.json()
    raw_emails = body.get("emails", [])
    if not isinstance(raw_emails, list):
        return []

    out: List[Email] = []
    for item in raw_emails:
        if not isinstance(item, dict):
            continue
        # 平台下发的字段与旧文档映射表不同（如 from_address 实为 fromAddress），
        # 按真实响应结构显式映射后归一化
        flat = {
            "id": item.get("id", ""),
            "from": item.get("fromAddress", ""),
            "name": item.get("fromName", ""),
            "to": addr,
            "subject": item.get("subject", ""),
            "text": item.get("bodyText", ""),
            "html": item.get("bodyHtml", ""),
            "date": item.get("receivedAt", ""),
            "isRead": item.get("isRead", False),
            "attachments": item.get("attachments"),
        }
        out.append(normalize_email(flat, addr))

    return out