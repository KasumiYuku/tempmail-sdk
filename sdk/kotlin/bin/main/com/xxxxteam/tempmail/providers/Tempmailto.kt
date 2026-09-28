package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import java.time.Instant

/**
 * Tempmailto 渠道实现（tempmailto.com，Laravel Cookie 会话）。
 *
 * 与 Go 端 tempmailto.go 协议一致（2026-09-27 抓包实证）：
 * - GET 首页由服务端渲染当前邮箱（#mainEmail value），同会话内不变，建箱只需 GET + 提取；
 * - POST /get_messages（表单 _token + captcha 留空）返回 {status, mailbox, email_token, messages, histories}；
 * - 详情页为站内 GET /view/{id}。
 *
 * 会话粘性：本端无全局 Cookie 罐，object 内以 [LinkedHashMap] 维护私有会话 Cookie，
 * 请求以显式 Cookie 头回传、响应 Set-Cookie 逐次覆写；邮箱由会话 Cookie 承载，
 * 读信当前邮箱与请求邮箱不一致时用 /change 以请求邮箱名拉回。
 * 邮箱约 10 分钟无活动过期。
 */
object Tempmailto : Provider {

    private const val CHANNEL = "tempmailto"
    private const val BASE_URL = "https://tempmailto.com"

    /** 固定浏览器 UA（与 Go 端 tls-client 指纹一致形态）。 */
    private const val UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
        "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

    /** 会话 Cookie 私罐：键为 cookie 名（LinkedHashMap 保序，后写覆盖同名键）。 */
    private val cookieStore = LinkedHashMap<String, String>()

    /** #mainEmail 服务端渲染邮箱正则。 */
    private val MAIN_EMAIL_RE = Regex("""(?is)id="mainEmail"[^>]*\bvalue="([^"]+)"""")

    /**
     * 组装首页 GET 的同站浏览器特征头。
     *
     * @param withCookie 是否携带当前私有会话 Cookie
     * @return 请求头映射
     */
    private fun browserHeaders(withCookie: Boolean): Map<String, String> {
        val h = HashMap<String, String>()
        h["User-Agent"] = UA
        h["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"
        h["Accept-Language"] = "en-US,en;q=0.9"
        h["Origin"] = BASE_URL
        h["Referer"] = "$BASE_URL/"
        if (withCookie) cookieHeader()?.let { h["Cookie"] = it }
        return h
    }

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
     * 拉取首页 HTML 并提取 CSRF token（读信/换箱共用），同时推进会话 Cookie。
     *
     * @return CSRF token；首页缺失 csrf-token 抛异常
     */
    private suspend fun fetchCsrf(): String {
        val resp = ProviderUtil.httpGet(BASE_URL, browserHeaders(true))
        updateCookies(resp)
        resp.ensureSuccess()
        val csrf = CSRF_RE.find(resp.body)?.groupValues?.get(1) ?: ""
        if (csrf.isEmpty()) throw RuntimeException("tempmailto: 首页未找到 csrf-token")
        return csrf
    }

    /** csrf-token meta 标签正则。 */
    private val CSRF_RE = Regex("""<meta\s+name="csrf-token"\s+content="([^"]+)"""")
    /** 本地部分正则（@ 前部）。 */
    private val LOCAL_PART_RE = Regex("""^[^@]+""")

    /** 创建 tempmailto.com 临时邮箱：GET 首页提取服务端渲染邮箱（10 分钟无活动过期）。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpGet(BASE_URL, browserHeaders(false))
        updateCookies(resp)
        resp.ensureSuccess()
        val email = MAIN_EMAIL_RE.find(resp.body)?.groupValues?.get(1)?.trim().orEmpty()
        if (email.isEmpty()) {
            throw RuntimeException("tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱")
        }
        return EmailInfo(email = email, channel = CHANNEL, token = email)
    }

    /** 获取 tempmailto.com 当前邮箱的收件列表。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val email = info.email.trim()
        if (email.isEmpty()) throw RuntimeException("tempmailto: 邮箱为空，请重新 Generate")

        var data = fetchMessages()

        // 会话当前邮箱与请求目标不一致 -> change 拉回（change 内置重新取 CSRF）
        val mailbox = ProviderUtil.str(data, "mailbox").trim()
        if (mailbox.isNotEmpty() && !mailbox.equals(email, ignoreCase = true)) {
            val changed = change(email)
            if (!changed.equals(email, ignoreCase = true)) {
                throw RuntimeException("tempmailto: 会话邮箱无法拉回请求邮箱")
            }
            data = fetchMessages()
        }

        val messages = ProviderUtil.arr(data, "messages") ?: return emptyList()
        val out = ArrayList<Email>(messages.size)
        for (m in messages.filterIsInstance<JsonObject>()) {
            val id = strOf(m, "id").trim()
            if (id.isEmpty()) continue
            val fromEmail = strOf(m, "from_email", "from")
            val from = strOf(m, "from_name").ifEmpty { fromEmail }
            val date = strOf(m, "receivedAt", "received_at", "createdAt").ifEmpty { Instant.now().toString() }
            val seen = seenOf(m["is_seen"])

            val html = viewDetail(id)
            var text = htmlToText(html)
            if (text.isEmpty()) {
                text = strOf(m, "body", "text", "snippet", "preview").ifEmpty { strOf(m, "subject") }
            }
            out.add(
                Normalize.fromMap(
                    mapOf(
                        "id" to id,
                        "from" to from,
                        "to" to email,
                        "subject" to strOf(m, "subject"),
                        "body" to text,
                        "html" to html.ifEmpty {
                            "<html><body><pre>" + htmlEscapeMin(text) + "</pre></body></html>"
                        },
                        "timestamp" to date,
                        "isRead" to seen,
                    ),
                    email,
                ),
            )
        }
        return out
    }

    /** POST /get_messages 读取收件箱（表单 _token + captcha 留空）。 */
    private suspend fun fetchMessages(): JsonObject {
        val csrf = fetchCsrf()
        val body = "_token=${ProviderUtil.urlEncode(csrf)}&captcha="
        val h = ajaxHeaders()
        val resp = ProviderUtil.httpPost(
            "$BASE_URL/get_messages", body, "application/x-www-form-urlencoded; charset=UTF-8", h)
        updateCookies(resp)
        resp.ensureSuccess()
        return ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("tempmailto: 解析读信响应失败")
    }

    /** POST /change 换箱并返回变更后的当前邮箱。 */
    private suspend fun change(email: String): String {
        val csrf = fetchCsrf()
        var name = LOCAL_PART_RE.find(email)?.value.orEmpty()
        if (name.isEmpty()) name = "TmSdk"
        var domain = "tempmailto.com"
        val at = email.lastIndexOf('@')
        if (at >= 0 && at + 1 < email.length) domain = email.substring(at + 1)

        val body = "_token=${ProviderUtil.urlEncode(csrf)}" +
            "&name=${ProviderUtil.urlEncode(name)}&domain=${ProviderUtil.urlEncode(domain)}"
        val resp = ProviderUtil.httpPost(
            "$BASE_URL/change", body, "application/x-www-form-urlencoded; charset=UTF-8", ajaxHeaders())
        updateCookies(resp)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("tempmailto: 解析 change 响应失败")
        val mailbox = ProviderUtil.str(data, "mailbox").trim()
        if (mailbox.isEmpty()) throw RuntimeException("tempmailto: change 响应异常")
        return mailbox
    }

    /** 组装 AJAX 表单 POST 的请求头（同站 fetch 全套）。 */
    private fun ajaxHeaders(): Map<String, String> {
        val h = HashMap<String, String>()
        h["User-Agent"] = UA
        h["Accept"] = "application/json, text/plain, */*"
        h["Accept-Language"] = "en-US,en;q=0.9"
        h["Content-Type"] = "application/x-www-form-urlencoded; charset=UTF-8"
        h["X-Requested-With"] = "XMLHttpRequest"
        h["Origin"] = BASE_URL
        h["Referer"] = "$BASE_URL/"
        cookieHeader()?.let { h["Cookie"] = it }
        return h
    }

    /**
     * GET /view/{id} 提取邮件正文 HTML（同会话 Cookie）。
     * 按候选 class 依次匹配 div/section/article，回退 <main>/<article>；失败返回空串。
     */
    private suspend fun viewDetail(id: String): String {
        val resp = ProviderUtil.httpGet("$BASE_URL/view/$id", browserHeaders(true))
        updateCookies(resp)
        if (!resp.isOk) return ""
        val page = resp.body
        for (cls in DETAIL_CLASSES) {
            val re = Regex("""(?is)<[^>]+class="[^"]*\b""" + Regex.escape(cls) +
                """\b[^"]*"[^>]*>([\s\S]*?)</(?:div|section|article)>""")
            val m = re.find(page)?.groupValues?.get(1)?.trim().orEmpty()
            if (m.isNotEmpty()) return m
        }
        val fallback = Regex("""(?is)<(main|article)[^>]*>([\s\S]*?)</\1>""")
        return fallback.find(page)?.groupValues?.get(2)?.trim().orEmpty()
    }

    /** 详情正文容器的候选 class（按平台实际结构依次尝试）。 */
    private val DETAIL_CLASSES = arrayOf(
        "mail-body", "mail_content", "email-body", "content-body", "message-content", "mail-content")

    // ==================== HTML 解析工具 ====================

    private val SCRIPT_RE = Regex("""(?is)<(script|style)[\s\S]*?</\1>""")
    private val TAG_RE = Regex("""(?s)<[^>]+>""")
    private val NUM_ENT_RE = Regex("""&#((?:x[0-9a-fA-F]+)|(?:[0-9]+));""")
    private val NAMED_ENTS = mapOf(
        "&lt;" to "<", "&gt;" to ">", "&amp;" to "&", "&quot;" to "\"",
        "&apos;" to "'", "&#39;" to "'", "&nbsp;" to " ",
    )

    /** HTML 实体反转义：先数字实体、后命名实体。 */
    private fun htmlUnescape(s: String): String {
        var out = NUM_ENT_RE.replace(s) { m ->
            val body = m.groupValues[1]
            val cp = if (body.startsWith("x") || body.startsWith("X")) {
                body.substring(1).toIntOrNull(16)
            } else {
                body.toIntOrNull()
            }
            if (cp != null && cp in 0..0x10FFFF) String(Character.toChars(cp)) else m.value
        }
        for ((k, v) in NAMED_ENTS) out = out.replace(k, v)
        return out
    }

    /** HTML 转纯文本（去 script/style/标签、反转义、压缩空白）。 */
    private fun htmlToText(src: String): String {
        val cleaned = TAG_RE.replace(SCRIPT_RE.replace(src, " "), " ")
        return htmlUnescape(cleaned).split(Regex("""\s+""")).filter { it.isNotEmpty() }.joinToString(" ")
    }

    /** 最小 HTML 转义（仅 & < >）。 */
    private fun htmlEscapeMin(s: String): String =
        s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")

    /** 按候选键顺序从 JSON 对象提取字符串值（数字转十进制字符串）。 */
    private fun strOf(m: JsonObject, vararg keys: String): String {
        for (key in keys) {
            val v = m[key] ?: continue
            if (v is JsonNull) continue
            if (v is JsonPrimitive) return v.content.trim().ifEmpty { v.content }
            return v.toString()
        }
        return ""
    }

    /** is_seen 三态归一为布尔：booleanOrNull / 数值非 0 / 字符串 "1"|"true" 忽略大小写。 */
    private fun seenOf(v: JsonElement?): Boolean {
        val p = v as? JsonPrimitive ?: return false
        p.booleanOrNull?.let { return it }
        if (p.content == "1" || p.content.equals("true", ignoreCase = true)) return true
        return p.content.toDoubleOrNull()?.let { it != 0.0 } ?: false
    }
}