"""
Crazymailing 渠道实现（crazymailing.com）
建箱 POST /api/mailbox（空 JSON body）；
读信 GET /api/messages?mailbox=<完整地址>，逐封 GET /api/message/{id}/body 拉取正文。
"""

import base64
import html
import json
import random
import re
from typing import List, Optional

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "crazymailing"
BASE_URL = "https://crazymailing.com"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


_ORIGIN_HEADERS = {
    **_HEADERS,
    "Origin": BASE_URL,
    "Referer": BASE_URL + "/",
}


def _message_id_of(raw: dict) -> str:
    """提取列表元素的邮件 ID（多候选键）"""
    for key in ("id", "_id", "messageId", "message_id", "slug"):
        value = raw.get(key)
        if value is not None and str(value).strip():
            return str(value).strip()
    return ""


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 crazymailing 临时邮箱（POST /api/mailbox，空 JSON body）"""
    resp = tm_http.post(
        f"{BASE_URL}/api/mailbox",
        headers={**_ORIGIN_HEADERS, "Content-Type": "application/json"},
        data="{}",
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"crazymailing: 创建邮箱失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    mailbox = data.get("mailbox") or {}
    address = (mailbox.get("address") or "").strip()
    if not address:
        raise RuntimeError("crazymailing: 创建响应缺少 mailbox.address")
    return EmailInfo(
        channel=channel,
        email=address,
        token=(mailbox.get("id") or "").strip(),
        expires_at=mailbox.get("expiresAt"),
    )


def _fetch_body(mid: str) -> str:
    """拉取单封正文（响应为完整 HTML 页面），失败返回空串"""
    try:
        resp = tm_http.get(
            f"{BASE_URL}/api/message/{mid}/body",
            headers={
                **_ORIGIN_HEADERS,
                "Accept": "text/html,application/xhtml+xml,*/*;q=0.8",
            },
        )
        if resp.status_code < 200 or resp.status_code >= 300:
            return ""
        return resp.text.strip()
    except Exception:
        return ""


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """读取 crazymailing 收件箱（逐封二拉正文，详情失败以列表摘要归一）"""
    address = (email or "").strip()
    if not address:
        raise ValueError("crazymailing: 邮箱地址为空")
    resp = tm_http.get(f"{BASE_URL}/api/messages?mailbox={address}", headers=_ORIGIN_HEADERS)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"crazymailing: 读取收件箱失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    messages = data.get("messages") or []
    out = []
    for raw in messages:
        flat = dict(raw)
        flat.setdefault("to", address)
        mid = _message_id_of(raw)
        if mid:
            body = _fetch_body(mid)
            if body:
                flat["html"] = body
        out.append(normalize_email(flat, address))
    return out
