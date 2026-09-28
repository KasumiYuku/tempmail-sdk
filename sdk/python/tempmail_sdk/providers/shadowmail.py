"""
Shadowmail 渠道实现（shadowmail.win）
注册 POST /api/register；登录 POST /api/login（Set-Cookie: sessionId）；
建箱 POST /api/new-address；读信 POST /api/get-emails。会话以显式 Cookie 头逐请求携带。
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

CHANNEL = "shadowmail"
BASE_URL = "https://shadowmail.win"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


PASSWORD = "Abcd1234!"
DOMAIN = "shadowmail.win"
TOKEN_PREFIX = "shadowmail|"


def _random_account() -> str:
    """生成随机注册邮箱前缀（sdk+8 位小写字母）"""
    return "sdk" + "".join(random.choice("abcdefghijklmnopqrstuvwxyz") for _ in range(8))


def _register_or_login(account: str, password: str, is_login: bool) -> str:
    """注册或登录（POST /api/register、/api/login），返回会话 id（纯 uuid）"""
    path = "/api/login" if is_login else "/api/register"
    resp = tm_http.post(
        f"{BASE_URL}{path}",
        headers={**_HEADERS, "Content-Type": "application/json"},
        json={"email": account, "password": password},
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"shadowmail {path}: http {resp.status_code}")
    data = resp.json()
    message = data.get("message") or ""
    if is_login and message != "Successfull Login":
        raise RuntimeError(f"shadowmail login: {message}")
    # 重复注册（幂等）：消息为 Email already in use 时视为账号已存在
    if not is_login and message not in ("Successfully Registered", "Email already in use"):
        raise RuntimeError(f"shadowmail register: {message}")
    session = ""
    for resp_cookie in resp.cookies:
        if resp_cookie.name == "sessionId":
            session = resp_cookie.value
            break
    if is_login and not session:
        raise RuntimeError("shadowmail login: 未下发 sessionId Cookie")
    return session


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """注册账号并创建临时邮箱地址（token: account|password|sessionId）"""
    account = f"{_random_account()}@gmail.com"
    _register_or_login(account, PASSWORD, False)
    session = _register_or_login(account, PASSWORD, True)
    resp = tm_http.post(
        f"{BASE_URL}/api/new-address",
        headers={**_HEADERS, "Content-Type": "application/json", "Cookie": f"sessionId={session}"},
        data="{}",
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"shadowmail new-address: http {resp.status_code}")
    data = resp.json()
    address = str(data.get("address") or "").lower().strip()
    if not address or not address.endswith(f"@{DOMAIN}"):
        raise RuntimeError("shadowmail new-address: 响应缺少有效地址")
    token = f"{TOKEN_PREFIX}{account}|{PASSWORD}|{session}"
    return EmailInfo(channel=channel, email=address, token=token)


def _parse_token(token: str):
    """解析凭据串为 account/password/sessionId 三元组"""
    if not (token or "").startswith(TOKEN_PREFIX):
        raise ValueError("shadowmail: token 格式错误")
    parts = (token or "")[len(TOKEN_PREFIX):].split("|")
    if len(parts) != 3:
        raise ValueError("shadowmail: token 字段缺失")
    account, password, session = parts
    if not account or not password or not session:
        raise ValueError("shadowmail: token 凭据字段为空")
    return account, password, session


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """读取 shadowmail 收件箱（会话失效 401/404 自动重登录重试一次）"""
    address = (email or "").strip()
    account, password, session = _parse_token(token)

    def _fetch():
        resp = tm_http.post(
            f"{BASE_URL}/api/get-emails",
            headers={**_HEADERS, "Content-Type": "application/json",
                     "Cookie": f"sessionId={session}"},
            json={"address": address},
        )
        return resp.status_code, resp.text

    status, body = _fetch()
    # sessionId 最长 1 小时（Max-Age 3600），过期后重登录重试一次
    if status in (401, 404):
        new_session = _register_or_login(account, password, True)
        if new_session:
            session = new_session
            status, body = _fetch()
    if status < 200 or status >= 300:
        raise RuntimeError(f"shadowmail get-emails: http {status}")
    data = json.loads(body)
    if data.get("message") != "Emails read":
        raise RuntimeError(f"shadowmail get-emails: {data.get('message')}")
    out = []
    for raw in data.get("mails") or []:
        flat = dict(raw)
        flat["from"] = raw.get("sender")
        flat["to"] = address
        flat["date"] = raw.get("created_at")
        # 平台无 text/html 区分，body 为正文（默认按纯文本处理）
        flat["text"] = raw.get("body")
        out.append(normalize_email(flat, address))
    return out
