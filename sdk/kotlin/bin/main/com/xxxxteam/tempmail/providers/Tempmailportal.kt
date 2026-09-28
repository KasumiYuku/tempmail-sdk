package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Tempmailportal 渠道实现（api.tempmailportal.com）。
 *
 * POST /api/v2/inbox 建箱（body {}，响应 address/token/private/expiresAt/retentionMs，token 为 p2 前缀），
 * GET /api/messages 读信（Header Authorization: Bearer <token>），
 * GET /api/messages/{id} 取单封详情（Bearer）。
 */
object Tempmailportal : Provider {

    private const val CHANNEL = "tempmailportal"
    private const val BASE_URL = "https://api.tempmailportal.com"
    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    )

    /** 附加 Bearer 认证的请求头。 */
    private fun authHeaders(token: String): Map<String, String> {
        if (token.isEmpty()) return headers
        return headers + ("Authorization" to "Bearer $token")
    }

    /** 创建临时邮箱：POST /api/v2/inbox（空 JSON body）。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpPost("$BASE_URL/api/v2/inbox", "{}", "application/json", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("tempmailportal: 建箱响应无效")
        val address = ProviderUtil.str(data, "address").trim()
        val token = ProviderUtil.str(data, "token").trim()
        if (address.isEmpty() || token.isEmpty()) {
            throw RuntimeException("tempmailportal: 建箱响应缺少 address 或 token")
        }
        return EmailInfo(
            email = address,
            channel = CHANNEL,
            token = token,
            expiresAt = ProviderUtil.str(data, "expiresAt"),
        )
    }

    /** 获取邮件列表：逐封合并详情。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val token = info.token.trim()
        if (token.isEmpty()) throw RuntimeException("tempmailportal: token 为空")
        val email = info.email.trim()
        val resp = ProviderUtil.httpGet("$BASE_URL/api/messages", authHeaders(token))
        resp.ensureSuccess()
        val list = ProviderUtil.parse(resp.body) as? JsonArray ?: return emptyList()

        return list.filterIsInstance<JsonObject>().map { m ->
            val row = m.toMutableMap()
            val id = ProviderUtil.str(m, "id")
            if (id.isNotEmpty()) {
                try {
                    val dr = ProviderUtil.httpGet("$BASE_URL/api/messages/" + ProviderUtil.urlEncode(id), authHeaders(token))
                    if (dr.isOk) {
                        ProviderUtil.parseObject(dr.body)?.forEach { (k, v) -> row.putIfAbsent(k, v) }
                    }
                } catch (_: Exception) {
                    // 详情失败时回退为列表摘要
                }
            }
            normalizeRow(row, email)
        }
    }

    /** 将扁平化的字段映射为标准化邮件。 */
    private fun normalizeRow(m: Map<String, JsonElement>, email: String): Email {
        val row = mapOf(
            "id" to jsonStr(m["id"]),
            "from" to jsonStr(m["from"]),
            "to" to email,
            "subject" to jsonStr(m["subject"]),
            "text" to jsonStr(m["text"]),
            "html" to jsonStr(m["html"]),
            "date" to jsonStr(m["date"]),
        )
        return Normalize.fromMap(row, email)
    }

    /** 兼容对象/原始值形态的字符串提取。 */
    private fun jsonStr(v: JsonElement?): String = when (v) {
        null, is JsonNull -> ""
        is JsonObject, is JsonArray -> v.toString()
        else -> (v as JsonPrimitive).content
    }
}