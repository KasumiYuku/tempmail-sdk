package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import java.util.Base64
import java.nio.charset.StandardCharsets

/**
 * NoxenDe5Net 渠道实现（UniMail-Bot 公共实例 tempmail.noxen.de5.net）。
 *
 * 完整接入契约：
 *   登录 POST /api/login {"username":"guest","password":"123456"}
 *     → 200 {"success":true,"role":"guest"} 并 Set-Cookie: iding-session=<JWT>；
 *   建箱 GET /api/generate → 200 {"email":"随机@域名","expires":毫秒时间戳}；
 *   读信 GET /api/emails?mailbox=<地址>&limit=20 → 200 邮件数组（无邮件为 []）；
 *   详情 GET /api/email/{id} → {..., content, html_content, to_addrs, r2_bucket,
 *     r2_object_key, download}；content/html_content 平台恒为空，原始 EML 存
 *     Cloudflare R2，download 指向 GET /api/email/{id}/download 下载端点。
 * 鉴权边界：读信不带会话 Cookie 返回 401；访客邮箱只能查自己的 mailbox。
 * 会话隔离：先 GET /api/session 兜底校验 cookie，未通过则重新 login；
 *   凭据串只由会话 Cookie 与同源地址构成，读信时逐请求显式携带。
 */
object NoxenDe5Net : Provider {

    private const val BASE_URL = "https://tempmail.noxen.de5.net"
    private const val USER = "guest"
    private const val PASS = "123456"

    /** 本渠道凭据串前缀。 */
    private const val TOKEN_PREFIX = "noxen-de5-net|"

    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /** 登录取得会话 Cookie（iding-session=JWT）。 */
    private suspend fun sessionCookie(): String {
        val body = buildJsonObject {
            put("username", USER)
            put("password", PASS)
        }.toString()
        val resp = ProviderUtil.httpPost(
            "$BASE_URL/api/login", body, "application/json", headers)
        if (!resp.isOk) throw RuntimeException("noxen-de5-net login: http ${resp.statusCode}")
        val data = ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("noxen-de5-net login: 响应非对象")
        val ok = (data["success"] as? JsonPrimitive)?.booleanOrNull ?: false
        if (!ok) throw RuntimeException("noxen-de5-net login: 登录失败")
        val session = cookieValue(resp.setCookies, "iding-session")
        if (session.isEmpty()) throw RuntimeException("noxen-de5-net login: 未下发会话 Cookie")
        return "iding-session=$session"
    }

    /** 从 Set-Cookie 列表中提取指定 cookie 的值。 */
    private fun cookieValue(setCookies: List<String>, name: String): String {
        for (sc in setCookies) {
            val kv = sc.split(";", limit = 2)[0].trim()
            if (kv.startsWith("$name=")) return kv.removePrefix("$name=")
        }
        return ""
    }

    /** 校验会话 Cookie 是否仍有效（GET /api/session）。 */
    private suspend fun cookieStillValid(cookie: String): Boolean {
        val resp = ProviderUtil.httpGet("$BASE_URL/api/session", headers + ("Cookie" to cookie))
        if (!resp.isOk) return false
        val data = ProviderUtil.parseObject(resp.body) ?: return false
        return (data["authenticated"] as? JsonPrimitive)?.booleanOrNull == true
    }

    /** 校验域名是否在平台域名池内。 */
    private suspend fun domainInPool(domain: String): Boolean {
        val resp = ProviderUtil.httpGet("$BASE_URL/api/domains", headers)
        if (!resp.isOk) return false
        val pool = ProviderUtil.parse(resp.body) as? JsonArray ?: return false
        return pool.any { it is JsonPrimitive && it.content.equals(domain, ignoreCase = true) }
    }

    /**
     * 登录并创建临时邮箱。
     * token 凭据串格式："noxen-de5-net|<iding-session=JWT>|base=<基址>"。
     */
    override suspend fun generate(): EmailInfo {
        val cookie = sessionCookie()
        val resp = ProviderUtil.httpGet("$BASE_URL/api/generate", headers + ("Cookie" to cookie))
        if (!resp.isOk) throw RuntimeException("noxen-de5-net generate: http ${resp.statusCode}")
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("noxen-de5-net generate: 响应非对象")
        val email = ProviderUtil.str(data, "email").trim()
        if (email.isEmpty()) throw RuntimeException("noxen-de5-net generate: 响应缺少 email")
        val expires = ProviderUtil.str(data, "expires").toLongOrNull() ?: 0L
        val expiresAt = if (expires > 0) {
            java.time.Instant.ofEpochMilli(expires).toString()
        } else {
            ""
        }
        val token = TOKEN_PREFIX + ProviderUtil.urlEncode(cookie) + "|base=" + BASE_URL
        return EmailInfo(email = email, channel = "noxen-de5-net", token = token, expiresAt = expiresAt)
    }

    /** 从凭据串解析会话 Cookie。 */
    private fun cookieFromToken(token: String): String {
        if (!token.startsWith(TOKEN_PREFIX)) throw RuntimeException("noxen-de5-net: token 格式错误")
        var enc = token.removePrefix(TOKEN_PREFIX)
        if (enc.endsWith("|base=$BASE_URL")) enc = enc.removeSuffix("|base=$BASE_URL")
        return java.net.URLDecoder.decode(enc, StandardCharsets.UTF_8)
    }

    /**
     * 读取收件箱。
     * 正文获取优先级：
     * 1) 详情（download 定位）+ 下载端点拉取原始 EML，本地拆分 text/plain 与 text/html；
     * 2) 详情/EML 链路不可得时，用 verification_code 置顶 + preview 合成占位正文。
     */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val addr = info.email.trim()
        var cookie = cookieFromToken(info.token)
        if (!cookieStillValid(cookie)) cookie = sessionCookie()

        val resp = ProviderUtil.httpGet(
            "$BASE_URL/api/emails?mailbox=" + ProviderUtil.urlEncode(addr) + "&limit=20",
            headers + ("Cookie" to cookie),
        )
        if (resp.statusCode == 401) {
            throw RuntimeException("noxen-de5-net 读信: http 401（会话失效或非本会话邮箱）")
        }
        if (!resp.isOk) throw RuntimeException("noxen-de5-net 读信: http ${resp.statusCode}")
        val list = ProviderUtil.parse(resp.body) as? JsonArray
            ?: throw RuntimeException("noxen-de5-net 读信: 收件箱响应非数组")

        return list.filterIsInstance<JsonObject>().map { m ->
            val flat = m.toMutableMap()
            m["sender"]?.let { flat["from"] = it }
            flat["to"] = JsonPrimitive(addr)
            m["received_at"]?.let { flat["date"] = it }
            m["preview"]?.let { flat["text"] = it }
            m["is_read"]?.let { flat["isRead"] = it }
            var full = false
            val id = ProviderUtil.str(m, "id").trim()
            if (id.isNotEmpty() && id != "0") {
                val detail = fetchDetail(cookie, id)
                if (detail != null) {
                    detail["content"]?.let { flat["content"] = it }
                    detail["html_content"]?.let { flat["html_content"] = it }
                    detail["to_addrs"]?.let { flat["to_addrs"] = it }
                    detail["r2_bucket"]?.let { flat["r2_bucket"] = it }
                    detail["r2_object_key"]?.let { flat["r2_object_key"] = it }
                    val dl = ProviderUtil.str(detail, "download").trim()
                    if (dl.isNotEmpty()) {
                        val eml = fetchEml(cookie, dl)
                        if (eml.isNotEmpty()) {
                            val (text, html) = parseEml(eml)
                            if (text.isNotEmpty() || html.isNotEmpty()) {
                                flat["text"] = JsonPrimitive(text)
                                flat["html"] = JsonPrimitive(html)
                                full = true
                            }
                        }
                    }
                }
                if (!full) flat["text"] = JsonPrimitive(composePlaceholder(m))
            }
            Normalize.fromJson(JsonObject(flat), addr)
        }
    }

    /** 拉取单封邮件详情（GET /api/email/{id}），失败返回 null。 */
    private suspend fun fetchDetail(cookie: String, id: String): JsonObject? {
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/api/email/" + ProviderUtil.urlEncode(id),
            headers + ("Cookie" to cookie),
        )
        if (!resp.isOk) return null
        return ProviderUtil.parseObject(resp.body)
    }

    /** 拉取原始 EML 报文（详情 download 字段指向的下载端点）。 */
    private suspend fun fetchEml(cookie: String, dlPath: String): String {
        val u = if (dlPath.startsWith("http://") || dlPath.startsWith("https://")) dlPath else BASE_URL + dlPath
        val resp = ProviderUtil.httpGet(
            u,
            mapOf(
                "Accept" to "message/rfc822, */*",
                "User-Agent" to headers["User-Agent"].orEmpty(),
                "Cookie" to cookie,
            ),
        )
        if (!resp.isOk) return ""
        return resp.body
    }

    /** 全文不可得时的合成占位正文：verification_code 置顶，preview 附后。 */
    private fun composePlaceholder(m: JsonObject): String {
        val code = ProviderUtil.str(m, "verification_code").trim()
        val preview = ProviderUtil.str(m, "preview").trim()
        return buildList {
            if (code.isNotEmpty()) add("验证码: $code")
            if (preview.isNotEmpty()) add(preview)
        }.joinToString("\n\n")
    }

    /** 解析 EML 原始报文 → (纯文本正文, HTML 正文)。 */
    private fun parseEml(eml: String): Pair<String, String> {
        val payload = eml.replace("\r\n", "\n").replace("\r", "")
        val (top, topBody) = splitEml(payload, 0)
        return parseEntity(top, topBody)
    }

    /** 将原始报文切分为首部 map 与正文块。 */
    private fun splitEml(payload: String, offset: Int): Pair<Map<String, String>, String> {
        val lines = payload.split("\n")
        val headers = mutableMapOf<String, String>()
        var i = offset
        if (i < lines.size && lines[i].startsWith("From ")) i++
        var curKey = ""
        while (i < lines.size) {
            val line = lines[i]
            if (line.isEmpty()) {
                i++
                break
            }
            // 折行续行（RFC 5322）：以空白开头且已有当前头键时拼接到上一条
            if ((line.startsWith(" ") || line.startsWith("\t")) && curKey.isNotEmpty()) {
                headers[curKey] = "${headers[curKey]} ${line.trim()}"
                i++
                continue
            }
            val k = line.indexOf(':')
            if (k > 0) {
                curKey = line.substring(0, k).trim().lowercase()
                headers[curKey] = line.substring(k + 1).trim()
            }
            i++
        }
        val body = lines.drop(i).joinToString("\n")
        return headers to body
    }

    /** 从 Content-Type 头值提取 multipart boundary（引号可选）。 */
    private fun extractBoundary(ctRaw: String): String {
        return Regex("(?i)boundary=\"?([^\";\\s]+)\"?").find(ctRaw)?.groupValues?.get(1) ?: ""
    }

    /** 按 boundary 切出各 part（含各自首部行）。 */
    private fun splitMultipart(body: String, boundary: String): List<String> {
        return body.split("--$boundary").mapNotNull { seg0 ->
            var seg = seg0
            if (seg.startsWith("\n")) seg = seg.substring(1)
            if (seg.endsWith("--\n")) seg = seg.dropLast(3)
            if (seg.endsWith("--")) seg = seg.dropLast(2)
            if (seg.trim().isEmpty()) null else seg
        }
    }

    /** 递归解析单个 MIME 实体（上游 parseEntity 同构）。 */
    private fun parseEntity(headers: Map<String, String>, body: String): Pair<String, String> {
        val ct = headers["content-type"].orEmpty().lowercase()
        val cte = headers["content-transfer-encoding"].orEmpty().lowercase()

        // 单体：text/html 或 text/plain（含无 Content-Type 时按纯文本处理）
        if (!ct.startsWith("multipart/")) {
            val decoded = decodePart(body, cte)
            return if (ct.contains("text/html")) "" to decoded else decoded to ""
        }

        // 复合：递归拆分，text 槽与 html 槽各自取第一个非空命中
        var text = ""
        var html = ""
        val boundary = extractBoundary(headers["content-type"].orEmpty())
        if (boundary.isNotEmpty()) {
            for (part in splitMultipart(body, boundary)) {
                val (ph, pb) = splitEml("#participant\n$part", 1)
                val pct = ph["content-type"].orEmpty().lowercase()
                when {
                    pct.startsWith("multipart/") -> {
                        val (t, h) = parseEntity(ph, pb)
                        if (text.isEmpty()) text = t
                        if (html.isEmpty()) html = h
                    }
                    pct.startsWith("message/rfc822") -> {
                        val (nh, nb) = splitEml(pb, 0)
                        val (t, h) = parseEntity(nh, nb)
                        if (text.isEmpty()) text = t
                        if (html.isEmpty()) html = h
                    }
                    pct.contains("rfc822-headers") -> {
                        // 纯头部 part 跳过，正文在后续 part 中抓取
                        continue
                    }
                    else -> {
                        val (t, h) = parseEntity(ph, pb)
                        if (text.isEmpty()) text = t
                        if (html.isEmpty()) html = h
                    }
                }
                if (text.isNotEmpty() && html.isNotEmpty()) break
            }
        }
        // 无 HTML 命中时从整体原文兜底抓取 HTML 片段（上游 guessHtmlFromRaw 同构）
        if (html.isEmpty()) html = guessHtml(body)
        return text to html
    }

    /** 按 Content-Transfer-Encoding 解码 part 内容。 */
    private fun decodePart(data: String, cte: String): String {
        return when (cte.trim().lowercase()) {
            "base64" -> {
                val joined = data.replace(Regex("[\n\r\t ]"), "")
                try {
                    String(Base64.getDecoder().decode(joined), Charsets.UTF_8).trim()
                } catch (_: IllegalArgumentException) {
                    data
                }
            }
            "quoted-printable" -> decodeQuotedPrintable(data)
            else -> data.trim() // 7bit/8bit/binary：原样返回
        }
    }

    /** 最小 quoted-printable 解码（=XX 十六进制转字节，行尾 = 软换行）。 */
    private fun decodeQuotedPrintable(data: String): String {
        var out = data.replace(Regex("=\r?\n"), "")
        out = Regex("=([0-9A-Fa-f]{2})").replace(out) { mr ->
            mr.groupValues[1].toInt(16).toChar().toString()
        }
        return out.trim()
    }

    /** 从整体原文中抓取 <html>…</html> 片段（上游 guessHtmlFromRaw 同构）。 */
    private fun guessHtml(body: String): String {
        if (body.isEmpty()) return ""
        val lower = body.lowercase()
        var hs = lower.indexOf("<html")
        if (hs < 0) hs = lower.indexOf("<!doctype html")
        if (hs < 0) return ""
        val he = lower.lastIndexOf("</html>")
        if (he < 0 || he < hs) return ""
        return body.substring(hs, he + 7)
    }
}