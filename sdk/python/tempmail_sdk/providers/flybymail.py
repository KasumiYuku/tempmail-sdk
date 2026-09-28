"""
Flybymail 渠道实现（flybymail.com）
POST /api/recipients 建箱（空 JSON body）；
GET /api/recipients/{email}/emails 读信（响应 {"emails":[...]}）。
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

CHANNEL = "flybymail"
BASE_URL = "https://flybymail.com"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 flybymail.com 临时邮箱（POST /api/recipients，expiresAt 为毫秒时间戳）"""
    resp = tm_http.post(
        f"{BASE_URL}/api/recipients",
        headers={**_HEADERS, "Content-Type": "application/json"},
        data="{}",
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"flybymail: 创建邮箱失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    if not data.get("id") or not data.get("email") or "@" not in (data.get("email") or ""):
        raise RuntimeError(f"flybymail: 创建邮箱响应缺少必要字段: {resp.text}")
    # expiresAt 为毫秒时间戳，转换为秒供 EmailInfo 统一展示
    expires_at = data.get("expiresAt")
    if expires_at and expires_at > 0:
        expires_at = str(expires_at // 1000)
    else:
        expires_at = None
    return EmailInfo(
        channel=channel,
        email=data["email"],
        token=data["id"],
        expires_at=expires_at,
    )


def _any_string(raw: dict, *keys: str) -> str:
    """从邮件元素提取候选键的首个非空字符串（数字转十进制字符串）"""
    for key in keys:
        value = raw.get(key)
        if isinstance(value, str) and value.strip():
            return value.strip()
        if isinstance(value, (int, float)):
            return str(int(value))
    return ""


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """获取 flybymail.com 邮件列表（GET /api/recipients/{email}/emails）"""
    address = (email or "").strip()
    if not address or "@" not in address:
        raise ValueError("flybymail: 邮箱地址为空或格式错误")
    resp = tm_http.get(f"{BASE_URL}/api/recipients/{address}/emails", headers=_HEADERS)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"flybymail: 获取邮件列表失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    out = []
    for raw in data.get("emails") or []:
        entry = {
            "from": raw.get("from"),
            "to": raw.get("to"),
            "subject": raw.get("subject"),
            "text": raw.get("body"),
            "html": raw.get("htmlBody"),
            "time": raw.get("time"),
            "read": raw.get("read"),
            "attachments": raw.get("attachments"),
        }
        # id 为数字时转字符串保持一致，time 为毫秒时间戳时按 timestamp 归一
        entry["id"] = _any_string(raw, "id")
        timestamp = raw.get("time", raw.get("date"))
        if timestamp is not None:
            entry["timestamp"] = timestamp
        out.append(normalize_email(entry, address))
    return out
