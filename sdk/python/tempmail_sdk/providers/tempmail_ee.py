"""
TempmailEE 渠道实现（tempmail.ee）

出口风控实证结论（2026-09-27 实测）：
  平台读信端 /api/mails 的 403 Access denied 不是 TLS 指纹、请求头顺序
  或浏览器真实性校验，而是「会话 Cookie 绑定校验」：
  - change（换箱）返回的 Set-Cookie 中 temp_mail_session
    （形如 tmapi_sess_xxx）与 temp_email 共同构成读信凭据；
  - 同会话查别的邮箱、只带 session 不带 temp_email、只带 visitor
    追踪 cookie，均 403；带齐 temp_email + temp_mail_session 即 200。
  因此只要在同一个「事务」内完成 change → 提取 Set-Cookie → 读信，
  纯云端 SDK 即可真实读到邮件，无需真实浏览器。

会话隔离：建箱与读信全部使用无 Cookie 罐的独立 requests 会话，
  会话凭据由本渠道以显式 Cookie 请求头逐请求携带，杜绝与全局
  Cookie 罐残留会话串池、也避免同行并发相互污染。

已验证通行配方（实测均 200）：
  - POST /api/mails 无 sec-ch-ua 头也能通过（有正确会话 Cookie 时），
    但为贴近真实浏览器 fetch，仍带上完整 sec-ch-ua 三件套，
    UA 使用固定 Chrome 154（与其 sec-ch-ua 品牌版本一致）。
  - POST /api/mailbox/change 若不带 sec-ch-ua 头会被平台拒绝
    （403 Browser request required），带齐即 200，并下发会话 Cookie。
"""

import base64
import html
import json
import re
from datetime import datetime, timedelta, timezone
from typing import List

import requests

from ..config import get_config
from ..html_utils import html_to_text
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "tempmail-ee"
BASE_URL = "https://tempmail.ee"

# 与 sec-ch-ua 品牌版本一致的固定 UA（Chrome 154 / Linux）
_USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"
)

# sec-ch-ua 三件套（change 必须，平台据此放行）
_SEC_CH_UA = '"Chromium";v="154", "Google Chrome";v="154", "Not.A/Brand";v="99"'

# Token 前缀，用于识别本渠道会话凭据串
_TOKEN_PREFIX = "tempmail-ee|"


def _browser_integrity() -> dict:
    """建箱提交的浏览器指纹（与官方前端一致）"""
    return {
        "webdriver": False,
        "languagesMissing": False,
        "languageMissing": False,
        "pluginsMissing": False,
        "pluginsUndefined": False,
        "outerSizeMissing": False,
        "innerSizeMissing": False,
        "screenMissing": False,
        "screenDepthMissing": False,
        "timezoneMissing": False,
        "timezoneOffsetMissing": False,
        "userAgentDataPresent": True,
        "userAgentMissing": False,
        "platformClass": "Linux",
        "mobile": False,
        "collectionFailed": False,
    }


def _session() -> requests.Session:
    """创建无 Cookie 罐、带全局配置的独立 requests.Session"""
    config = get_config()
    sess = requests.Session()
    if config.proxy:
        sess.proxies = {"http": config.proxy, "https": config.proxy}
    sess.verify = not config.insecure
    if config.headers:
        sess.headers.update(config.headers)
    return sess


def _set_browser_headers(headers: dict, with_sec_ch: bool) -> dict:
    """设置浏览器特征安全头（同站 fetch 全套）"""
    headers.update(
        {
            "Accept": "application/json",
            "Content-Type": "application/json",
            "X-Requested-With": "XMLHttpRequest",
            "Origin": BASE_URL,
            "Referer": f"{BASE_URL}/",
            "Sec-Fetch-Site": "same-origin",
            "Sec-Fetch-Mode": "cors",
            "Sec-Fetch-Dest": "empty",
            "Accept-Language": "en-US,en;q=0.9",
            "User-Agent": _USER_AGENT,
        }
    )
    if with_sec_ch:
        headers.update(
            {
                "Sec-Ch-Ua": _SEC_CH_UA,
                "Sec-Ch-Ua-Mobile": "?0",
                "Sec-Ch-Ua-Platform": '"Linux"',
            }
        )
    return headers


def _build_token(email: str, session: str) -> str:
    """由邮箱与 temp_mail_session 组装渠道内部凭据串"""
    return f"{_TOKEN_PREFIX}temp_email={email}; temp_mail_session={session}"


def _parse_token(token: str, email: str):
    """解析读信凭据，返回 (cookie, ok)；会话绑定邮箱以请求邮箱为准重拼，防止凭据与邮箱错配"""
    if not (token or "").startswith(_TOKEN_PREFIX):
        return "", False
    cred = token[len(_TOKEN_PREFIX):]
    session = ""
    for part in cred.split(";"):
        kv = part.strip()
        if kv.startswith("temp_mail_session="):
            session = kv[len("temp_mail_session="):]
    if not session:
        return "", False
    return f"temp_email={email}; temp_mail_session={session}", True


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """
    创建 tempmail.ee 临时邮箱

    事务会话：GET / 面熟 → POST /api/mailbox/change（sec-ch-ua 全套头）换新邮箱
    → 提取 Set-Cookie 中的 temp_mail_session，随 EmailInfo token 透传给读信。
    """
    config = get_config()
    sess = _session()

    # 步骤 1：GET / 建立 Cookie 会话（面熟首访），失败不阻断后续流程
    try:
        _ = sess.get(
            BASE_URL,
            headers={
                "User-Agent": _USER_AGENT,
                "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            },
            timeout=config.timeout,
        )
    except Exception:
        pass

    # 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua）
    body = json.dumps(
        {"turnstileToken": None, "browserIntegrity": _browser_integrity()}
    )
    resp = sess.post(
        f"{BASE_URL}/api/mailbox/change",
        headers=_set_browser_headers({}, True),
        data=body,
        timeout=config.timeout,
    )
    resp.raise_for_status()
    data = resp.json()
    if not data.get("success") or not data.get("newEmail"):
        raise RuntimeError(f"tempmail-ee: 建箱失败 {data!r}")

    # 步骤 3：从 Set-Cookie 接管会话凭据，供读信校验使用
    cookie_email = resp.cookies.get("temp_email")
    session = resp.cookies.get("temp_mail_session")
    email = data.get("newEmail")
    if cookie_email and cookie_email != email:
        # 以防万一以响应体为准，Cookie 仅取 session
        email = cookie_email
    if not session:
        raise RuntimeError("tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信")

    expires = data.get("expiresAt") or (
        datetime.now(timezone.utc) + timedelta(minutes=60)
    ).isoformat()

    return EmailInfo(
        channel=channel,
        email=email,
        _token=_build_token(email, session),
        expires_at=expires,
    )


def get_emails(email: str, token: str = "") -> List[Email]:
    """
    读取 tempmail.ee 收件箱

    凭据来自 Generate 时从 change 响应提取的 temp_mail_session，
    逐请求以显式 Cookie 头携带（temp_email + temp_mail_session 缺一不可）。
    /api/mails 列表只含元数据，正文逐一请求 GET /api/mails/{id}，
    响应的 content 字段为 multipart（RFC822 MIME 原始，HTML 实体已转义），
    解码拆出 text / html；单封详情拉取失败不阻塞列表其余邮件。
    """
    config = get_config()
    addr = (email or "").strip()
    if not addr:
        raise ValueError("tempmail-ee: 邮箱为空")
    if not (token or "").strip():
        raise ValueError("tempmail-ee: token 为空")

    cookie, ok = _parse_token(token, addr)
    if not ok:
        raise ValueError("tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱")

    sess = _session()
    resp = sess.post(
        f"{BASE_URL}/api/mails",
        headers=_set_browser_headers({"Cookie": cookie}, True),
        data=json.dumps({"email": addr}),
        timeout=config.timeout,
    )
    if resp.status_code == 403:
        raise RuntimeError(
            "tempmail-ee: http 403（会话 Cookie 校验失败，邮箱可能已过期，请重新 Generate）"
        )
    resp.raise_for_status()
    data = resp.json()
    mails = data.get("mails") if isinstance(data, dict) else None
    if not isinstance(mails, list):
        return []

    out: List[Email] = []
    for m in mails:
        if not isinstance(m, dict):
            continue
        mid = m.get("id")
        if not mid:
            continue
        skeleton = {
            "id": str(mid),
            "from": m.get("fromAddress", m.get("from", m.get("sender", ""))),
            "to": m.get("toAddress", m.get("to", addr)),
            "subject": m.get("subject", ""),
            "date": m.get("createdAt", m.get("date", m.get("receivedAt", ""))),
            "is_read": m.get("isRead", False),
            "text": "",
            "html": "",
        }
        try:
            detail = _fetch_detail(sess, cookie, mid)
        except Exception:
            # 单封详情拉取失败不阻塞列表其余邮件（详情偶发 4xx/网络抖动）
            continue
        content = (
            detail.get("content")
            or detail.get("text")
            or detail.get("body")
            or detail.get("html")
        )
        if not content:
            continue
        text, html_part = _parse_content(content)
        skeleton["text"] = text
        skeleton["html"] = html_part
        # 详情缺失 from/subject 时补全（列表行已带则不动）
        if not skeleton["from"]:
            skeleton["from"] = detail.get("fromAddress", detail.get("from", ""))
        if not skeleton["subject"]:
            skeleton["subject"] = detail.get("subject", "")
        out.append(normalize_email(skeleton, addr))
    return out


def _fetch_detail(sess: requests.Session, cookie: str, mail_id) -> dict:
    """拉取单封详情，GET /api/mails/{id}（带显式会话 Cookie）"""
    resp = sess.get(
        f"{BASE_URL}/api/mails/{mail_id}",
        headers=_set_browser_headers({"Cookie": cookie}, False),
        timeout=get_config().timeout,
    )
    resp.raise_for_status()
    data = resp.json()
    return data if isinstance(data, dict) else {}


_SCRIPT_RE = re.compile(r"(?is)<script[\s\S]*?</script>")
_TAG_RE = re.compile(r"(?s)<[^>]+>")


def _html_to_text_local(src: str) -> str:
    """将 HTML 转为纯文本（与根包逻辑一致的本地版本）"""
    cleaned = _SCRIPT_RE.sub(" ", src)
    cleaned = _TAG_RE.sub(" ", cleaned)
    return html_to_text(cleaned)


def _parse_content(raw: str):
    """
    解析详情 content：

    1) 平台已做 HTML 实体转义（= 写成 &#61;、+ 写成 &#43;、/ 写成 &#47;），
       先反转义；之后补充标准 HTML 反转义。
    2) content 是 MIME multipart 原文（含顶部边界装饰头），按首个边界行
       切块，各 part 依据 Content-Transfer-Encoding 做 base64 /
       quoted-printable 解码，text part 入文本、html part 入 HTML。

    返回 (纯文本正文, HTML 正文)，两者之一为空时互为兜底合成。
    """
    payload = html.unescape(raw)
    # unescape 已处理数值实体，此替换为兜底（与 Go 端一致）
    payload = payload.replace("&#61;", "=").replace("&#43;", "+").replace("&#47;", "/")
    payload = payload.replace("\r\n", "\n")

    lines = payload.split("\n")
    boundary = _boundary_of(lines)
    text, html_part = ("", "")
    if boundary:
        text, html_part = _parts(lines, boundary)
    if not boundary or (not text and not html_part):
        # 无有效 multipart 结构（如邮件 body 仅单个 part）：整个 content 去壳后作为正文
        text, html_part = _singleton(payload)
    if not text and html_part:
        text = _html_to_text_local(html_part)
    if not html_part and text:
        html_part = (
            "<html><body><pre>" + html.escape(text) + "</pre></body></html>"
        )
    return text, html_part


def _singleton(payload: str):
    """
    单 part（无边界或拆不出内容）降级解析：
    依次按「头部区隔（首个空行之后）→ 整体」取内容，
    并通过关键字识别 Content-Transfer-Encoding 做解码。
    """
    body = payload.strip()
    if "\n" not in body:
        return body, ""
    lines = payload.split("\n")
    # 头部以 RFC822 形式出现（首行含冒号键值）时，正文从首个空行后开始
    for i, ln in enumerate(lines):
        if not ln.strip():
            body = "\n".join(lines[i + 1:])
            break
        if i > 40 or (i >= 3 and ":" not in ln):
            break
    lower = payload.lower()
    cte = ""
    for enc in ("base64", "quoted-printable"):
        if enc in lower:
            cte = enc
    return _decode_part(body, cte), ""


def _boundary_of(lines: List[str]) -> str:
    """扫描首块寻找边界行（默认 multipart 边界行处于块首）"""
    for i in range(min(len(lines), 120)):
        if lines[i].startswith("--") and len(lines[i]) > 2:
            return lines[i].rstrip("\r")[2:]
    return ""


def _parts(lines: List[str], boundary: str):
    """按 boundary 拆分 multipart 并解码归并 text/html 两个 part"""
    text, html_part = ("", "")
    for i in range(len(lines)):
        if not lines[i].startswith("--" + boundary):
            continue
        if lines[i].startswith("--" + boundary + "--"):
            break
        headers = {}
        j = i + 1
        # part 头部：直到首个空行（RFC 空行在 Content-* 头之后）
        while j < len(lines) and lines[j] != "" and not lines[j].startswith("--" + boundary):
            ln = lines[j]
            if ":" in ln:
                k = ln.index(":")
                if k > 0:
                    headers[ln[:k].strip().lower()] = ln[k + 1:].strip()
            j += 1
        if j < len(lines) and lines[j] == "":
            j += 1
        # part 正文：到下一个边界行为止，内部空行属于正文内容
        body = []
        while j < len(lines) and not lines[j].startswith("--" + boundary):
            body.append(lines[j])
            j += 1
        text, html_part = _merge_part(body, headers, text, html_part)
        i = j - 1
    return text, html_part


def _merge_part(body: List[str], headers: dict, text: str, html_part: str):
    """单个 part 的解码归并：按 content-type 归类，缺省回退 text 槽"""
    ct = headers.get("content-type", "").lower()
    if ";" in ct:
        ct = ct.split(";", 1)[0]
    cte = headers.get("content-transfer-encoding", "").lower()
    content = "\n".join(body)
    if "text/plain" in ct:
        if not text:
            text = _decode_part(content, cte)
        return text, html_part
    if "text/html" in ct:
        if not html_part:
            html_part = _decode_part(content, cte)
        return text, html_part
    if not text:
        text = _decode_part(content, cte)
    return text, html_part


def _decode_part(data: str, cte: str) -> str:
    """按 Content-Transfer-Encoding 解码 part 内容"""
    data = data.strip()
    if cte == "base64":
        joined = re.sub(r"\s", "", data)
        try:
            return base64.b64decode(joined).decode("utf-8", "replace").strip()
        except Exception:
            pass
    elif cte == "quoted-printable":
        import quopri

        try:
            return (
                quopri.decodestring(data.encode("utf-8"))
                .decode("utf-8", "replace")
                .strip()
            )
        except Exception:
            pass
    return data