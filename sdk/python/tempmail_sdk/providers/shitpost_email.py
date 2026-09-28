"""
ShitpostEmail 渠道实现（shitpost.email 公共实例）
无认证 REST：POST /api/create 建箱（username/domain/ttl → email/token/type/expires），
GET /api/inbox?email=&token= 读信（messages[] 含 from/fromName/subject/text/html/date）。
域名池：shitpost.email / letsfuckingpiss.party（克隆自 shamu4life/throwaway-email 公共实例）。
"""

import random
import string
from typing import List
from urllib.parse import urlencode

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "shitpost-email"
BASE_URL = "https://shitpost.email"

_DOMAINS = ["shitpost.email", "letsfuckingpiss.party"]

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def _local() -> str:
    """生成 "sdk"+10 位随机本地名"""
    chars = string.ascii_lowercase + string.digits
    return "sdk" + "".join(random.choice(chars) for _ in range(10))


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建临时邮箱，POST /api/create body {username,domain,ttl}"""
    domain = random.choice(_DOMAINS)
    resp = tm_http.post(
        f"{BASE_URL}/api/create",
        headers={**_HEADERS, "Content-Type": "application/json"},
        json={"username": _local(), "domain": domain, "ttl": 3600},
    )
    resp.raise_for_status()
    data = resp.json()
    if not data.get("email") or not data.get("token"):
        raise RuntimeError("shitpost-email: 创建邮箱响应缺少 email 或 token")

    expires = data.get("expires")
    return EmailInfo(
        channel=channel,
        email=data["email"],
        _token=data["token"],
        expires_at=str(expires) if expires is not None else None,
    )


def get_emails(email: str, token: str = "") -> List[Email]:
    """获取收件箱邮件，GET /api/inbox?email=&token="""
    query = urlencode({"email": email or "", "token": token or ""})
    resp = tm_http.get(f"{BASE_URL}/api/inbox?{query}", headers=_HEADERS)
    resp.raise_for_status()
    data = resp.json()
    messages = data.get("messages") if isinstance(data, dict) else None
    if not isinstance(messages, list):
        return []

    out: List[Email] = []
    for m in messages:
        if not isinstance(m, dict):
            continue
        raw = dict(m)
        raw["from"] = m.get("from", "")
        raw["to"] = email
        raw["text"] = m.get("text", "")
        raw["html"] = m.get("html", "")
        raw["date"] = m.get("date", "")
        out.append(normalize_email(raw, email))
    return out