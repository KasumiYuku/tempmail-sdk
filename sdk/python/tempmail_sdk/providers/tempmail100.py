"""
TempMail100 渠道实现（tempmail100.com）
POST /init 初始化（code/data.token）；POST /web/generate 建随机地址（Authorization: <token>）；
GET /web/emails 读信列表（Authorization: <token>）。列表元素 content 恒空（平台限制，如实留空）。
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

CHANNEL = "tempmail100"
BASE_URL = "https://tempmail100.com"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


def _str_of(value) -> str:
    """接口字段值安全转换为字符串（非字符串数字转十进制字符串，其余返回空串）"""
    if isinstance(value, str):
        return value
    if isinstance(value, (int, float)):
        return str(int(value))
    return ""


def _read_of(value) -> bool:
    """read 字段归一为布尔已读标记（兼容 bool/number/string）"""
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return value != 0
    if isinstance(value, str):
        return value.strip().lower() == "true" or value.strip() == "1"
    return False


def _auth_headers(token: str) -> dict:
    """设置通用请求头与裸 token 认证（前端使用 Authorization: <token> 不带 Bearer）"""
    headers = dict(_HEADERS)
    if token:
        headers["Authorization"] = token
    return headers


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 tempmail100.com 临时邮箱（POST /init 取 token，再 POST /web/generate 建地址）"""
    resp = tm_http.post(f"{BASE_URL}/init", headers=_HEADERS)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"tempmail100: 初始化失败 http {resp.status_code}: {resp.text}")
    init = resp.json()
    token = ((init.get("data") or {}).get("token") or "").strip()
    if init.get("code") != 0 or not token:
        raise RuntimeError(f"tempmail100: 初始化响应异常: {resp.text}")
    resp = tm_http.post(f"{BASE_URL}/web/generate", headers=_auth_headers(token))
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"tempmail100: 创建地址失败 http {resp.status_code}: {resp.text}")
    gen = resp.json()
    address = ((gen.get("data") or {}).get("address") or "").strip()
    if gen.get("code") != 0 or not address or "@" not in address:
        raise RuntimeError(f"tempmail100: 创建地址响应异常: {resp.text}")
    return EmailInfo(channel=channel, email=address, token=token)


def _normalize_item(raw: dict, address: str) -> Email:
    """归一化 /web/emails 列表元素（content 平台恒空，如实留空）"""
    from_address = _str_of(raw.get("fromAddress"))
    from_name = _str_of(raw.get("fromName"))
    # fromName+fromAddress 组合为 "Name <address>" 填入 from
    if from_name and from_address and from_name.lower() != from_address.lower() \
            and "@" in from_address:
        from_address = f"{from_name} <{from_address}>"
    flat = {
        "id": _str_of(raw.get("uuid")),
        "from": from_address,
        "to": _str_of(raw.get("toAddress")),
        "subject": _str_of(raw.get("subject")),
        "content": _str_of(raw.get("content")),
        "timestamp": raw.get("timestamp"),
        "isRead": _read_of(raw.get("read")),
    }
    return normalize_email(flat, address)


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """获取 tempmail100.com 邮件列表（GET /web/emails，Authorization 头为裸 token）"""
    address = (email or "").strip()
    resp = tm_http.get(f"{BASE_URL}/web/emails", headers=_auth_headers(token or ""))
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"tempmail100: 获取邮件列表失败 http {resp.status_code}: {resp.text}")
    data = resp.json()
    if data.get("code") != 0:
        raise RuntimeError(f"tempmail100: 获取邮件列表响应异常: {data.get('message')}")
    rows = ((data.get("data") or {}).get("list")) or []
    return [_normalize_item(raw, address) for raw in rows]
