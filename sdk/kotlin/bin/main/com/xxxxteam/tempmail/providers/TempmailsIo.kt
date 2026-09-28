package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * TempmailsIo 渠道实现（tempmails.io）。
 *
 * 无认证 REST：POST /api/temp-mail/generate 建箱（响应 email/token/expires_at），
 * GET /api/temp-mail/inbox/{token} 读信（messages[] 含 from_email/text_body/html_body/attachments）。
 * 邮箱借用 uberip.com 等公共域（10 分钟自动过期）。
 */
object TempmailsIo : Provider {

    private const val CHANNEL = "tempmails-io"
    private const val BASE_URL = "https://tempmails.io"
    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    )

    /** 创建 10 分钟临时邮箱：POST /api/temp-mail/generate（空 JSON body）。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpPost("$BASE_URL/api/temp-mail/generate", "{}", "application/json", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("tempmails-io: 建箱响应无效")
        val ok = data["success"] as? JsonPrimitive
        val dataObj = data["data"] as? JsonObject
        val email = ProviderUtil.str(dataObj, "email").trim()
        val token = ProviderUtil.str(dataObj, "token").trim()
        if (ok?.booleanOrNull != true || email.isEmpty() || token.isEmpty()) {
            throw RuntimeException("tempmails-io: 建箱响应缺少 email 或 token")
        }
        return EmailInfo(
            email = email,
            channel = CHANNEL,
            token = token,
            expiresAt = ProviderUtil.str(dataObj, "expires_at"),
        )
    }

    /** 读取收件箱：触发同步后读静态 inbox。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val token = info.token.trim()
        if (token.isEmpty()) throw RuntimeException("tempmails-io: token 为空")
        val email = info.email.trim()

        // 1) 触发平台对上游信箱的主动同步（失败不致命，仍尝试静态读）
        try {
            ProviderUtil.httpPost("$BASE_URL/api/temp-mail/fetch-emails/$token", null, null, headers)
        } catch (_: Exception) {
            // 同步失败仅降级为静态读取
        }

        // 2) 读静态收件箱
        val resp = ProviderUtil.httpGet("$BASE_URL/api/temp-mail/inbox/$token", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val dataObj = data["data"] as? JsonObject ?: return emptyList()
        val messages = ProviderUtil.arr(dataObj, "messages") ?: return emptyList()
        return messages.filterIsInstance<JsonObject>().map { m ->
            val row = mapOf(
                "id" to ProviderUtil.str(m, "id"),
                "from" to ProviderUtil.str(m, "from_email"),
                "to" to email,
                "subject" to ProviderUtil.str(m, "subject"),
                "text" to ProviderUtil.str(m, "text_body"),
                "html" to ProviderUtil.str(m, "html_body"),
                "date" to ProviderUtil.str(m, "received_at"),
            )
            Normalize.fromMap(row, email)
        }
    }
}