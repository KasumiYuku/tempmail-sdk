package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * NowtempMail 渠道实现（nowtempmail.com）。
 *
 * POST /mailbox 建箱（无 body 或 {}，响应 token（JWT）/mailbox），
 * GET /messages 读信列表（Header Authorization: Bearer <token>，
 *   响应 {"messages":[...]}），GET /message/{id} 取单封详情（Bearer）。
 * 列表元素按多候选字段归一；详情拉取失败时以列表摘要兜底。
 */
object Nowtempmail : Provider {

    private const val CHANNEL = "nowtempmail"
    private const val BASE_URL = "https://nowtempmail.com"

    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /** 设置 nowtempmail 请求的通用请求头（可选附带 Bearer token）。 */
    private fun authHeaders(token: String): Map<String, String> {
        return if (token.isEmpty()) headers else headers + ("Authorization" to "Bearer $token")
    }

    /** 创建 nowtempmail.com 临时邮箱：POST /mailbox 返回 token（JWT）与 mailbox 地址。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpPost(
            "$BASE_URL/mailbox", "{}", "application/json", authHeaders(""))
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("nowtempmail: 创建响应非对象")
        val token = ProviderUtil.str(data, "token").trim()
        val mailbox = ProviderUtil.str(data, "mailbox").trim()
        if (token.isEmpty() || mailbox.isEmpty() || !mailbox.contains("@")) {
            throw RuntimeException("nowtempmail: 创建邮箱响应缺少必要字段")
        }
        return EmailInfo(email = mailbox, channel = CHANNEL, token = token)
    }

    /**
     * 获取 nowtempmail.com 邮件列表。
     * 流程：GET /messages 取列表，对每个元素按 id 逐封 GET /message/{id} 合并详情；
     * 详情失败时以列表摘要归一。
     */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val addr = info.email.trim()
        val token = info.token.trim()
        val resp = ProviderUtil.httpGet("$BASE_URL/messages", authHeaders(token))
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val messages = ProviderUtil.arr(data, "messages") ?: return emptyList()

        return messages.filterIsInstance<JsonObject>().map { m ->
            val flat = mutableMapOf<String, JsonElement>()
            val id = messageIdOf(m)
            if (id.isNotEmpty()) {
                // 详情仅填补列表缺失字段（列表已有键优先，与 Go 端一致）
                getDetail(token, id)?.let { flat.putAll(it) }
            }
            flat.putAll(m)
            Normalize.fromJson(JsonObject(flat), addr)
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

    /** 获取单封详情（GET /message/{id}，Bearer token），失败返回 null。 */
    private suspend fun getDetail(token: String, id: String): JsonObject? {
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/message/" + ProviderUtil.urlEncode(id), authHeaders(token))
        if (!resp.isOk) return null
        return ProviderUtil.parseObject(resp.body)
    }
}