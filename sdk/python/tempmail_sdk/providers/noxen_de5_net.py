"""
NoxenDe5Net 渠道实现（UniMail-Bot 公共实例 tempmail.noxen.de5.net）
登录 POST /api/login；建箱 GET /api/generate；读信 GET /api/emails?mailbox=&limit=20，
详情 GET /api/email/{id}，全文 GET /api/email/{id}/download（原始 EML 本地拆分 text/html）。
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

CHANNEL = "noxen-de5-net"
BASE_URL = "https://tempmail.noxen.de5.net"

_HEADERS = {
    "Accept": "application/json",
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36",
}


LOGIN_USER = "guest"
LOGIN_PASS = "123456"
TOKEN_PREFIX = "noxen-de5-net|"

_BOUNDARY_RE = re.compile(r'(?i)boundary="?([^";\s]+)"?')
_QP_RE = re.compile(r"=([0-9A-Fa-f]{2})")
_TAG_RE = re.compile(r"<script[\s\S]*?</script>|<style[\s\S]*?</style>|<[^>]+>")


def _login() -> str:
    """登录取得会话 Cookie（iding-session=JWT）"""
    resp = tm_http.post(
        f"{BASE_URL}/api/login",
        headers={**_HEADERS, "Content-Type": "application/json"},
        json={"username": LOGIN_USER, "password": LOGIN_PASS},
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"noxen-de5-net login: http {resp.status_code}")
    data = resp.json()
    if not data.get("success"):
        raise RuntimeError("noxen-de5-net login: 登录失败")
    cookie = resp.headers.get("Set-Cookie", "")
    for part in cookie.split(","):
        item = part.split(";")[0].strip()
        if item.startswith("iding-session="):
            return item
    raise RuntimeError("noxen-de5-net login: 未下发会话 Cookie")


def _cookie_still_valid(cookie: str) -> bool:
    """校验会话 Cookie 是否仍有效（GET /api/session）"""
    try:
        resp = tm_http.get(f"{BASE_URL}/api/session", headers={**_HEADERS, "Cookie": cookie})
        return resp.status_code == 200 and bool(resp.json().get("authenticated"))
    except Exception:
        return False


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """登录并创建临时邮箱（GET /api/generate，token 为凭据串）"""
    cookie = _login()
    resp = tm_http.get(f"{BASE_URL}/api/generate", headers={**_HEADERS, "Cookie": cookie})
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"noxen-de5-net generate: http {resp.status_code}")
    data = resp.json()
    address = (data.get("email") or "").strip()
    if not address:
        raise RuntimeError("noxen-de5-net generate: 响应缺少 email")
    token = f"{TOKEN_PREFIX}{cookie}|base={BASE_URL}"
    expires = data.get("expires") or 0
    import datetime
    expires_at = (
        (datetime.datetime.fromtimestamp(expires / 1000, tz=datetime.timezone.utc)
         .strftime("%Y-%m-%dT%H:%M:%SZ")
         if expires > 0 else None)
    )
    return EmailInfo(channel=channel, email=address, token=token, expires_at=expires_at)


def _boundary_of(content_type: str) -> str:
    """从 Content-Type 头值提取 multipart boundary"""
    match = _BOUNDARY_RE.search(content_type or "")
    return match.group(1) if match else ""


def _split_eml(payload: str, offset: int = 0):
    """将原始报文切分为首部映射与正文块"""
    lines = payload.split("\n")
    headers = {}
    i = offset
    if i < len(lines) and lines[i].startswith("From "):
        i += 1
    current_key = ""
    while i < len(lines):
        line = lines[i]
        if line == "":
            i += 1
            break
        if (line.startswith(" ") or line.startswith("\t")) and current_key:
            headers[current_key] += " " + line.strip()
            i += 1
            continue
        if ":" in line and line.index(":") > 0:
            current_key = line.split(":", 1)[0].strip().lower()
            headers[current_key] = line.split(":", 1)[1].strip()
        i += 1
    return headers, "\n".join(lines[i:])


def _split_multipart(body: str, boundary: str) -> list:
    """按 boundary 切出各 part（已剥离边界标记与首部空行）"""
    parts = []
    for segment in body.split(f"--{boundary}"):
        segment = segment.lstrip("\n")
        segment = segment.rstrip("--").rstrip("--\n") if segment.endswith("--") else segment
        if segment.strip():
            parts.append(segment)
    return parts


def _decode_part(data: str, encoding: str) -> str:
    """按 Content-Transfer-Encoding 解码 part 内容"""
    enc = (encoding or "").strip().lower()
    if enc == "base64":
        joined = re.sub(r"[\r\n\t ]", "", data)
        try:
            return base64.b64decode(joined).decode("utf-8", "replace").strip()
        except Exception:
            return data
    if enc == "quoted-printable":
        merged = re.sub(r"=\r?\n", "", data)
        return _QP_RE.sub(lambda m: chr(int(m.group(1), 16)), merged)
    return data.strip()


def _guess_html(body: str) -> str:
    """从整体原文抓取 <html>…</html> 片段（HTML 未正确声明时兜底）"""
    if not body:
        return ""
    lower = body.lower()
    start = lower.find("<html")
    if start == -1:
        start = lower.find("<!doctype html")
    if start == -1:
        return ""
    end = lower.rfind("</html>")
    if end == -1 or end < start:
        return ""
    return body[start:end + 7]


def _parse_entity(headers: dict, body: str):
    """递归解析单个 MIME 实体 → (纯文本正文, HTML 正文)"""
    content_type_raw = headers.get("content-type") or ""
    content_type = content_type_raw.lower()
    encoding = (headers.get("content-transfer-encoding") or "").lower()
    if not content_type.startswith("multipart/"):
        decoded = _decode_part(body, encoding)
        if "text/html" in content_type:
            return "", decoded
        return decoded, ""
    text, html_body = "", ""
    boundary = _boundary_of(content_type_raw)
    if boundary:
        for part in _split_multipart(body, boundary):
            part_headers, part_body = _split_eml(f"#participant\n{part}", 1)
            part_type = (part_headers.get("content-type") or "").lower()
            if part_type.startswith("multipart/"):
                t, h = _parse_entity(part_headers, part_body)
            elif part_type.startswith("message/rfc822"):
                nested_headers, nested_body = _split_eml(part_body, 0)
                t, h = _parse_entity(nested_headers, nested_body)
            elif "rfc822-headers" in part_type:
                continue
            else:
                t, h = _parse_entity(part_headers, part_body)
            if not text:
                text = t
            if not html_body:
                html_body = h
            if text and html_body:
                break
    if not html_body:
        html_body = _guess_html(body)
    return text, html_body


def _parse_eml(raw: bytes):
    """解析 EML 原始报文 → (纯文本正文, HTML 正文)"""
    payload = raw.decode("utf-8", "replace").replace("\r\n", "\n").replace("\r", "")
    headers, body = _split_eml(payload, 0)
    return _parse_entity(headers, body)


def _compose_placeholder(raw: dict) -> str:
    """verification_code 置顶 + preview 附后合成占位正文"""
    code = str(raw.get("verification_code") or "").strip()
    preview = str(raw.get("preview") or "").strip()
    parts = []
    if code:
        parts.append(f"验证码: {code}")
    if preview:
        parts.append(preview)
    return "\n\n".join(parts)


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """读取 noxen-de5-net 收件箱（列表 + 详情/EML 全文 + 占位兜底）"""
    address = (email or "").strip()
    if not (token or "").startswith(TOKEN_PREFIX):
        raise ValueError("noxen-de5-net: token 格式错误")
    cookie = (token or "")[len(TOKEN_PREFIX):]
    # 剥离尾缀 "|base=<基址>"，仅保留会话 Cookie 键值对
    cookie = cookie.split("|", 1)[0]
    if not _cookie_still_valid(cookie):
        cookie = _login()
    resp = tm_http.get(
        f"{BASE_URL}/api/emails?mailbox={address}&limit=20",
        headers={**_HEADERS, "Cookie": cookie},
    )
    if resp.status_code == 401:
        raise RuntimeError("noxen-de5-net 读信: http 401（会话失效或非本会话邮箱）")
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"noxen-de5-net 读信: http {resp.status_code}")
    rows = resp.json()
    if not isinstance(rows, list):
        raise RuntimeError("noxen-de5-net 读信: 响应非数组")
    out = []
    for raw in rows:
        flat = dict(raw)
        flat["from"] = raw.get("sender")
        flat["to"] = address
        flat["date"] = raw.get("received_at")
        flat["text"] = raw.get("preview")
        mid = str(raw.get("id") or "")
        if mid and mid != "0":
            full = False
            try:
                detail_resp = tm_http.get(
                    f"{BASE_URL}/api/email/{mid}", headers={**_HEADERS, "Cookie": cookie})
                if detail_resp.status_code == 200:
                    detail = detail_resp.json()
                    flat["content"] = detail.get("content")
                    flat["html_content"] = detail.get("html_content")
                    flat["to_addrs"] = detail.get("to_addrs")
                    download = detail.get("download")
                    if download:
                        eml_resp = tm_http.get(
                            (download if download.startswith("http") else BASE_URL + download),
                            headers={**_HEADERS, "Cookie": cookie,
                                     "Accept": "message/rfc822, */*"})
                        if eml_resp.status_code == 200:
                            text, html_body = _parse_eml(eml_resp.content)
                            if text or html_body:
                                flat["text"] = text
                                flat["html_content"] = html_body
                                full = True
            except Exception:
                pass
            if not full:
                flat["text"] = _compose_placeholder(raw)
        out.append(normalize_email(flat, address))
    return out
