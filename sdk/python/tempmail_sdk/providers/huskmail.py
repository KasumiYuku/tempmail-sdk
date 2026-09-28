"""
Huskmail 渠道实现（huskmail.xyz）
POST /api/v1/accounts 建箱（body {}，响应 id/address/password/token/expiresAt/tier，token 为 JWT），
GET /v1/messages 读信（Header Authorization: Bearer <token>，响应 {"messages":[...]}），
GET /v1/messages/{id} 取单封详情（Bearer）。
收信域固定为 @huskmail.xyz（huskmail.space 无 MX）。
"""

from typing import List, Optional

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "huskmail"
BASE_URL = "https://api.huskmail.space"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def _auth_headers(token: str = "") -> dict:
    """生成带 Bearer 认证的请求头"""
    headers = dict(_HEADERS)
    if token:
        headers["Authorization"] = f"Bearer {token}"
    return headers


def _message_id_of(m: dict) -> str:
    """从列表元素中提取邮件 ID，候选字段 id/Id/slug/messageId/message_id"""
    for key in ("id", "Id", "slug", "messageId", "message_id"):
        v = m.get(key)
        if isinstance(v, str) and v.strip():
            return v.strip()
    return ""


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 huskmail（@huskmail.xyz）临时邮箱，POST /v1/accounts（空 JSON body）"""
    resp = tm_http.post(
        f"{BASE_URL}/v1/accounts",
        headers={**_auth_headers(), "Content-Type": "application/json"},
        json={},
    )
    resp.raise_for_status()
    data = resp.json()
    if not data.get("address") or not data.get("token"):
        raise RuntimeError("huskmail: 创建邮箱响应缺少必要字段")

    return EmailInfo(
        channel=channel,
        email=data["address"],
        _token=data["token"],
        expires_at=data.get("expiresAt"),
    )


def get_emails(email: str, token: str = "") -> List[Email]:
    """
    获取 huskmail 邮件列表

    流程：GET /v1/messages 取列表（{"messages":[...]}），对每个元素按 id 逐封 GET /v1/messages/{id}
    合并详情；详情失败时以列表摘要归一。
    """
    resp = tm_http.get(f"{BASE_URL}/v1/messages", headers=_auth_headers(token))
    resp.raise_for_status()
    data = resp.json()
    messages = data.get("messages") if isinstance(data, dict) else None
    if not isinstance(messages, list):
        return []

    out: List[Email] = []
    for m in messages:
        if not isinstance(m, dict):
            continue
        mid = _message_id_of(m)
        detail: Optional[dict] = None
        if mid:
            detail = _fetch_detail(token, mid)
        if detail:
            # 详情缺字段时以列表摘要为准
            merged = dict(detail)
            for k, v in m.items():
                merged.setdefault(k, v)
            out.append(normalize_email(merged, email))
        else:
            # 详情失败时回退为列表摘要
            out.append(normalize_email(dict(m), email))
    return out


def _fetch_detail(token: str, message_id: str) -> Optional[dict]:
    """获取 huskmail 单封邮件详情"""
    try:
        resp = tm_http.get(
            f"{BASE_URL}/v1/messages/{message_id}",
            headers=_auth_headers(token),
        )
        resp.raise_for_status()
        data = resp.json()
        return data if isinstance(data, dict) else None
    except Exception:
        return None