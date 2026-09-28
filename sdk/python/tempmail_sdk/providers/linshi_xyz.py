"""
LinshiXYZ 渠道实现（linshi.xyz）
无建箱请求：本地随机 6 位 hex 前缀 + @linshi.xyz。
读信 GET /api/mails/{前缀}，响应为邮件对象数组（headers.from/to/subject/date/html）。
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

CHANNEL = "linshi-xyz"
BASE_URL = "https://linshi.xyz"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def _local_name() -> str:
    """生成本地随机 6 位 hex 前缀（与官网 client 相同格式）"""
    return "".join(random.choice("0123456789abcdef") for _ in range(6))


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 linshi.xyz 临时邮箱（无需建箱请求，token 复用完整地址）"""
    address = f"{_local_name()}@linshi.xyz"
    return EmailInfo(channel=channel, email=address, token=address)


def _normalize_mail(raw: dict, address: str) -> Email:
    """headers 对象平铺为顶层字段并注入收件人地址"""
    flat = dict(raw)
    headers = raw.get("headers")
    if isinstance(headers, dict):
        flat.update(headers)
    flat.setdefault("to", address)
    return normalize_email(flat, address)


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """读取 linshi.xyz 收件箱（GET /api/mails/{前缀}）"""
    address = (email or "").strip()
    if not address or "@" not in address:
        raise ValueError(f"linshi-xyz: 邮箱地址无效: {email!r}")
    local = address.split("@", 1)[0]
    resp = tm_http.get(
        f"{BASE_URL}/api/mails/{local}",
        headers=_HEADERS,
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"linshi-xyz: 读取收件箱失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    if not isinstance(data, list):
        raise RuntimeError("linshi-xyz: 收件箱响应非数组骨架")
    return [_normalize_mail(m, address) for m in data]
