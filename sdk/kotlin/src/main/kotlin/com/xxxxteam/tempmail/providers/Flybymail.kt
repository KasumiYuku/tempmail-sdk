package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Flybymail 渠道实现（flybymail.com）。
 *
 * POST /api/recipients 建箱（空 JSON body，响应 id/email/createdAt/expiresAt，
 *   expiresAt 为毫秒时间戳），GET /api/recipients/{email}/emails 读信
 *   （按邮箱地址、非 id 查询，响应 {"emails":[...]}）。
 * 信件字段：id/from/to/subject/body（纯文本）/htmlBody（HTML 正文）/preview/time/read。
 */
object Flybymail : Provider {

    private const val CHANNEL = "flybymail"
    private const val BASE_URL = "https://flybymail.com"

    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /**
     * 创建 flybymail.com 临时邮箱。
     * POST /api/recipients（空 JSON body）返回 id/email/createdAt/expiresAt，
     * expiresAt 为毫秒时间戳（约 4 小时）转 ISO8601 供 EmailInfo 统一展示。
     */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpPost("$BASE_URL/api/recipients", "{}", "application/json", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("flybymail: 创建响应非对象")
        val id = ProviderUtil.str(data, "id").trim()
        val email = ProviderUtil.str(data, "email").trim()
        if (id.isEmpty() || email.isEmpty() || !email.contains("@")) {
            throw RuntimeException("flybymail: 创建邮箱响应缺少必要字段")
        }
        val expiresMillis = ProviderUtil.str(data, "expiresAt").toLongOrNull() ?: 0L
        val expiresAt = if (expiresMillis > 0) {
            java.time.Instant.ofEpochMilli(expiresMillis).toString()
        } else {
            ""
        }
        return EmailInfo(email = email, channel = CHANNEL, token = id, expiresAt = expiresAt)
    }

    /** 获取 flybymail.com 邮件列表（GET /api/recipients/{email}/emails，按地址查询）。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val addr = info.email.trim()
        if (addr.isEmpty() || !addr.contains("@")) {
            throw RuntimeException("flybymail: 邮箱地址为空或格式错误")
        }
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/api/recipients/" + ProviderUtil.urlEncode(addr) + "/emails", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val emails = ProviderUtil.arr(data, "emails") ?: return emptyList()

        return emails.filterIsInstance<JsonObject>().map { raw ->
            // id 为数字时按字符串归一，time 为毫秒时间戳时按 timestamp 归一
            val row = mapOf<String, Any?>(
                "id" to ProviderUtil.str(raw, "id").trim(),
                "from" to strOrNull(raw, "from"),
                "to" to strOrNull(raw, "to"),
                "subject" to strOrNull(raw, "subject"),
                "text" to strOrNull(raw, "body"),
                "html" to strOrNull(raw, "htmlBody"),
                "time" to strOrNull(raw, "time"),
                "read" to strOrNull(raw, "read"),
                "attachments" to strOrNull(raw, "attachments"),
                "timestamp" to (strOrNull(raw, "time") ?: strOrNull(raw, "date")),
            )
            Normalize.fromMap(row, addr)
        }
    }

    /** 取字符串字段（JsonNull 视为缺失）。 */
    private fun strOrNull(obj: JsonObject, key: String): String? {
        val v = obj[key] ?: return null
        if (v is JsonNull) return null
        return if (v is JsonPrimitive) v.content else v.toString()
    }
}