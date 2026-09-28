"""
Mtempmail 渠道实现（mtempmail.com，公共 key 认证）
建箱: POST /api/emails/{apiKey}（body {}）→
  {"status":true,"data":{"email":"xxx@domain","domain":"..","ip":"..",
   "fingerprint":"..","expire_at":"..","created_at":"..","id":213000,"email_token":"..."}}
读信: GET /api/messages/{apiKey}/{email} →
  {"status":true,"mailbox":"..","email_token":"..","messages":[]}
  消息列表元素为 map（mailgun 入站 webhook 风格）：
  {"to":[{..}],"body":[{content_type:"text/html",value:".."},{content_type:"text/plain",value:".."}],
   "created_at":"..","id":123,"from":[{"full":"Sender <a@b.com>"}],"subject":"..","flags":[..]}
域名: GET /api/domains/{apiKey}/all → {"status":true,"data":{"domains":[...]}}
邮箱 24 小时有效（建箱响应过期时间为北京时间，标注不一致，以服务端为准）。
"""

import re
from typing import List

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "mtempmail"
BASE_URL = "https://mtempmail.com"

# 公共固定 API key（mtempmail.com 官方提供）
PUBLIC_KEY = "pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}

# 剥离后台拼接的 "• " 前缀（或实收分隔符）
_BULLET_RE = re.compile(r"^[•·]+\s*")


def _clean_subject(subject: str) -> str:
    """清洗主题前导分隔符（后台可能以 "• " 开头拼接微件）"""
    return _BULLET_RE.sub("", (subject or "").strip())


def _body_text(parts: list) -> str:
    """拼接正文纯文本（body[].value 按序）"""
    chunks = [p["value"] for p in parts if isinstance(p, dict) and p.get("value")]
    return "\n".join(chunks)


def _body_html(parts: list) -> str:
    """提取首个 text/html 段"""
    for p in parts:
        if isinstance(p, dict) and p.get("content_type") == "text/html":
            if p.get("value"):
                return p["value"]
    return ""


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 mtempmail 临时邮箱，POST /api/emails/{apiKey}（空 JSON body）"""
    resp = tm_http.post(
        f"{BASE_URL}/api/emails/{PUBLIC_KEY}",
        headers={**_HEADERS, "Content-Type": "application/json"},
        json={},
    )
    resp.raise_for_status()
    data = resp.json()
    payload = data.get("data") if isinstance(data, dict) else None
    if not data.get("status") or not isinstance(payload, dict) or not payload.get("email"):
        raise RuntimeError("mtempmail: 创建邮箱响应缺少邮箱")

    return EmailInfo(
        channel=channel,
        email=payload["email"],
        _token=payload.get("email_token"),
        expires_at=payload.get("expire_at"),
        created_at=payload.get("created_at"),
    )


def get_emails(email: str, token: str = "") -> List[Email]:
    """读取 mtempmail 收件箱，GET /api/messages/{apiKey}/{email}；token 元数据仅为校验，不参与请求"""
    addr = (email or "").strip()
    if not addr:
        raise ValueError("mtempmail: 邮箱为空")
    if not (token or "").strip():
        raise ValueError("mtempmail: token 为空")

    resp = tm_http.get(
        f"{BASE_URL}/api/messages/{PUBLIC_KEY}/{addr}", headers=_HEADERS
    )
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
        raw["to"] = addr
        if isinstance(m.get("subject"), str):
            raw["subject"] = _clean_subject(m["subject"])
        body_parts = m.get("body")
        if isinstance(body_parts, list):
            parts = [p for p in body_parts if isinstance(p, dict)]
            text = _body_text(parts)
            if text:
                raw["text"] = text
            html_part = _body_html(parts)
            if html_part:
                raw["html"] = html_part
        raw["date"] = m.get("created_at", "")
        out.append(normalize_email(raw, addr))
    return out