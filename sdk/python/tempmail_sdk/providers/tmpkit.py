"""
Tmpkit 渠道实现（tmpkit.com，Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）

调研实证结论（2026-09-28，与 Go 端 tmpkit.go 一致）：
  - POST /api/rpc/tempmail/initSession，body 为 tRPC 包裹 {"json":{}}，
    免凭据。响应 {"json":{"session":{sessionId,email,createdAt,expiresAt,
    extendedCount},"availableDomains":["tmpkit.com"]}}，同时
    Set-Cookie: tempmail_session=<sessionId>; Max-Age=3600（与 sessionId
    同值）。
  - POST /api/rpc/tempmail/getEmails，body {"json":{"offset":0,
    "limit":20}}，需带 tempmail_session Cookie。响应
    {"json":{"emails":[...],"total":N,"session":{"email","expiresIn"}}}；
    列表元素键为 mailId/from/subject/excerpt/date/timestamp/hasAttach/
    isRead（无 id 键）。
  - POST /api/rpc/tempmail/getEmailDetail，body
    {"json":{"mailId":<数字>}}（mailId 为数字，字符串会 zod 400）。
    响应为单封详情对象：mailId/from/to/subject/body/date/timestamp/
    contentType/sourceEmail；body 为含 <br> 的纯文本正文。

会话粘性：token 保存 sessionId（tempMailSession=<sid>），每次读信以
  显式 Cookie 请求头携带，防全局会话被并行会话覆盖后串箱。
"""

from typing import List, Optional

import requests

from ..config import get_config
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "tmpkit"
BASE_URL = "https://tmpkit.com"
RPC_PREFIX = BASE_URL + "/api/rpc/tempmail"

# 固定浏览器 UA（与 Go 端 tls-client 同款形态）
_USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"
)


def _rpc_headers() -> dict:
    """tRPC 调用请求头（Content-Type JSON、Accept 通配、同站 Origin/Referer）"""
    return {
        "User-Agent": _USER_AGENT,
        "Accept": "*/*",
        "Content-Type": "application/json",
        "Origin": BASE_URL,
        "Referer": BASE_URL + "/en",
    }


def _rpc(procedure: str, req_body: dict, cookie: str = "") -> dict:
    """
    对 tmpkit 发起 rpc 调用（tRPC 包裹 {"json":<reqBody>}）

    响应外层 {"json":{...}} 解析为字典返回；cookie 非空时以显式
    Cookie 请求头携带 tempmail_session（sessionId）。
    """
    config = get_config()
    headers = _rpc_headers()
    if cookie:
        headers["Cookie"] = "tempmail_session=" + cookie
    resp = requests.post(
        RPC_PREFIX + "/" + procedure,
        headers=headers,
        json={"json": req_body},
        timeout=config.timeout,
    )
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"tmpkit: {procedure} 失败 http {resp.status_code}: {resp.text[:200]}"
        )
    outer = resp.json()
    payload = outer.get("json")
    if not isinstance(payload, dict):
        raise RuntimeError(f"tmpkit: {procedure} 响应缺 json 载荷: {resp.text[:200]}")
    return payload


def _map_get(m: dict, key: str) -> str:
    """从 map 容错取字符串（与 Go 端 tmpkitMapGet 语义一致）"""
    if not isinstance(m, dict):
        return ""
    v = m.get(key)
    if v is None:
        return ""
    return str(v)


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """
    创建 tmpkit.com 临时邮箱

    调 initSession（tRPC 包裹 {"json":{}}），sessionId 与邮箱地址同返；
    token 约定为 tempMailSession=<sessionId>。
    """
    data = _rpc("initSession", {})
    sess = data.get("session")
    if not isinstance(sess, dict):
        raise RuntimeError("tmpkit: 创建会话响应缺 session 字段")
    email = _map_get(sess, "email").strip()
    session_id = _map_get(sess, "sessionId").strip()
    if not email or not session_id or "@" not in email:
        raise RuntimeError("tmpkit: 创建会话响应缺少必要字段（email/sessionId）")
    return EmailInfo(channel=channel, email=email, _token="tempMailSession=" + session_id)


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """
    获取 tmpkit.com 邮件列表

    getEmails（offset 0 / limit 20）取摘要，逐封 getEmailDetail 拉详情并
    将详情键并入摘要（键名不猜测，归一化交给 normalize 候选字段）；
    详情失败时回退列表摘要。getEmails 返回的 session.email 与收信邮箱
    不符时报错，防止会话被覆盖。

    @param email 邮箱地址
    @param token 会话凭据串（tempMailSession=<sessionId>）
    """
    mailbox = (token or "").strip()
    if mailbox.startswith("tempMailSession="):
        mailbox = mailbox[len("tempMailSession="):]
    if not mailbox:
        raise ValueError("tmpkit: 会话 token 为空")

    data = _rpc("getEmails", {"offset": 0, "limit": 20}, mailbox)
    # 会话指向校验：getEmails 响应自带 session.email（无 Cookie 时
    # session 为 null，同样视为会话失效）
    sess = data.get("session")
    if isinstance(sess, dict):
        got = _map_get(sess, "email").strip()
        if got and got != email:
            raise RuntimeError(
                f"tmpkit: 会话邮箱不匹配（响应 {got}，请求 {email}）"
            )
    else:
        raise RuntimeError("tmpkit: 会话已失效（getEmails 返回空会话）")

    rows = data.get("emails")
    if not isinstance(rows, list):
        raise RuntimeError("tmpkit: 邮件列表响应缺 emails 字段")

    out: List[Email] = []
    for item in rows:
        if not isinstance(item, dict):
            continue
        # mailId 为数字：合法时逐封拉详情（字符串会 zod 400，跳过详情）
        mail_id = item.get("mailId")
        if isinstance(mail_id, (int, float)) and mail_id > 0:
            try:
                detail = _rpc(
                    "getEmailDetail", {"mailId": int(mail_id)}, mailbox
                )
                for k in ("session", "emails", "total", "error"):
                    detail.pop(k, None)
                item.update(detail)
            except Exception:
                # 详情失败回退列表摘要
                pass
        out.append(normalize_email(item, email))
    return out