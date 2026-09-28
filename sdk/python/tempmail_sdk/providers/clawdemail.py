"""
ClawdEmail 渠道实现（clawdemail.com，API 域 api.clawdemail.com）
POST /register 建箱（success/email/token）；
GET /inbox?limit=50 读信列表（Bearer）；GET /email/{id} 取单封详情（Bearer）。
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

CHANNEL = "clawdemail"
BASE_URL = "https://api.clawdemail.com"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def _message_id_of(raw: dict) -> str:
    """提取列表元素的邮件 ID（多候选键）"""
    for key in ("id", "_id", "messageId", "message_id", "slug"):
        value = raw.get(key)
        if value is not None and str(value).strip():
            return str(value).strip()
    return ""


def _auth_headers(token: str) -> dict:
    """设置通用请求头与 Bearer 认证"""
    headers = dict(_HEADERS)
    if token:
        headers["Authorization"] = f"Bearer {token}"
    return headers


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 clawdemail.com 临时邮箱（POST /register，name 空串）"""
    resp = tm_http.post(
        f"{BASE_URL}/register",
        headers={**_HEADERS, "Content-Type": "application/json"},
        json={"name": ""},
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"clawdemail: 创建邮箱失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    address = (data.get("email") or "").strip()
    token = (data.get("token") or "").strip()
    if not address or not token or "@" not in address:
        raise RuntimeError(f"clawdemail: 创建邮箱响应缺少必要字段: {resp.text}")
    return EmailInfo(channel=channel, email=address, token=token)


def _fetch_detail(token: str, mid: str):
    """获取单封邮件详情（Bearer），响应含 email 嵌套对象时提升之，失败返回 None"""
    try:
        resp = tm_http.get(
            f"{BASE_URL}/email/{mid.rsplit('/', 1)[-1]}",
            headers=_auth_headers(token),
        )
        if resp.status_code < 200 or resp.status_code >= 300:
            return None
        detail = resp.json()
        nested = detail.get("email")
        if isinstance(nested, dict):
            return nested
        return detail
    except Exception:
        return None


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """获取 clawdemail.com 邮件列表（列表 + 详情合并，详情失败回退列表摘要）"""
    address = (email or "").strip()
    resp = tm_http.get(f"{BASE_URL}/inbox?limit=50", headers=_auth_headers(token or ""))
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"clawdemail: 获取邮件列表失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    if not data.get("success"):
        raise RuntimeError(f"clawdemail: 读取收件箱失败: {data.get('error')}")
    out = []
    for raw in data.get("emails") or []:
        mid = _message_id_of(raw)
        if not mid:
            out.append(normalize_email(raw, address))
            continue
        detail = _fetch_detail(token or "", mid)
        if detail is None:
            out.append(normalize_email(raw, address))
            continue
        merged = dict(raw)
        for key, value in (detail or {}).items():
            merged.setdefault(key, value)
        out.append(normalize_email(merged, address))
    return out
