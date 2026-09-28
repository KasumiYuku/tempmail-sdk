package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import kotlin.random.Random
import java.time.Instant

/**
 * TenminApp 渠道实现（tenmin.app，真实 API 域 api.tenmin.app）。
 *
 * 建箱+读信共用 GET /api/inbox/{localpart}（Accept: application/json）。
 * localpart 为随机 6 位小写十六进制串（首访即建箱，无显式创建接口）；
 * 响应：{"inboxId":..,"address":"..@tenmin.app","ttl":600,"count":0,"messages":[]}，
 * messages[] 元素字段：id/from/subject/text/html/receivedAt（from 为 {name,address} 对象）。
 */
object TenminApp : Provider {

    private const val CHANNEL = "tenmin-app"
    private const val BASE_URL = "https://api.tenmin.app"
    private const val DOMAIN = "tenmin.app"
    private const val HEX_CHARS = "0123456789abcdef"
    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    )

    /** 生成 6 位小写十六进制随机 localpart。 */
    private fun localPart(): String {
        val sb = StringBuilder(6)
        repeat(6) { sb.append(HEX_CHARS[Random.nextInt(HEX_CHARS.length)]) }
        return sb.toString()
    }

    /** 请求 /api/inbox/{localpart}。 */
    private suspend fun fetchInbox(localpart: String): JsonObject {
        val resp = ProviderUtil.httpGet("$BASE_URL/api/inbox/$localpart", headers)
        resp.ensureSuccess()
        return ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("tenmin-app: 收件箱响应无效")
    }

    /** 创建临时邮箱：首次 GET 随机 localpart 即自动建箱（10 分钟 TTL）。 */
    override suspend fun generate(): EmailInfo {
        val local = localPart()
        val data = fetchInbox(local)
        val address = ProviderUtil.str(data, "address").ifEmpty { "$local@$DOMAIN" }
        val ttl = (data["ttl"] as? JsonPrimitive)?.longOrNull ?: 0L
        val expiresAt = if (ttl > 0) Instant.now().plusSeconds(ttl).toString() else ""
        return EmailInfo(email = address, channel = CHANNEL, token = local, expiresAt = expiresAt)
    }

    /** 读取收件箱：复用建箱同一 localpart；from 为 {name,address} 对象时拆出地址。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val token = info.token.trim()
        if (token.isEmpty()) throw RuntimeException("tenmin-app: token 为空")
        val email = info.email.trim()
        val data = fetchInbox(token)
        val messages = ProviderUtil.arr(data, "messages") ?: return emptyList()

        return messages.filterIsInstance<JsonObject>().map { m ->
            val from = when (val f = m["from"]) {
                is JsonObject -> {
                    val addr = ProviderUtil.str(f, "address")
                    val name = ProviderUtil.str(f, "name")
                    when {
                        addr.isNotEmpty() && name.isNotEmpty() -> "$name <$addr>"
                        addr.isNotEmpty() -> addr
                        else -> name
                    }
                }
                is JsonPrimitive -> f.content
                else -> ""
            }
            val row = mapOf(
                "id" to ProviderUtil.str(m, "id"),
                "from" to from,
                "to" to email,
                "subject" to ProviderUtil.str(m, "subject"),
                "text" to ProviderUtil.str(m, "text"),
                "html" to ProviderUtil.str(m, "html"),
                "date" to ProviderUtil.str(m, "receivedAt"),
            )
            Normalize.fromMap(row, email)
        }
    }
}