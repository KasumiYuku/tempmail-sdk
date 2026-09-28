"""
TenminApp 渠道实现（tenmin.app，真实 API 域 api.tenmin.app）
建箱+读信共用 GET /api/inbox/{localpart}（Accept: application/json）。
localpart 为随机 6 位小写十六进制串（首访即建箱，无显式创建接口）；
响应：{"inboxId":..,"address":"..@tenmin.app","ttl":600,"count":0,"messages":[]}，
messages[] 元素字段：id/from/subject/text/html/receivedAt（from 为 {name,address} 对象）。
非 /[0-9a-z.-]+ 的 localpart 会返回 400 invalid_inbox_id。
"""

import random
from datetime import datetime, timedelta, timezone
from typing import List

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "tenmin-app"
BASE_URL = "https://api.tenmin.app"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def _local() -> str:
    """生成 6 位小写十六进制随机 localpart"""
    return "".join(random.choice("0123456789abcdef") for _ in range(6))


def _fetch_inbox(localpart: str) -> dict:
    """请求 /api/inbox/{localpart}，返回解析后的收件箱响应"""
    resp = tm_http.get(f"{BASE_URL}/api/inbox/{localpart}", headers=_HEADERS)
    resp.raise_for_status()
    data = resp.json()
    if not isinstance(data, dict):
        raise RuntimeError("tenmin-app: 收件箱响应格式无效")
    return data


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 tenmin.app 临时邮箱：首次 GET 随机 localpart 即自动建箱（10 分钟 TTL），token 存储 localpart"""
    local = _local()
    data = _fetch_inbox(local)
    address = data.get("address")
    if not address:
        address = f"{local}@tenmin.app"

    ttl = data.get("ttl")
    expires = None
    if isinstance(ttl, (int, float)) and ttl > 0:
        expires = (
            datetime.now(timezone.utc) + timedelta(seconds=int(ttl))
        ).isoformat()

    return EmailInfo(channel=channel, email=address, _token=local, expires_at=expires)


def get_emails(email: str, token: str = "") -> List[Email]:
    """读取 tenmin.app 收件箱：复用建箱同一 localpart 轮询；from 为 {name,address} 对象，展开后交归一化处理"""
    local = (token or "").strip()
    if not local:
        raise ValueError("tenmin-app: token 为空")

    data = _fetch_inbox(local)
    messages = data.get("messages")
    if not isinstance(messages, list):
        return []

    out: List[Email] = []
    for m in messages:
        if not isinstance(m, dict):
            continue
        raw = dict(m)
        # from 为对象（{name,address}）时拆出地址字段
        from_obj = m.get("from")
        if isinstance(from_obj, dict):
            addr = from_obj.get("address")
            name = from_obj.get("name")
            if addr and name:
                raw["from"] = f"{name} <{addr}>"
            elif addr:
                raw["from"] = addr
            else:
                raw["from"] = name
        raw["to"] = email
        raw["text"] = m.get("text", "")
        raw["html"] = m.get("html", "")
        raw["date"] = m.get("receivedAt", "")
        out.append(normalize_email(raw, email))
    return out