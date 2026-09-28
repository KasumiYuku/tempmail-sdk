package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Crazymailing 渠道实现（crazymailing.com）。
 *
 * Next.js 全栈站点，API 结构：
 *   POST /api/mailbox（空 JSON body）建箱，响应
 *     {"mailbox":{"id":"...","address":"...@crazymailing.com","expiresAt":"<RFC3339>"}}；
 *   GET /api/messages?mailbox=<完整地址 URL 编码> 读信，响应 {"messages":[...]}；
 *   GET /api/message/{id}/body 取单封正文（完整 HTML 页面）。
 * 请求需携带 Origin/Referer 浏览器形态头。
 */
object Crazymailing : Provider {

    private const val CHANNEL = "crazymailing"
    private const val BASE_URL = "https://crazymailing.com"

    private val headers = mapOf(
        "Accept" to "application/json",
        "Origin" to BASE_URL,
        "Referer" to "$BASE_URL/",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /** 创建临时邮箱：POST /api/mailbox（空 JSON body）；域名由服务端统一分配。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpPost(
            "$BASE_URL/api/mailbox", "{}", "application/json", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("crazymailing: 创建响应非对象")
        val mailbox = data["mailbox"] as? JsonObject
            ?: throw RuntimeException("crazymailing: 创建响应缺少 mailbox")
        val address = ProviderUtil.str(mailbox, "address").trim()
        if (address.isEmpty()) {
            throw RuntimeException("crazymailing: 创建响应缺少 mailbox.address")
        }
        return EmailInfo(
            email = address,
            channel = CHANNEL,
            token = ProviderUtil.str(mailbox, "id"),
            expiresAt = ProviderUtil.str(mailbox, "expiresAt"),
        )
    }

    /**
     * 读取收件箱：GET /api/messages?mailbox=<完整地址>；
     * 对每个元素逐封 GET /api/message/{id}/body 拉取正文（失败不阻断列表）。
     */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val addr = info.email.trim()
        if (addr.isEmpty()) throw RuntimeException("crazymailing: 邮箱地址为空")
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/api/messages?mailbox=" + ProviderUtil.urlEncode(addr), headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val messages = ProviderUtil.arr(data, "messages") ?: return emptyList()

        return messages.filterIsInstance<JsonObject>().map { m ->
            val row = m.toMutableMap()
            if (!row.containsKey("to")) row["to"] = JsonPrimitive(addr)
            val id = messageIdOf(m)
            if (id.isNotEmpty()) {
                // 列表元素为摘要，正文须逐封二拉（失败不阻断）
                val html = getBody(id)
                if (html.isNotEmpty()) row["html"] = JsonPrimitive(html)
            }
            Normalize.fromJson(JsonObject(row), addr)
        }
    }

    /** 从列表元素提取邮件 ID，候选字段 id/Id/slug/messageId/message_id。 */
    private fun messageIdOf(m: JsonObject): String {
        for (key in listOf("id", "Id", "slug", "messageId", "message_id")) {
            val v = m[key] ?: continue
            val s = ProviderUtil.str(m, key).trim()
            if (v !is JsonNull && s.isNotEmpty()) return s
        }
        return ""
    }

    /** 拉取单封正文（GET /api/message/{id}/body，响应为完整 HTML 页面）。 */
    private suspend fun getBody(id: String): String {
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/api/message/" + ProviderUtil.urlEncode(id) + "/body",
            mapOf(
                "Accept" to "text/html,application/xhtml+xml,*/*;q=0.8",
                "Origin" to BASE_URL,
                "Referer" to "$BASE_URL/",
                "User-Agent" to headers["User-Agent"].orEmpty(),
            ),
        )
        if (!resp.isOk) return ""
        return resp.body
    }
}