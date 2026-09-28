package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Huskmail 渠道实现（huskmail.xyz），Bearer（JWT）认证 REST。
 *
 * POST /api/v1/accounts 建箱（body {}，响应 id/address/password/token/expiresAt/tier），
 * GET /v1/messages 读信（Header Authorization: Bearer <token>，响应 {"messages":[...]}），
 * GET /v1/messages/{id} 取单封详情（Bearer）。
 * 收信域固定为 @huskmail.xyz。
 */
object Huskmail : Provider {

    private const val CHANNEL = "huskmail"
    private const val BASE_URL = "https://api.huskmail.space"
    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    )

    /** 附加 Bearer 认证的请求头。 */
    private fun authHeaders(token: String): Map<String, String> = headers + ("Authorization" to "Bearer $token")

    /** 从列表元素中提取邮件 ID。 */
    private fun messageIDOf(m: JsonObject): String {
        for (key in listOf("id", "Id", "slug", "messageId", "message_id")) {
            val v = m[key] ?: continue
            if (v is JsonPrimitive && v.content.isNotBlank()) return v.content
        }
        return ""
    }

    /** 创建临时邮箱：POST /v1/accounts（空 JSON body）。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpPost("$BASE_URL/v1/accounts", "{}", "application/json", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("huskmail: 建箱响应无效")
        val address = ProviderUtil.str(data, "address").trim()
        val token = ProviderUtil.str(data, "token").trim()
        if (address.isEmpty() || token.isEmpty()) {
            throw RuntimeException("huskmail: 建箱响应缺少 address 或 token")
        }
        return EmailInfo(
            email = address,
            channel = CHANNEL,
            token = token,
            expiresAt = ProviderUtil.str(data, "expiresAt"),
        )
    }

    /** 获取邮件列表：列表 + 逐封详情合并。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val token = info.token.trim()
        if (token.isEmpty()) throw RuntimeException("huskmail: token 为空")
        val email = info.email.trim()
        val resp = ProviderUtil.httpGet("$BASE_URL/v1/messages", authHeaders(token))
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val list = ProviderUtil.arr(data, "messages") ?: return emptyList()

        return list.filterIsInstance<JsonObject>().map { m ->
            val row = m.toMutableMap()
            val id = messageIDOf(m)
            if (id.isNotEmpty()) {
                try {
                    val dr = ProviderUtil.httpGet("$BASE_URL/v1/messages/$id", authHeaders(token))
                    if (dr.isOk) {
                        ProviderUtil.parseObject(dr.body)?.forEach { (k, v) -> row.putIfAbsent(k, v) }
                    }
                } catch (_: Exception) {
                    // 详情失败时回退为列表摘要
                }
            }
            Normalize.fromMap(jsonRow(row, email), email)
        }
    }

    /** 将扁平化的字段映射为标准化邮件。 */
    private fun jsonRow(m: Map<String, JsonElement>, email: String): Map<String, Any?> {
        val id = when (val v = m["id"]) {
            is JsonObject -> ProviderUtil.str(v, "id")
            is JsonPrimitive -> v.content
            else -> ""
        }
        return mapOf(
            "id" to id,
            "from" to jsonStr(m["from"]),
            "to" to email,
            "subject" to jsonStr(m["subject"]),
            "text" to jsonStr(m["text"]),
            "html" to jsonStr(m["html"]),
            "date" to jsonStr(m["date"]),
        )
    }

    /** 兼容对象/原始值形态的字符串提取。 */
    private fun jsonStr(v: JsonElement?): String = when (v) {
        null, is JsonNull -> ""
        is JsonObject, is JsonArray -> v.toString()
        else -> (v as JsonPrimitive).content
    }
}