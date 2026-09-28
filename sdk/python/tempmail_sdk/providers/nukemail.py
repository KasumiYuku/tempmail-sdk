"""
Nukemail 渠道实现（nukemail.app）
PoW 建箱：GET /api/pow/challenge?difficulty=4，本地求 nonce，POST /api/inbox/create；
读信：GET /api/inbox（Cookie: nukemail_token=<token>）。
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

CHANNEL = "nukemail"
BASE_URL = "https://nukemail.app"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


import hashlib


def _solve_pow(challenge: str, difficulty: int) -> int:
    """求解 PoW：返回使 SHA-256(challenge+nonce) 前 difficulty 位为 0 的最小 nonce"""
    prefix = "0" * difficulty
    nonce = 0
    while True:
        digest = hashlib.sha256(f"{challenge}{nonce}".encode()).hexdigest()
        if digest.startswith(prefix):
            return nonce
        nonce += 1


def _random_address() -> str:
    """生成本地随机名（与前端 generateRandomName 等价形态）"""
    chars = "abcdefghijklmnopqrstuvwxyz0123456789"
    return "nuke" + "".join(random.choice(chars) for _ in range(10))


def _pick_domain() -> str:
    """取第一个非 premium 的活跃域名"""
    resp = tm_http.get(f"{BASE_URL}/api/domains", headers=_HEADERS)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"nukemail domains: http {resp.status_code}")
    for item in resp.json().get("domains") or []:
        if not item.get("is_premium_only") and item.get("domain"):
            return item["domain"]
    raise RuntimeError("nukemail generate: 无可用非 premium 域名")


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """创建 nukemail 临时邮箱（PoW 建箱，token 为 NUKE-<随机> 访问码）"""
    resp = tm_http.get(f"{BASE_URL}/api/pow/challenge?difficulty=4", headers=_HEADERS)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"nukemail generate: challenge http {resp.status_code}")
    challenge = resp.json()
    if not challenge.get("id") or not challenge.get("challenge"):
        raise RuntimeError("nukemail generate: challenge 响应缺少 id/challenge")
    difficulty = challenge.get("difficulty") or 4
    nonce = _solve_pow(challenge["challenge"], difficulty)
    domain = _pick_domain()
    resp = tm_http.post(
        f"{BASE_URL}/api/inbox/create",
        headers={**_HEADERS, "Content-Type": "application/json"},
        json={"address": _random_address(), "domain": domain,
              "pow_id": challenge["id"], "pow_nonce": str(nonce)},
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"nukemail generate: create http {resp.status_code}: {resp.text.strip()}")
    data = resp.json()
    if not data.get("token") or not data.get("email"):
        raise RuntimeError("nukemail generate: create 响应缺少 token/email")
    return EmailInfo(channel=channel, email=data["email"], token=data["token"])


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """读取 nukemail 收件箱（GET /api/inbox，会话过期经 resume 重设后重试）"""
    address = (email or "").strip()
    access = (token or "").strip()
    if not access:
        raise ValueError("nukemail: token 为空")
    cookie = f"nukemail_token={access}"

    def _fetch():
        resp = tm_http.get(f"{BASE_URL}/api/inbox", headers={**_HEADERS, "Cookie": cookie})
        if resp.status_code < 200 or resp.status_code >= 300:
            raise RuntimeError(f"nukemail 读信: http {resp.status_code}")
        return resp.json()

    data = _fetch()
    if data.get("state") in ("expired", None, ""):
        try:
            tm_http.post(
                f"{BASE_URL}/api/inbox/resume",
                headers={**_HEADERS, "Content-Type": "application/json"},
                json={"accessCode": access},
            )
        except Exception:
            pass
        data = _fetch()
    out = []
    for raw in data.get("messages") or []:
        flat = dict(raw)
        flat["to"] = address
        if flat.get("text") is None and raw.get("body_text") is not None:
            flat["text"] = raw["body_text"]
        if flat.get("html") is None and raw.get("body_html") is not None:
            flat["html"] = raw["body_html"]
        flat["date"] = raw.get("received_at")
        flat["read"] = raw.get("read")
        flat["sender_email"] = raw.get("sender")
        out.append(normalize_email(flat, address))
    return out
