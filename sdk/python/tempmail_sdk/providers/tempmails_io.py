"""
TempmailsIo 渠道实现（tempmails.io）
无认证 REST：POST /api/temp-mail/generate 建箱（响应 email/token/expires_at），
GET /api/temp-mail/inbox/{token} 读信（messages[] 含 from_email/text_body/html_body/attachments）。
邮箱借用 uberip.com 等公共域（10 分钟自动过期）。
"""

from typing import List

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "tempmails-io"
BASE_URL = "https://tempmails.io"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 10 分钟临时邮箱，POST /api/temp-mail/generate（空 body）"""
    resp = tm_http.post(
        f"{BASE_URL}/api/temp-mail/generate",
        headers={**_HEADERS, "Content-Type": "application/json"},
        json={},
    )
    resp.raise_for_status()
    data = resp.json()
    payload = data.get("data") if isinstance(data, dict) else None
    if (
        not isinstance(payload, dict)
        or not data.get("success")
        or not payload.get("email")
        or not payload.get("token")
    ):
        raise RuntimeError("tempmails-io: 创建邮箱响应缺少 email 或 token")

    return EmailInfo(
        channel=channel,
        email=payload["email"],
        _token=payload["token"],
        expires_at=payload.get("expires_at"),
    )


def get_emails(email: str, token: str = "") -> List[Email]:
    """
    获取收件箱邮件

    注意：必须先 POST /api/temp-mail/fetch-emails/{token} 触发平台对上游
    信箱（uberip.com 域，mail.tm 别名）的主动同步，其后 GET /api/temp-mail/inbox/{token}
    才能读到新邮件。只轮询 inbox 会永远为空（实测 poll10 全 0，fetch 后 1 次即到）。
    """
    if not token:
        raise ValueError("tempmails-io: token 不能为空")

    # 1) 触发同步（失败不致命，仍尝试静态读）
    try:
        tm_http.post(
            f"{BASE_URL}/api/temp-mail/fetch-emails/{token}",
            headers=_HEADERS,
            timeout=15,
        )
    except Exception:
        pass

    # 2) 读静态收件箱
    resp = tm_http.get(f"{BASE_URL}/api/temp-mail/inbox/{token}", headers=_HEADERS)
    resp.raise_for_status()
    data = resp.json()
    payload = data.get("data") if isinstance(data, dict) else None
    messages = payload.get("messages") if isinstance(payload, dict) else None
    if not isinstance(messages, list):
        return []

    out: List[Email] = []
    for m in messages:
        if not isinstance(m, dict):
            continue
        raw = dict(m)
        # 直接映射字段，归一化按多候选字段取值
        raw["from"] = m.get("from_email", "")
        raw["to"] = email
        raw["text"] = m.get("text_body", "")
        raw["html"] = m.get("html_body", "")
        raw["date"] = m.get("received_at", "")
        out.append(normalize_email(raw, email))
    return out