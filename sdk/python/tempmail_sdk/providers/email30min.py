"""
30minemail 渠道实现（30minemail.com）
建箱 GET /?generate 返回 HTML 页面，从其中解析 16 位 hex 本地名地址；
读信 GET /messages.php?email=<完整地址>&_=<unix毫秒>。无认证、无 Cookie、无 CSRF。
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

CHANNEL = "30minemail"
BASE_URL = "https://30minemail.com"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


import time

DOMAIN = "30minemail.com"


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 30minemail.com 临时邮箱（GET /?generate，解析页面中的邮箱地址）"""
    resp = tm_http.get(
        f"{BASE_URL}/?generate",
        headers={**_HEADERS,
                 "Accept": "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8"},
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"30minemail: 创建邮箱失败 http {resp.status_code}")
    page = resp.text
    idx = page.find(f"@{DOMAIN}")
    if idx < 0:
        raise RuntimeError("30minemail: 创建页面未找到邮箱地址")
    # 向前查找本地名起点：空白或 > 之后
    start = idx
    while start > 0 and page[start - 1] not in (" ", "\n", "\t", ">", '"'):
        start -= 1
    local = page[start:idx].strip()
    if len(local) < 8:
        raise RuntimeError(f"30minemail: 创建页面解析地址异常: {page[start:idx + len(DOMAIN) + 1]}")
    address = f"{local}@{DOMAIN}"
    return EmailInfo(channel=channel, email=address, token=address)


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """读取 30minemail.com 收件箱（GET /messages.php?email=&_=<unix毫秒>）"""
    address = (email or "").strip()
    if not address:
        raise ValueError("30minemail: 邮箱地址为空")
    url = f"{BASE_URL}/messages.php?email={address}&_={int(time.time() * 1000)}"
    resp = tm_http.get(url, headers=_HEADERS)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"30minemail: 读取收件箱失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    if not data.get("ok") or data.get("expired"):
        raise RuntimeError(f"30minemail: 收件箱不可用或已过期: {resp.text}")
    out = []
    for raw in data.get("emails") or []:
        flat = dict(raw)
        # 列表元素无 to 字段时注入收件人地址
        flat.setdefault("to", address)
        out.append(normalize_email(flat, address))
    return out
