package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Temporarymail 渠道实现（temporarymail.com）。
 *
 * 无认证 REST（key 为空即随机建箱）：
 *   GET /api/?action=requestEmailAccess&key=&value=random 建箱，
 *   响应 {"address":"...","secretKey":"..."}，secretKey 用于后续 checkInbox。
 * 读信 GET /api/?action=checkInbox&value=<secretKey>，响应有两种形态：
 *   空收件箱为 []，有信时为 map[id]→邮件元数据对象。
 * 详情 POST /api/?action=getEmail&value=<id> 覆盖真实主题（列表常为 "[No Subject]"）；
 * 全文 GET /view/?i=<id> 返回 HTML 化网页，本地剥标签还原纯文本。
 * 地址最长周期固定为 4 小时。
 */
object TemporarymailCom : Provider {

    private const val CHANNEL = "temporarymail-com"
    private const val BASE_URL = "https://temporarymail.com"

    /** 备用浏览器 UA（403 重试用，规避共享池随机 UA 耗尽）。 */
    private const val ALT_UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
        "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

    private const val UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
        "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

    private val HEADERS = mapOf(
        "Accept" to "application/json, text/plain, */*",
        "Accept-Language" to "en-US,en;q=0.9",
        "Sec-Fetch-Site" to "same-origin",
        "Sec-Fetch-Mode" to "cors",
        "Sec-Fetch-Dest" to "empty",
        "Referer" to "$BASE_URL/",
        "Origin" to BASE_URL,
    )

    /** 构造 /api/ 请求头（403 时换备用 UA 重试）。 */
    private fun apiHeaders(ua: String): Map<String, String> = HEADERS + ("User-Agent" to ua)

    /** 创建 temporarymail.com 临时邮箱；token 复用 secretKey。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/api/?action=requestEmailAccess&key=&value=random", apiHeaders(UA))
        if (resp.statusCode == 429) {
            throw RuntimeException("temporarymail: 创建邮箱平台限流(429)，请稍后重试")
        }
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("temporarymail: 创建响应非对象")
        val addr = ProviderUtil.str(data, "address").trim()
        val key = ProviderUtil.str(data, "secretKey").trim()
        if (addr.isEmpty() || key.isEmpty()) {
            throw RuntimeException("temporarymail: 创建响应缺少 address 或 secretKey")
        }
        return EmailInfo(email = addr, channel = CHANNEL, token = key)
    }

    /**
     * 读取 temporarymail 收件箱。
     * 列表主题常为 "[No Subject]"：逐封拉详情覆盖真实主题，并逐封抓 /view/ 全文。
     */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val tk = info.token.trim()
        if (tk.isEmpty()) throw RuntimeException("temporarymail: token 为空")
        val addr = info.email.trim()

        val body = checkInbox(tk)
        val list = inboxListOf(body) ?: return emptyList()
        return list.mapNotNull { m ->
            if (m !is JsonObject) return@mapNotNull null
            val row = m.toMutableMap()
            if (!row.containsKey("to")) row["to"] = JsonPrimitive(addr)
            val id = ProviderUtil.str(m, "id").trim()
            if (id.isNotEmpty()) {
                // 详情覆盖真实主题/发件人（失败不致命：列表元数据兜底）
                fetchDetail(id)?.let { det ->
                    if (ProviderUtil.str(det, "subject").isNotBlank()) {
                        det["subject"]?.let { row["subject"] = it }
                    }
                    if (ProviderUtil.str(det, "from").isNotBlank()) {
                        det["from"]?.let { row["from"] = it }
                    }
                }
                // /view/ 渲染端点全文（失败不致命：列表元数据兜底）
                val text = fetchView(id)
                if (text.isNotEmpty()) row["text"] = JsonPrimitive(text)
            }
            Normalize.fromJson(JsonObject(row), addr)
        }
    }

    /** 解析平台双形态响应（[] 或 map[id]→对象）为元素列表。 */
    private fun inboxListOf(body: String): List<JsonElement>? {
        return when (val el = ProviderUtil.parse(body) ?: return null) {
            is JsonArray -> el.toList()
            is JsonObject -> el.values.toList()
            else -> null
        }
    }

    /** 拉取 checkInbox 响应体；403 换备用 UA 重试一次，429 报平台限流。 */
    private suspend fun checkInbox(token: String): String {
        var lastStatus = 0
        for (ua in listOf(UA, ALT_UA)) {
            val resp = ProviderUtil.httpGet(
                "$BASE_URL/api/?action=checkInbox&value=" + ProviderUtil.urlEncode(token),
                apiHeaders(ua),
            )
            if (resp.statusCode == 429) {
                throw RuntimeException("temporarymail: 读取收件箱平台限流(429)，请拉大轮询间隔")
            }
            if (resp.isOk) return resp.body
            lastStatus = resp.statusCode
            // 403/404 疑似 UA 键控风控，换备用 UA 重试一次
            if (resp.statusCode != 403 && resp.statusCode != 404) {
                throw RuntimeException("temporarymail: 读取收件箱失败 http ${resp.statusCode}")
            }
        }
        throw RuntimeException("temporarymail: 读取收件箱失败 http $lastStatus（两次尝试均被拒）")
    }

    /** 拉取单封详情（POST /api/?action=getEmail&value=<id>），风控失败返回 null 降级。 */
    private suspend fun fetchDetail(id: String): JsonObject? {
        val resp = ProviderUtil.httpPost(
            "$BASE_URL/api/?action=getEmail&value=" + ProviderUtil.urlEncode(id),
            headers = apiHeaders(UA),
        )
        if (resp.statusCode == 429 || !resp.isOk) return null
        val data = ProviderUtil.parseObject(resp.body) ?: return null
        return data.values.firstOrNull() as? JsonObject
    }

    /** 抓取 /view/ 渲染端点全文并还原纯文本。 */
    private suspend fun fetchView(id: String): String {
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/view/?i=" + ProviderUtil.urlEncode(id) + "&width=800",
            mapOf(
                "Accept" to "text/html, */*",
                "User-Agent" to UA,
                "Referer" to "$BASE_URL/",
            ),
        )
        if (!resp.isOk) return ""
        return viewToText(resp.body)
    }

    /** 将 /view/ 响应剥标签还原为纯文本（<br>/<p> 换行保留）。 */
    private fun viewToText(src: String): String {
        var s = src
        for (tag in listOf("<br />", "<br/>", "<br>", "<p>", "</p>")) {
            s = s.replace(tag, "\n")
        }
        s = Regex("<script[\\s\\S]*?</script>|<style[\\s\\S]*?</style>", RegexOption.IGNORE_CASE).replace(s, " ")
        s = Regex("<[^>]+>").replace(s, " ")
        s = s.replace("&gt;", ">").replace("&lt;", "<").replace("&quot;", "\"")
            .replace("&#39;", "'").replace("&nbsp;", " ").replace("&amp;", "&")
        return s.split("\n").joinToString("\n") { it.trim() }.trim()
    }
}