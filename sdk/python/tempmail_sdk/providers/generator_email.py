"""
GeneratorEmail 渠道实现（generator.email，PHP SSR 网页型）

调研实证结论（2026-09-28，与 Go 端 generator_email.go 一致）：
  - 无独立建箱 API：服务器端渲染直接生成随机邮箱并写进页面内联
    window.SITE_DATA：cur_user:"hripynok"、cur_domain:"redproxies.com"
    （邮箱=user@domain），同页 Set-Cookie: inbox_ctx=redproxies.com%2Fhripynok
    （URL 编码）选中该邮箱会话。
  - 读信同为 SSR：GET /inbox4/（轮换别名 /inbox{1..4}/，hom 页同构）
    带 inbox_ctx Cookie 返回该邮箱渲染页。信件列表渲染在 #email-table，
    每条为 div.list-group-item 内三个子 div：g8r.from_div_45g45gg（From）、
    g8r.subj_div_45g45gg（Subject）、g8r.time_div_45g45gg（Time (UTC)）。
    空箱时容器留空、num_mess=0。
  - 限制：本站邮件正文不提供纯文本/HTML 原文（默认渲染摘要），
    SDK 按列表三要素归一。

会话隔离：邮箱由服务端依据 inbox_ctx Cookie 选择，本渠道维护专属
  requests.Session（Cookie 由本渠道独占，防污染全局会话）。
Token 语义：token 保存 {email, domain, user} JSON 快照。
"""

import json
import re
from typing import List, Optional

import requests

from ..config import get_config
from ..normalize import normalize_email
from ..types import Email, EmailInfo

CHANNEL = "generator-email"
BASE_URL = "https://generator.email"
INBOX_URL = BASE_URL + "/inbox4/"

# 固定浏览器 UA（与 Go 端 tls-client 同款形态）
_USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"
)

# SITE_DATA 快照提取（cur_user / cur_domain）
_USER_RE = re.compile(r'cur_user:"([^"]*)"')
_DOMAIN_RE = re.compile(r'cur_domain:"([^"]*)"')
# 列表条目与三要素块（类名后缀锚定）
_ITEM_RE = re.compile(
    r'(?s)<div[^>]*class="[^"]*list-group-item[^"]*"[^>]*>(.*?)(?:</div>\s*){3}'
)
_FROM_RE = re.compile(
    r'(?s)class="[^"]*from_div_45g45gg[^"]*"[^>]*>(.*?)</div>'
)
_SUBJ_RE = re.compile(
    r'(?s)class="[^"]*subj_div_45g45gg[^"]*"[^>]*>(.*?)</div>'
)
_TIME_RE = re.compile(
    r'(?s)class="[^"]*time_div_45g45gg[^"]*"[^>]*>(.*?)</div>'
)
_SCRIPT_RE = re.compile(r"(?is)<(script|style)[\s\S]*?</\1>")
_TAG_RE = re.compile(r"<[^>]+>")

# 模块级专属会话（inbox_ctx Cookie 由本渠道独占）
_sess: Optional[requests.Session] = None


def _session() -> requests.Session:
    """
    模块级专属 requests.Session（同 ..config 配置源）

    不复用全局 Session 是因为邮箱完全由 inbox_ctx Cookie 上下文决定，
    串用会污染其它渠道。
    """
    global _sess
    if _sess is None:
        config = get_config()
        _sess = requests.Session()
        if config.proxy:
            _sess.proxies = {"http": config.proxy, "https": config.proxy}
        _sess.verify = not config.insecure
        if config.headers:
            _sess.headers.update(config.headers)
    return _sess


def _headers() -> dict:
    """同站浏览器特征头（读页/收信页共用）"""
    return {
        "User-Agent": _USER_AGENT,
        "Accept": (
            "text/html,application/xhtml+xml,application/xml;q=0.9,"
            "image/avif,image/webp,*/*;q=0.8"
        ),
        "Accept-Language": "en-US,en;q=0.9",
        "Referer": BASE_URL + "/",
    }


def _fetch_page() -> str:
    """请求收件箱渲染页并返回 HTML（邮箱由服务端依据 inbox_ctx Cookie 选择）"""
    config = get_config()
    resp = _session().get(INBOX_URL, headers=_headers(), timeout=config.timeout)
    if resp.status_code < 200 or resp.status_code >= 300:
        raise RuntimeError(
            f"generator-email: 请求失败 http {resp.status_code}: "
            f"{resp.text[:200]}"
        )
    return resp.text


def _strip_tags(s: str) -> str:
    """去标签与脚本/样式块，压缩空白"""
    s = _SCRIPT_RE.sub(" ", s)
    return re.sub(
        r"\s+", " ", _TAG_RE.sub(" ", s)
    ).strip()


def _match_first(pattern, src: str) -> str:
    """取正则第一个捕获组，无匹配返回空串"""
    m = pattern.search(src)
    return m.group(1) if m else ""


def generate_email(channel: str = CHANNEL) -> EmailInfo:
    """
    创建 generator.email 临时邮箱

    解析首页 SITE_DATA 快照（cur_user/cur_domain）得到邮箱地址；
    token 保存 {email, domain, user} JSON 快照。
    """
    src = _fetch_page()
    user = _match_first(_USER_RE, src)
    domain = _match_first(_DOMAIN_RE, src)
    if not user or not domain:
        raise RuntimeError(
            "generator-email: 首页未携带邮箱快照（cur_user/cur_domain）"
        )
    email = user + "@" + domain
    return EmailInfo(
        channel=channel,
        email=email,
        _token=json.dumps({"email": email, "domain": domain, "user": user}),
    )


def get_emails(email: str, token: Optional[str] = None) -> List[Email]:
    """
    获取 generator.email 收件箱

    解析收件箱渲染页的列表条目（from/subj/time 三要素）；本站不提供
    原文正文，SDK 按摘要归一。

    @param email 邮箱地址
    @param token 会话凭据 JSON（{email, domain, user}）
    """
    try:
        sess = json.loads(token or "")
    except (ValueError, TypeError) as err:
        raise ValueError(f"generator-email: 会话凭据解析失败: {err}") from err
    if not isinstance(sess, dict):
        raise ValueError("generator-email: 会话凭据解析失败（非对象）")
    if sess.get("email") != email:
        raise ValueError("generator-email: 会话邮箱与查询邮箱不匹配")

    src = _fetch_page()
    # 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换
    if _match_first(_DOMAIN_RE, src) != sess.get("domain"):
        raise RuntimeError(
            "generator-email: 会话域名已切换"
            f"（token {sess.get('domain')}，服务端 {_match_first(_DOMAIN_RE, src)}）"
        )

    # 列表区域锚定（#email-table ... #markodile 之间）
    list_start = src.find('id="email-table"')
    list_end = src.find('id="markodile"')
    region = ""
    if list_start >= 0 and list_end > list_start:
        region = src[list_start:list_end]

    out: List[Email] = []
    for m in _ITEM_RE.finditer(src):
        raw = m.group(1)
        # 跳过列表容器外的候选：要求条目文本来自列表区域
        if region and raw not in region:
            continue
        from_addr = _strip_tags(_match_first(_FROM_RE, raw))
        subject = _strip_tags(_match_first(_SUBJ_RE, raw))
        when = _strip_tags(_match_first(_TIME_RE, raw))
        if not from_addr and not subject and not when:
            continue
        out.append(
            normalize_email(
                {
                    "from": from_addr,
                    "to": email,
                    "subject": subject,
                    "date": when,
                },
                email,
            )
        )
    return out