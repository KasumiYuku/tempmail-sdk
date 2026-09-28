"""
Mailticking 渠道实现（www.mailticking.com，旧域名 temporary-mail.net 的更名站）

实测协议（JSON 交流、Cloudflare 前置、需浏览器 UA）：
  1. 建箱  POST /get-mailbox   body {"types":["4"]}（4=独立域名；排除
     Gmail 别名（type "2"））
     响应 {"success":true,"email":"xxx@domain","activate_token":"..."}
  2. 激活  POST /activate-email body {"email":..,"source":"homepage",
     "activate_token":..}      响应 {"success":true}，并下发
     active_mailbox / temp_mail_history Cookie
  3. 同步  GET /?email=..&activate_token=..  服务端把该邮箱写入活跃状态，
     页面 #active-mail 渲染 value="{email}" data-code="{64位列信码}"；
     列信码是"当前活跃邮箱"的派生凭据，与 activate_token 是两套值
  4. 列信  POST /get-emails?lang=en body {"email":..,"code":列信码}
     空箱实测响应 {"emails":[],"success":true}；`?lang=` 缺省直接 400；
     邮箱不活跃（code 失效）时回 {"error":"Invalid request","success":false}

token 语义：单字符串 "{列信码}|{email}"，生成时已与服务器同步。

读信协议：官网没有可观察的独立读信端点，当前实现只提供列表；
列表元素的字段名没有站点文档佐证，不做任何猜测，交给 normalize_email 的
既有候选字段策略提取。待详情端点或字段结构有明确证据后再升级。
"""

import json
import re
from typing import Dict, List, Optional
from urllib.parse import quote

from .. import http as tm_http
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "mailticking"
BASE_URL = "https://www.mailticking.com"

_USER_AGENT = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
)

_API_HEADERS = {
    "Accept": "application/json",
    "Accept-Language": "en-US,en;q=0.9",
    "Content-Type": "application/json",
    "User-Agent": _USER_AGENT,
    "Referer": f"{BASE_URL}/",
    "Origin": BASE_URL,
}

_PAGE_HEADERS = {
    "Accept": "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
    "Accept-Language": "en-US,en;q=0.9",
    "User-Agent": _USER_AGENT,
}

# 从首页抽取 #active-mail 输入框起始标签，再从中取 value 与 data-code
_INPUT_TAG_RE = re.compile(r"<input\b[^>]*\bid=['\"]active-mail['\"][^>]*>", re.S)
_VALUE_RE = re.compile(r"\bvalue=['\"]([^'\"]*)['\"]")
_DATA_CODE_RE = re.compile(r"\bdata-code=['\"]([^'\"]*)['\"]")

# 候选发件人键：列表元素字段全名没有站点文档佐证，按常见字段多候选提取
_FROM_KEYS = [
    "mail_from",
    "from_mail",
    "from_email",
    "sender_address",
    "from_address",
    "send_addr",
    "mail_addr",
    "address_from",
    "ho_from",
    "fromname",
    "fromS",
]


def _error_text(data: dict, fallback: str) -> str:
    """从响应中提取 error 优先、message 次之的错误文案"""
    if isinstance(data.get("error"), str) and data["error"].strip():
        return data["error"].strip()
    if isinstance(data.get("message"), str) and data["message"].strip():
        return data["message"].strip()
    return fallback


def _post(path: str, payload: dict) -> dict:
    """执行 POST JSON 请求并解析响应；非 2xx 时优先使用响应体中的错误文案"""
    resp = tm_http.post(
        f"{BASE_URL}{path}",
        headers=_API_HEADERS,
        data=json.dumps(payload),
    )
    try:
        data = resp.json() if isinstance(resp.json(), dict) else {}
    except ValueError:
        # 站点偶尔返回非 JSON 网关文案，保留原样供错误提示使用
        data = {}
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"mailticking: http {resp.status_code}: "
            f"{_error_text(data, resp.text.strip())}"
        )
    return data


def _get_page(url: str) -> str:
    """带参 GET 首页文本，用于抽取列信码"""
    resp = tm_http.get(url, headers=_PAGE_HEADERS)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(f"mailticking: http {resp.status_code}")
    return resp.text


def _parse_active_mail(page: str):
    """从首页 HTML 解析活跃邮箱 value 与 data-code，失败返回 None"""
    tag = _INPUT_TAG_RE.search(page)
    if not tag:
        return None
    val = _VALUE_RE.search(tag.group(0))
    cod = _DATA_CODE_RE.search(tag.group(0))
    if not val or not cod:
        return None
    pair = (val.group(1).strip(), cod.group(1).strip())
    if not pair[0] or not pair[1]:
        return None
    return pair


def _sync_code(email: str, activate_token: str):
    """
    执行激活后的会话同步：以查询参数加载首页，从 #active-mail 解析
    value（邮箱）与 data-code（列信码）。失败抛出异常。
    """
    url = (
        f"{BASE_URL}/?email={quote(email)}"
        f"&activate_token={quote(activate_token)}"
    )
    page = _get_page(url)
    pair = _parse_active_mail(page)
    if not pair:
        raise RuntimeError("mailticking: sync inbox code failed")
    return pair


def _split_token(token: str, email: str):
    """从 token 拆出列信码与邮箱；无分隔符时整个 token 视为列信码"""
    if "|" in token:
        code, tok_email = token.split("|", 1)
        return code.strip(), tok_email.strip()
    return token.strip(), (email or "").strip()


def _migrated_fields(raw: dict) -> dict:
    """尽量映射列表字段到统一字段名，未命中的字段留给 normalize_email 自己处理"""
    m = dict(raw)
    if m.get("from") is None:
        for key in _FROM_KEYS:
            v = m.get(key)
            if isinstance(v, str) and v.strip():
                m["from"] = v
                break
    if m.get("sender") is None:
        f = m.get("from")
        if isinstance(f, str) and f.strip():
            m["sender"] = f
    if m.get("id") is None and m.get("mail_id") is not None:
        m["id"] = m["mail_id"]
    if m.get("date") is None and m.get("received_at") is not None:
        m["date"] = m["received_at"]
    return m


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """
    创建 mailticking 邮箱账号（type=4 独立域名）。
    流程：建箱 → 激活 → 带参加载首页同步出列信码；token 存 "{列信码}|{email}"。
    """
    box = _post("/get-mailbox", {"types": ["4"]})
    if not box.get("success"):
        raise RuntimeError(
            f"mailticking: get-mailbox failed: {_error_text(box, 'unknown error')}"
        )
    box_email = str(box.get("email") or "").strip()
    activate_token = str(box.get("activate_token") or "").strip()
    if not box_email:
        raise RuntimeError("mailticking: get-mailbox returned empty email")
    if not activate_token:
        raise RuntimeError("mailticking: get-mailbox returned empty activate_token")

    # 激活邮箱，服务端据此下发 active_mailbox Cookie 并登记活跃状态
    act = _post(
        "/activate-email",
        {
            "email": box_email,
            "source": "homepage",
            "activate_token": activate_token,
        },
    )
    if not act.get("success"):
        raise RuntimeError(
            f"mailticking: activate-email failed: {_error_text(act, 'unknown error')}"
        )

    # 会话同步：带参加载首页，取列信码（列信必需的最新派生凭据）
    pair = _sync_code(box_email, activate_token)
    if pair[0] != box_email:
        raise RuntimeError(
            f"mailticking: inbox session mismatch: want {box_email} got {pair[0]}"
        )

    return EmailInfo(
        channel=channel,
        email=box_email,
        token=f"{pair[1]}|{box_email}",
    )


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """
    获取 mailticking 邮箱的邮件列表。
    发送 {"email":邮箱, "code":token 中的列信码}，空箱返回空列表不报错。
    限制说明：读信正文无公开端点，仅提供列表；字段名候选提取，能识别
    多少字段取决于站点实际响应。
    """
    address = (email or "").strip()
    if not address:
        raise ValueError("mailticking: empty email")
    token_raw = (token or "").strip()
    if not token_raw:
        raise ValueError("mailticking: empty token")
    code, tok_email = _split_token(token_raw, address)

    list_data = _post("/get-emails?lang=en", {"email": tok_email, "code": code})
    if not list_data.get("success"):
        if list_data.get("needNewEmail"):
            raise RuntimeError("mailticking: mailbox expired, please renew")
        if tok_email == address:
            # 无 activate_token 可重放：直接报错，避免误把邮箱判死
            raise RuntimeError(
                "mailticking: get-emails rejected, refresh inbox in Generate"
            )
        raise RuntimeError("mailticking: get-emails failed")
    if tok_email != address:
        raise RuntimeError("mailticking: token email mismatch")

    out = []
    for raw in list_data.get("emails") or []:
        if not isinstance(raw, dict):
            continue
        out.append(normalize_email(_migrated_fields(raw), address))
    return out