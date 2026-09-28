package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Nullmail 渠道实现（nullmail.cc / maildock.store）。
 *
 * 无认证 REST：POST /api/emails（空 JSON body）建箱，响应 {"address":"...@maildock.store","expiry":"..."}；
 * 读信 GET /api/emails/{address}（URL 编码），响应 {"expiry":"...","emails":[...]}，
 * 列表项只有 id/sender/subject/delivered，正文须逐封二拉 GET /api/emails/{addr}/body/{id}
 * （响应 {"body":...}）。
 */
object Nullmail : Provider {

    private const val CHANNEL = "nullmail"
    private const val BASE_URL = "https://www.nullmail.cc"
    private val headers = mapOf(
        "Accept" to "application/json",
        "Origin" to BASE_URL,
        "Referer" to "$BASE_URL/",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    )

    /** 创建临时邮箱：POST /api/emails（空 JSON body），token 复用完整地址。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpPost("$BASE_URL/api/emails", "{}", "application/json", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("nullmail: 建箱响应无效")
        val addr = ProviderUtil.str(data, "address").trim()
        if (addr.isEmpty()) throw RuntimeException("nullmail: 建箱响应缺少 address 字段")
        return EmailInfo(
            email = addr,
            channel = CHANNEL,
            token = addr,
            expiresAt = ProviderUtil.str(data, "expiry"),
        )
    }

    /** 读取收件箱：GET /api/emails/{address}，正文逐封二拉。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val email = info.email.trim()
        if (email.isEmpty()) throw RuntimeException("nullmail: 邮箱地址为空")
        val resp = ProviderUtil.httpGet("$BASE_URL/api/emails/" + ProviderUtil.urlEncode(email), headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val emails = ProviderUtil.arr(data, "emails") ?: return emptyList()

        return emails.filterIsInstance<JsonObject>().map { m ->
            val body = fetchBody(email, m)
            val row = mapOf(
                "id" to ProviderUtil.str(m, "id"),
                "from" to ProviderUtil.str(m, "sender"),
                "to" to email,
                "subject" to ProviderUtil.str(m, "subject"),
                "text" to body,
                "date" to ProviderUtil.str(m, "delivered"),
            )
            Normalize.fromMap(row, email)
        }
    }

    /** 单封正文二拉：GET /api/emails/{addr}/body/{id}，失败降级留空不阻断列表。 */
    private suspend fun fetchBody(addr: String, m: JsonObject): String {
        val id = m["id"] ?: return ""
        val idStr = (id as? JsonPrimitive)?.content ?: id.toString()
        if (idStr.isEmpty()) return ""
        return try {
            val resp = ProviderUtil.httpGet(
                "$BASE_URL/api/emails/" + ProviderUtil.urlEncode(addr) +
                    "/body/" + ProviderUtil.urlEncode(idStr),
                headers,
            )
            if (!resp.isOk) "" else ProviderUtil.str(ProviderUtil.parseObject(resp.body), "body")
        } catch (_: Exception) {
            ""
        }
    }
}