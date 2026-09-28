"""
Firetempmail 渠道实现（firetempmail.com）
无认证 REST：建箱无需请求，本地生成 随机词+0-999@<域>（域池与官网一致：
offrework.click / service-today.click / jobsdeforyou.sa.com）；
读信 GET https://mail.firetempmail.com/mail/get?address=<邮箱 URL 编码>，
必须携带 Header Origin: https://firetempmail.com（否则 403 'Origin not allowed'）。
响应形如 {"status":"ok","code":200,"msg":"...","stats":{},"mails":[...]}，
邮件字段以 sender/subject/date/recipient/suffix + content-html/content-text/content-plain
多候选归一化（为空则体现为 "No available emails"）。
"""

import random
import string
from typing import List
from urllib.parse import quote

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "firetempmail"
API_BASE = "https://mail.firetempmail.com"
ORIGIN = "https://firetempmail.com"

# 官网 JS chunk 中的完整平台域池，顺序与官网一致
DOMAINS = ["offrework.click", "service-today.click", "jobsdeforyou.sa.com"]

_HEADERS = {
    "Accept": "application/json",
    "Origin": ORIGIN,
    "Referer": f"{ORIGIN}/",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def _local() -> str:
    """随机小写单词 + 0-999（与官网 faker unique 词 + 1e3 取整一致）"""
    chars = string.ascii_lowercase
    word = "".join(random.choice(chars) for _ in range(random.randint(3, 6)))
    return f"{word}{random.randint(0, 999)}"


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建临时邮箱：建箱无需请求，本地生成随机词+数字@域名，token 复用完整地址"""
    domain = random.choice(DOMAINS)
    address = f"{_local()}@{domain}"
    return EmailInfo(channel=channel, email=address, _token=address)


def get_emails(email: str, token: str = "") -> List[Email]:
    """读取收件箱，GET /mail/get?address=<URL 编码完整邮箱>，必带 Origin 头"""
    addr = (email or "").strip()
    if not addr:
        raise ValueError("firetempmail: 邮箱地址为空")

    resp = tm_http.get(
        f"{API_BASE}/mail/get?address={quote(addr)}", headers=_HEADERS
    )
    resp.raise_for_status()
    data = resp.json()
    if data.get("status") not in ("", "ok"):
        raise RuntimeError(f"firetempmail: 读信失败 {data.get('msg', '')}")

    mails = data.get("mails")
    if not isinstance(mails, list):
        return []

    out: List[Email] = []
    for m in mails:
        if not isinstance(m, dict):
            continue
        raw = dict(m)
        # 官网 JSON 无统一 to 字段，收件人固定为当前邮箱
        raw["to"] = addr
        # 正文多候选：content-html 优先，其次 content-text / content-plain / text
        raw["html"] = m.get("content-html", m.get("html", ""))
        raw["text"] = m.get("content-text", m.get("content-plain", m.get("text", "")))
        raw["from"] = m.get("sender", m.get("from", m.get("from_address", "")))
        raw["subject"] = m.get("subject", m.get("title", ""))
        raw["date"] = m.get("date", m.get("received_at", m.get("created_at", "")))
        out.append(normalize_email(raw, addr))
    return out