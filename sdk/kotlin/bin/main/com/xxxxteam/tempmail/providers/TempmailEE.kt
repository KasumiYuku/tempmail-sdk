package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import java.time.Instant
import java.util.Base64

/**
 * TempmailEE 渠道实现（tempmail.ee）。
 *
 * 出口风控结论（2026-09-27 实测）：读信 /api/mails 的 403 是会话 Cookie 绑定校验——
 * change（换箱）响应 Set-Cookie 中的 temp_mail_session 与 temp_email 共同构成读信凭据，
 * 带齐 temp_email + temp_mail_session 即 200。建箱与读信均以显式 Cookie 头逐请求携带凭据。
 * POST /api/mailbox/change 必须带 sec-ch-ua 三件套（否则 403 Browser request required），
 * UA 使用固定 Chrome 154（与其 sec-ch-ua 品牌版本一致）。
 */
object TempmailEE : Provider {

    private const val CHANNEL = "tempmail-ee"
    private const val BASE_URL = "https://tempmail.ee"

    /** 与 sec-ch-ua 品牌版本一致的固定 UA（Chrome 154 / Linux）。 */
    private const val UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
        "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

    /** Token 前缀，用于识别本渠道会话凭据串。 */
    private const val TOKEN_PREFIX = "tempmail-ee|"

    private val SEC_CH_UA = "\"Chromium\";v=\"154\", \"Google Chrome\";v=\"154\", \"Not.A/Brand\";v=\"99\""

    private val BASE_HEADERS = mapOf(
        "Accept" to "application/json",
        "Content-Type" to "application/json",
        "X-Requested-With" to "XMLHttpRequest",
        "Origin" to BASE_URL,
        "Referer" to "$BASE_URL/",
        "Sec-Fetch-Site" to "same-origin",
        "Sec-Fetch-Mode" to "cors",
        "Sec-Fetch-Dest" to "empty",
        "Accept-Language" to "en-US,en;q=0.9",
        "User-Agent" to UA,
    )

    private val CH_HEADERS = mapOf(
        "Sec-Ch-Ua" to SEC_CH_UA,
        "Sec-Ch-Ua-Mobile" to "?0",
        "Sec-Ch-Ua-Platform" to "\"Linux\"",
    )

    /**
     * 浏览器特征请求头：change 必须携带 sec-ch-ua 三件套。
     *
     * @param withSecCH 是否携带 sec-ch-ua 三件套
     * @param cookie 显式 Cookie 请求头（会话凭据串，空则不携带）
     */
    private fun browserHeaders(withSecCH: Boolean, cookie: String = ""): Map<String, String> {
        val h = BASE_HEADERS.toMutableMap()
        if (withSecCH) h.putAll(CH_HEADERS)
        if (cookie.isNotEmpty()) h["Cookie"] = cookie
        return h
    }

    /** 建箱提交的浏览器指纹（与官方前端一致）。 */
    private fun browserIntegrity(): JsonObject = buildJsonObject {
        put("webdriver", false)
        put("languagesMissing", false)
        put("languageMissing", false)
        put("pluginsMissing", false)
        put("pluginsUndefined", false)
        put("outerSizeMissing", false)
        put("innerSizeMissing", false)
        put("screenMissing", false)
        put("screenDepthMissing", false)
        put("timezoneMissing", false)
        put("timezoneOffsetMissing", false)
        put("userAgentDataPresent", true)
        put("userAgentMissing", false)
        put("platformClass", "Linux")
        put("mobile", false)
        put("collectionFailed", false)
    }

    /** 从 change 响应提取会话 Cookie 键值对（手动接管会话）。 */
    private fun extractCookies(setCookies: List<String>): Pair<String, String> {
        var email = ""
        var session = ""
        for (raw in setCookies) {
            val kv = raw.substringBefore(';').trim()
            when {
                kv.startsWith("temp_email=") -> email = kv.substringAfter('=')
                kv.startsWith("temp_mail_session=") -> session = kv.substringAfter('=')
            }
        }
        return email to session
    }

    /** 解析读信凭据：返回 "temp_email=..; temp_mail_session=.."，无效返回空串。 */
    private fun parseToken(token: String, email: String): String {
        if (!token.startsWith(TOKEN_PREFIX)) return ""
        val cred = token.substring(TOKEN_PREFIX.length)
        var session = ""
        for (part in cred.split(";")) {
            val kv = part.trim()
            if (kv.startsWith("temp_mail_session=")) session = kv.substringAfter('=')
        }
        if (session.isEmpty()) return ""
        // 会话绑定邮箱：以请求邮箱为准重拼 cookie，防止凭据与邮箱错配
        return "temp_email=$email; temp_mail_session=$session"
    }

    /** 创建临时邮箱：GET / 面熟 → POST /api/mailbox/change 换新邮箱并提取会话 Cookie。 */
    override suspend fun generate(): EmailInfo {
        // 步骤 1：GET / 建立会话（面熟首访）
        ProviderUtil.httpGet(
            BASE_URL,
            mapOf(
                "User-Agent" to UA,
                "Accept" to "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            ),
        )

        // 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua）
        val payload = buildJsonObject {
            put("turnstileToken", JsonNull)
            put("browserIntegrity", browserIntegrity())
        }.toString()
        val resp = ProviderUtil.httpPost("$BASE_URL/api/mailbox/change", payload, "application/json", browserHeaders(true))
        resp.ensureSuccess()
        val chg = ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("tempmail-ee: 建箱响应无效")
        val ok = chg["success"] as? JsonPrimitive
        var email = ProviderUtil.str(chg, "newEmail").trim()
        if (ok?.booleanOrNull != true || email.isEmpty()) {
            throw RuntimeException("tempmail-ee: 建箱失败（status ${resp.statusCode}）")
        }

        // 步骤 3：接管会话凭据，供读信校验使用
        val (cookieEmail, session) = extractCookies(resp.setCookies)
        if (cookieEmail.isNotEmpty() && cookieEmail != email) email = cookieEmail
        if (session.isEmpty()) {
            throw RuntimeException("tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信")
        }
        val expires = ProviderUtil.str(chg, "expiresAt")
        return EmailInfo(
            email = email,
            channel = CHANNEL,
            token = TOKEN_PREFIX + "temp_email=$email; temp_mail_session=$session",
            expiresAt = expires,
        )
    }

    /** 读取收件箱：列表仅元数据，逐封 GET /api/mails/{id} 解析 MIME content。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val email = info.email.trim()
        if (email.isEmpty()) throw RuntimeException("tempmail-ee: 邮箱为空")
        if (info.token.isBlank()) throw RuntimeException("tempmail-ee: token 为空")
        val cookie = parseToken(info.token.trim(), email)
        if (cookie.isEmpty()) {
            throw RuntimeException("tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱")
        }

        val payload = buildJsonObject { put("email", email) }.toString()
        val resp = ProviderUtil.httpPost("$BASE_URL/api/mails", payload, "application/json", browserHeaders(true, cookie))
        if (resp.statusCode == 403) {
            throw RuntimeException("tempmail-ee: http 403（会话 Cookie 校验失败，邮箱可能已过期，请重新 Generate）")
        }
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val mails = ProviderUtil.arr(data, "mails") ?: return emptyList()

        val out = ArrayList<Email>()
        for (m in mails.filterIsInstance<JsonObject>()) {
            val id = strOf(m, "id").trim()
            if (id.isEmpty()) continue
            var to = strOf(m, "toAddress", "to")
            if (to.isEmpty()) to = email
            val date = strOf(m, "createdAt", "date", "receivedAt").ifEmpty { Instant.now().toString() }
            var from = strOf(m, "fromAddress", "from", "sender")
            var subject = strOf(m, "subject")
            // 详情拉取失败不阻塞列表其余邮件（与 Go 端一致：失败即跳过该封）
            val detail = fetchDetail(cookie, id) ?: continue
            if (from.isEmpty()) from = detail.from
            if (subject.isEmpty()) subject = detail.subject
            val body = if (detail.text.isNotEmpty()) detail.text else detail.html
            out.add(Email(id = id, from = from, to = to, subject = subject, body = body, date = date))
        }
        return out
    }

    /**
     * 拉取单封详情并解析 content。
     *
     * @return (text, html, from, subject)；HTTP/JSON 失败或 content 缺失返回 null
     */
    private suspend fun fetchDetail(cookie: String, id: String): Quad? {
        val resp = ProviderUtil.httpGet("$BASE_URL/api/mails/$id", browserHeaders(false, cookie))
        if (!resp.isOk) return null
        val detail = ProviderUtil.parseObject(resp.body) ?: return null
        var content = strOf(detail, "content")
        if (content.isEmpty()) {
            // content 缺失时回退兜底候选键（实测老版前端仅用 content）
            content = strOf(detail, "text", "body", "html")
            if (content.isEmpty()) return null
        }
        val (text, html) = parseContent(content)
        return Quad(text, html, strOf(detail, "fromAddress", "from"), strOf(detail, "subject"))
    }

    /** 四元组（text, html, from, subject）。 */
    private data class Quad(val text: String, val html: String, val from: String, val subject: String)

    // ==================== MIME content 解析 ====================

    private val SCRIPT_RE = Regex("(?is)<script[\\s\\S]*?</script>")
    private val TAG_RE = Regex("(?s)<[^>]+>")
    private val NUM_ENT_RE = Regex("&#((?:x[0-9a-fA-F]+)|(?:[0-9]+));")
    private val NAMED_ENTS = mapOf(
        "&lt;" to "<", "&gt;" to ">", "&amp;" to "&", "&quot;" to "\"",
        "&apos;" to "'", "&#39;" to "'", "&nbsp;" to " ",
    )

    /** HTML 实体反转义：先后数字实体、后命名实体。 */
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

    /** 将 HTML 转为纯文本（去 script/标签、反转义、压缩空白）。 */
    private fun htmlToText(src: String): String {
        val cleaned = TAG_RE.replace(SCRIPT_RE.replace(src, " "), " ")
        return htmlUnescape(cleaned).split(Regex("\\s+")).filter { it.isNotEmpty() }.joinToString(" ")
    }

    /**
     * 解析详情 content：
     * 1) 平台已做 HTML 实体转义，先反转义；
     * 2) content 是 MIME multipart 原文（含顶部边界装饰头），按首个边界行切块，
     *    各 part 依据 Content-Transfer-Encoding 做 base64 / quoted-printable 解码，
     *    text part 入文本、html part 入 HTML。
     */
    private fun parseContent(raw: String): Pair<String, String> {
        var payload = htmlUnescape(raw)
        payload = payload.replace("\r\n", "\n")
        val lines = payload.split("\n")
        val boundary = boundaryOf(lines)
        var text = ""
        var html = ""
        if (boundary.isNotEmpty()) {
            val (t, h) = parseParts(lines, boundary)
            text = t
            html = h
        }
        if (boundary.isEmpty() || (text.isEmpty() && html.isEmpty())) {
            // 无有效 multipart 结构：整个 content 去壳后作为正文
            val (t, h) = parseSingleton(payload)
            text = t
            html = h
        }
        if (text.isEmpty() && html.isNotEmpty()) text = htmlToText(html)
        if (html.isEmpty() && text.isNotEmpty()) {
            html = "<html><body><pre>" + htmlEscapeMin(text) + "</pre></body></html>"
        }
        return text to html
    }

    /** 最小 HTML 转义（仅 & < >）。 */
    private fun htmlEscapeMin(s: String): String =
        s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")

    /**
     * 单 part（无边界或拆不出内容）降级解析：
     * 依次按「头部区隔（首个空行之后）→ 整体」取内容并解码。
     */
    private fun parseSingleton(payload: String): Pair<String, String> {
        var body = payload.trim()
        if (!body.contains("\n")) return body to ""
        val lines = payload.split("\n")
        for (i in lines.indices) {
            val ln = lines[i]
            if (ln.trim().isEmpty()) {
                body = lines.drop(i + 1).joinToString("\n")
                break
            }
            if (i > 40 || (i >= 3 && !ln.contains(":"))) break
        }
        val lower = payload.lowercase()
        val cte = listOf("base64", "quoted-printable").firstOrNull { lower.contains(it) } ?: ""
        return decodePart(body, cte) to ""
    }

    /** 扫描前 120 行寻找边界行（--xxx）。 */
    private fun boundaryOf(lines: List<String>): String {
        for (i in lines.indices.take(120)) {
            val ln = lines[i]
            if (ln.startsWith("--") && ln.length > 2) {
                return ln.removePrefix("--").removeSuffix("\r")
            }
        }
        return ""
    }

    /** 按 boundary 拆分 multipart 并解码归并 text/html 两个 part。 */
    private fun parseParts(lines: List<String>, boundary: String): Pair<String, String> {
        var text = ""
        var html = ""
        var i = 0
        while (i < lines.size) {
            if (!lines[i].startsWith("--$boundary")) {
                i++
                continue
            }
            if (lines[i].startsWith("--$boundary--")) break
            val headers = mutableMapOf<String, String>()
            var j = i + 1
            // part 头部：直到首个空行
            while (j < lines.size && lines[j].isNotEmpty() && !lines[j].startsWith("--$boundary")) {
                val ln = lines[j]
                val k = ln.indexOf(':')
                if (k > 0) headers[ln.substring(0, k).trim().lowercase()] = ln.substring(k + 1).trim()
                j++
            }
            if (j < lines.size && lines[j].isEmpty()) j++
            // part 正文：到下一个边界行为止
            val body = mutableListOf<String>()
            while (j < lines.size && !lines[j].startsWith("--$boundary")) {
                body.add(lines[j])
                j++
            }
            val (t, h) = mergePart(body, headers, text, html)
            text = t
            html = h
            i = j
        }
        return text to html
    }

    /** 单个 part 的解码归并：按 content-type 归类，缺省回退 text 槽。 */
    private fun mergePart(
        body: List<String>,
        headers: Map<String, String>,
        text: String,
        html: String,
    ): Pair<String, String> {
        var ct = headers["content-type"] ?: ""
        ct = ct.substringBefore(';')
        val cte = headers["content-transfer-encoding"] ?: ""
        val content = body.joinToString("\n")
        return when {
            ct.contains("text/plain") -> Pair(if (text.isEmpty()) decodePart(content, cte) else text, html)
            ct.contains("text/html") -> Pair(text, if (html.isEmpty()) decodePart(content, cte) else html)
            else -> Pair(if (text.isEmpty()) decodePart(content, cte) else text, html)
        }
    }

    /** 按 Content-Transfer-Encoding 解码 part 内容。 */
    private fun decodePart(data0: String, cte: String): String {
        val data = data0.trim()
        return when (cte.lowercase()) {
            "base64" -> {
                val joined = data.filterNot { it == '\n' || it == '\r' || it == '\t' || it == ' ' }
                try {
                    String(Base64.getDecoder().decode(joined), Charsets.UTF_8).trim()
                } catch (_: Exception) {
                    data
                }
            }
            "quoted-printable" -> qpDecode(data)
            else -> data
        }
    }

    /** quoted-printable 解码（含软换行容忍，等价 Go quotedprintable.Reader）。 */
    private fun qpDecode(data: String): String {
        val out = StringBuilder()
        var i = 0
        while (i < data.length) {
            val c = data[i]
            when {
                c == '=' -> {
                    if (i + 2 < data.length) {
                        val hex = data.substring(i + 1, i + 3)
                        val v = hex.toIntOrNull(16)
                        if (v != null) {
                            out.append(v.toChar())
                            i += 3
                            continue
                        }
                    }
                    out.append(c)
                }
                c == '_' -> {
                    if (out.isNotEmpty() && out.last() != ' ' && data.getOrNull(i - 1) != '=') out.append(' ')
                }
                else -> out.append(c)
            }
            i++
        }
        return out.toString()
    }

    /** 按候选键顺序从 JSON 对象提取字符串值（数字转十进制字符串）。 */
    private fun strOf(m: JsonObject, vararg keys: String): String {
        for (key in keys) {
            val v = m[key] ?: continue
            when (v) {
                is JsonNull -> continue
                is JsonPrimitive -> return v.content.trim().ifEmpty { v.content }
                else -> return v.toString()
            }
        }
        return ""
    }
}