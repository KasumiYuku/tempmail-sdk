package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * ClawdEmail 渠道实现（clawdemail.com，API 域 api.clawdemail.com）。
 *
 * POST /register 建箱（无验证码，body {"name":"xxx"}，响应 success/email/token），
 * GET /inbox 读信列表（Header Authorization: Bearer <token>，
 *   响应 success/email/count/unread/emails[]），GET /email/{id} 取单封详情（Bearer）。
 * 信件保留 30 分钟，仅接收不发送，正文仅纯文本。
 */
object Clawdemail : Provider {

    private const val CHANNEL = "clawdemail"
    private const val BASE_URL = "https://api.clawdemail.com"

    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /** 设置 clawdemail 请求的通用请求头（可选附带 Bearer token）。 */
    private fun authHeaders(token: String): Map<String, String> {
        return if (token.isEmpty()) headers else headers + ("Authorization" to "Bearer $token")
    }

    /** 创建 clawdemail.com 临时邮箱：POST /register（body {"name":""}）返回 email 与 token。 */
    override suspend fun generate(): EmailInfo {
        val body = buildJsonObject { put("name", "") }.toString()
        val resp = ProviderUtil.httpPost("$BASE_URL/register", body, "application/json", authHeaders(""))
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("clawdemail: 创建响应非对象")
        val email = ProviderUtil.str(data, "email").trim()
        val token = ProviderUtil.str(data, "token").trim()
        if (email.isEmpty() || token.isEmpty() || !email.contains("@")) {
            throw RuntimeException("clawdemail: 创建邮箱响应缺少必要字段")
        }
        return EmailInfo(email = email, channel = CHANNEL, token = token)
    }

    /**
     * 获取 clawdemail.com 邮件列表。
     * 流程：GET /inbox?limit=50 取列表，对每个元素按 id 逐封 GET /email/{id} 合并详情；
     * 详情失败时以列表摘要归一。
     */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val addr = info.email.trim()
        val token = info.token.trim()
        val resp = ProviderUtil.httpGet("$BASE_URL/inbox?limit=50", authHeaders(token))
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("clawdemail: 收件箱响应非对象")
        val ok = (data["success"] as? JsonPrimitive)?.booleanOrNull ?: false
        if (!ok) throw RuntimeException("clawdemail: 读取收件箱失败: ${ProviderUtil.str(data, "error")}")
        val emails = ProviderUtil.arr(data, "emails") ?: return emptyList()

        return emails.filterIsInstance<JsonObject>().map { m ->
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

    /**
     * 获取单封详情（GET /email/{id}，Bearer token），
     * 响应含 email 嵌套对象（from_addr/body_text/received_at）时提升嵌套对象。
     */
    private suspend fun getDetail(token: String, id: String): JsonObject? {
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/email/" + ProviderUtil.urlEncode(id), authHeaders(token))
        if (!resp.isOk) return null
        val detail = ProviderUtil.parseObject(resp.body) ?: return null
        val nested = detail["email"] as? JsonObject
        return nested ?: detail
    }
}