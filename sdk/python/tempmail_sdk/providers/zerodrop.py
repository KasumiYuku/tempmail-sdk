"""
Zerodrop 渠道实现（zerodrop.dev）
无认证 REST：建箱无需请求，本地生成 "sdk"+8 位随机名，地址为 <名>@zerodrop-sandbox.online；
读信 GET /api/inbox/{name}?source=sdk，响应形如 {"emails":[...],"count":N}。
平台邮件对象只有 id/from/to/subject/receivedAt/raw/otp/magicLink：正文仅存在于
raw（完整 MIME 原文，头部与 body 以空行分隔），无 text/html 字段，
故须从 raw 中剥离头部提取纯文本 body 填入 text，html 留空由 normalize_email 互转。
"""

import random
import string
from typing import List

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "zerodrop"
BASE_URL = "https://zerodrop.dev"
DOMAIN = "zerodrop-sandbox.online"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def _local_name() -> str:
    """生成 "sdk"+8 位随机本地名"""
    chars = string.ascii_lowercase + string.digits
    return "sdk" + "".join(random.choice(chars) for _ in range(8))


def _raw_body(raw: str) -> str:
    """
    从 raw（完整 MIME 原文）提取纯文本正文：
    定位首个空行（头部/正文分隔），其后部分即 body。
    """
    if "\r\n\r\n" in raw:
        return raw.split("\r\n\r\n", 1)[1]
    if "\n\n" in raw:
        return raw.split("\n\n", 1)[1]
    return ""


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建临时邮箱：建箱无需请求，本地生成随机名，token 复用完整地址以便收件箱回查"""
    address = f"{_local_name()}@{DOMAIN}"
    return EmailInfo(channel=channel, email=address, _token=address)


def get_emails(email: str, token: str = "") -> List[Email]:
    """读取收件箱，GET /api/inbox/{name}?source=sdk，多候选字段交由 normalize_email 归一化"""
    addr = (email or "").strip()
    parts = addr.split("@", 1)
    if len(parts) != 2 or parts[1] != DOMAIN:
        raise ValueError(f"zerodrop: 非 {DOMAIN} 域邮箱地址")

    resp = tm_http.get(
        f"{BASE_URL}/api/inbox/{parts[0]}?source=sdk", headers=_HEADERS
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
        # 平台响应无 to 字段，收件人固定为当前邮箱
        raw["to"] = addr
        # 正文仅存在于 raw（完整 MIME 原文），无 text/html 字段：提取纯文本 body 作 text
        raw_value = m.get("raw")
        if isinstance(raw_value, str) and raw_value:
            body = _raw_body(raw_value)
            if body:
                raw["text"] = body
        out.append(normalize_email(raw, addr))
    return out