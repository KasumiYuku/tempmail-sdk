package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * GeneratorEmail 渠道实现（generator.email，PHP SSR 网页型）。
 *
 * 与 Go 端 generator_email.go 协议一致（2026-09-28 抓包实证）：
 * - 无独立建箱 API：服务器端渲染直接生成随机邮箱并写进页面内联
 *   window.SITE_DATA：cur_user / cur_domain（邮箱=user@domain），
 *   同页 Set-Cookie: inbox_ctx=<domain>%2F<user>（URL 编码）选中会话。
 * - 读信同为 SSR：GET /inbox4/（轮换别名 /inbox{1..4}/ 同构）带
 *   inbox_ctx Cookie 返回该邮箱渲染页；信件列表渲染在 #email-table，
 *   每条 div.list-group-item 内三个子 div：from_div_45g45gg（From）、
 *   subj_div_45g45gg（Subject）、time_div_45g45gg（Time (UTC)）。
 * - 限制：本站邮件正文不提供纯文本/HTML 原文（默认渲染摘要），
 *   SDK 按列表三要素归一。
 *
 * 会话粘性：本端无全局 Cookie 罐，object 内以 [LinkedHashMap] 维护私有会话 Cookie，
 * 请求以显式 Cookie 头回传、响应 Set-Cookie 逐次覆写；token 保存
 * {email, domain, user} JSON 快照。
 */
object GeneratorEmail : Provider {

    private const val CHANNEL = "generator-email"
    private const val BASE_URL = "https://generator.email"
    private const val INBOX_URL = "$BASE_URL/inbox4/"

    /** 固定浏览器 UA（与 Go 端 tls-client 指纹一致形态）。 */
    private const val UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
        "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

    /** SITE_DATA 快照提取（cur_user / cur_domain）。 */
    private val USER_RE = Regex("""cur_user:"([^"]*)"""")
    private val DOMAIN_RE = Regex("""cur_domain:"([^"]*)"""")

    /** 列表条目与三要素块（g8r 前缀可变，按类名后缀锚定）。 */
    private val ITEM_RE = Regex("""(?s)<div[^>]*class="[^"]*list-group-item[^"]*"[^>]*>(.*?)(?:</div>\s*){3}""")
    private val FROM_RE = Regex("""(?s)class="[^"]*from_div_45g45gg[^"]*"[^>]*>(.*?)</div>""")
    private val SUBJ_RE = Regex("""(?s)class="[^"]*subj_div_45g45gg[^"]*"[^>]*>(.*?)</div>""")
    private val TIME_RE = Regex("""(?s)class="[^"]*time_div_45g45gg[^"]*"[^>]*>(.*?)</div>""")

    private val SCRIPT_RE = Regex("""(?is)<(script|style)[\s\S]*?</\1>""")
    private val TAG_RE = Regex("""(?s)<[^>]+>""")

    /** 会话 Cookie 私罐：键为 cookie 名（LinkedHashMap 保序，后写覆盖同名键）。 */
    private val cookieStore = LinkedHashMap<String, String>()

    /** 将私有 Cookie 罐拼为 "k=v; k2=v2" 字符串，罐空返回 null。 */
    private fun cookieHeader(): String? = if (cookieStore.isEmpty()) {
        null
    } else {
        cookieStore.entries.joinToString("; ") { "${it.key}=${it.value}" }
    }

    /** 用响应 Set-Cookie 逐次覆写私有 Cookie 罐。 */
    private fun updateCookies(resp: HttpResp) {
        for (raw in resp.setCookies) {
            val pair = raw.substringBefore(';').trim()
            val eq = pair.indexOf('=')
            if (eq > 0) cookieStore[pair.substring(0, eq).trim()] = pair.substring(eq + 1).trim()
        }
    }

    /**
     * 请求收件箱渲染页并返回 HTML。
     *
     * 附带浏览器级头部与私有会话 Cookie；邮箱由服务端依据 inbox_ctx
     * Cookie 选择。
     *
     * @return 页面 HTML
     */
    private suspend fun fetchPage(): String {
        val h = HashMap<String, String>()
        h["User-Agent"] = UA
        h["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"
        h["Accept-Language"] = "en-US,en;q=0.9"
        h["Referer"] = "$BASE_URL/"
        cookieHeader()?.let { h["Cookie"] = it }

        val resp = ProviderUtil.httpGet(INBOX_URL, h)
        updateCookies(resp)
        if (!resp.isOk) {
            throw RuntimeException("generator-email: 请求失败 http ${resp.statusCode}: ${resp.body.trim()}")
        }
        return resp.body
    }

    /** 去标签与脚本/样式块，压缩空白。 */
    private fun stripTags(s: String): String {
        val cleaned = TAG_RE.replace(SCRIPT_RE.replace(s, " "), " ")
        return cleaned.split(Regex("""\s+""")).filter { it.isNotEmpty() }.joinToString(" ")
    }

    /**
     * 创建 generator.email 临时邮箱：解析首页 SITE_DATA 快照
     * （cur_user/cur_domain）得到邮箱地址；token 保存 {email, domain, user} JSON。
     */
    override suspend fun generate(): EmailInfo {
        val src = fetchPage()
        val user = USER_RE.find(src)?.groupValues?.get(1)?.trim().orEmpty()
        val domain = DOMAIN_RE.find(src)?.groupValues?.get(1)?.trim().orEmpty()
        if (user.isEmpty() || domain.isEmpty()) {
            throw RuntimeException("generator-email: 首页未携带邮箱快照（cur_user/cur_domain）")
        }
        val email = "$user@$domain"
        val session = buildJsonObject {
            put("email", email)
            put("domain", domain)
            put("user", user)
        }.toString()
        return EmailInfo(email = email, channel = CHANNEL, token = session)
    }

    /**
     * 获取 generator.email 收件箱：解析收件箱渲染页的列表条目
     * （from/subj/time 三要素）；本站不提供原文正文，SDK 按摘要归一。
     */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val body = ProviderUtil.parse(info.token) as? JsonObject
            ?: throw RuntimeException("generator-email: 会话凭据解析失败")
        val sessDomain = strOf(body, "domain")
        if (strOf(body, "email") != info.email) {
            throw RuntimeException("generator-email: 会话邮箱与查询邮箱不匹配")
        }

        val src = fetchPage()
        // 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换
        val serverDomain = DOMAIN_RE.find(src)?.groupValues?.get(1).orEmpty()
        if (serverDomain != sessDomain) {
            throw RuntimeException("generator-email: 会话域名已切换（token $sessDomain，服务端 $serverDomain）")
        }

        // 列表区域锚定（#email-table ... #markodile 之间）
        val listStart = src.indexOf("id=\"email-table\"")
        val listEnd = src.indexOf("id=\"markodile\"")
        val region = if (listStart >= 0 && listEnd > listStart) src.substring(listStart, listEnd) else ""

        val out = ArrayList<Email>(4)
        for (m in ITEM_RE.findAll(src)) {
            val raw = m.groupValues[1]
            // 跳过列表容器外的候选：要求条目文本来自列表区域
            if (region.isNotEmpty() && !region.contains(raw)) continue
            val from = stripTags(FROM_RE.find(raw)?.groupValues?.get(1).orEmpty())
            val subject = stripTags(SUBJ_RE.find(raw)?.groupValues?.get(1).orEmpty())
            val when0 = stripTags(TIME_RE.find(raw)?.groupValues?.get(1).orEmpty())
            if (from.isEmpty() && subject.isEmpty() && when0.isEmpty()) continue
            out.add(
                Normalize.fromMap(
                    mapOf(
                        "from" to from,
                        "to" to info.email,
                        "subject" to subject,
                        "date" to when0,
                    ),
                    info.email,
                ),
            )
        }
        return out
    }

    /** 从 JsonObject 取字符串（数字/布尔自动转文本），缺失返回空串。 */
    private fun strOf(m: JsonObject, vararg keys: String): String {
        for (key in keys) {
            val v = m[key] ?: continue
            if (v is JsonNull) continue
            if (v is JsonPrimitive) return v.content.trim().ifEmpty { v.content }
            return v.toString()
        }
        return ""
    }
}