package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Mtempmail 渠道实现（mtempmail.com，公共 key 认证）。
 *
 * 建箱: POST /api/emails/{apiKey}（body {}）→
 *   {"status":true,"data":{"email":"xxx@domain","domain":"..","expire_at":"..",
 *    "created_at":"..","id":213000,"email_token":"..."}}
 * 读信: GET /api/messages/{apiKey}/{email} →
 *   {"status":true,"mailbox":"..","email_token":"..","messages":[]}
 *   消息列表元素为 mailgun 入站 webhook 风格：
 *   {"to":[{..}],"body":[{content_type:"text/html",value:".."}],
 *    "created_at":"..","id":123,"from":[{"full":"Sender <a@b.com>"}],"subject":"..","flags":[..]}。
 */
object Mtempmail : Provider {

    private const val CHANNEL = "mtempmail"
    private const val BASE_URL = "https://mtempmail.com"

    /** 公共固定 API key（mtempmail.com 官方提供）。 */
    private const val PUBLIC_KEY = "pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw"
    private val BULLET_RE = Regex("^[•·]+\\s*")
    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    )

    /** 清洗主题前导分隔符（后台可能以 "• " 开头拼接微件）。 */
    private fun cleanSubject(s: String): String = BULLET_RE.replace(s.trim(), "")

    /** 拼接正文纯文本（body[].value 按序）。 */
    private fun bodyText(parts: List<JsonObject>): String {
        val sb = StringBuilder()
        for (p in parts) {
            val v = p["value"]
            if (v is JsonPrimitive) sb.append(v.content).append("\n")
        }
        return sb.toString().trimEnd('\n')
    }

    /** 提取首个 text/html 段。 */
    private fun bodyHTML(parts: List<JsonObject>): String {
        for (p in parts) {
            if (ProviderUtil.str(p, "content_type") == "text/html") {
                val v = p["value"]
                if (v is JsonPrimitive) return v.content
            }
        }
        return ""
    }

    /** 创建临时邮箱：POST /api/emails/{apiKey}（空 JSON body）。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpPost("$BASE_URL/api/emails/$PUBLIC_KEY", "{}", "application/json", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("mtempmail: 建箱响应无效")
        val ok = data["status"] as? JsonPrimitive
        val d = data["data"] as? JsonObject
        val email = ProviderUtil.str(d, "email").trim()
        if (ok?.booleanOrNull != true || email.isEmpty()) {
            throw RuntimeException("mtempmail: 建箱响应缺少邮箱")
        }
        return EmailInfo(
            email = email,
            channel = CHANNEL,
            token = ProviderUtil.str(d, "email_token"),
            expiresAt = ProviderUtil.str(d, "expire_at"),
            createdAt = ProviderUtil.str(d, "created_at"),
        )
    }

    /** 读取收件箱：GET /api/messages/{apiKey}/{email}。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val email = info.email.trim()
        if (email.isEmpty()) throw RuntimeException("mtempmail: 邮箱为空")
        if (info.token.isBlank()) throw RuntimeException("mtempmail: token 为空")
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/api/messages/$PUBLIC_KEY/" + ProviderUtil.urlEncode(email),
            headers,
        )
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val ok = data["status"] as? JsonPrimitive
        if (ok != null && ok.booleanOrNull != true) return emptyList()
        val messages = ProviderUtil.arr(data, "messages") ?: return emptyList()

        return messages.filterIsInstance<JsonObject>().map { m ->
            val bodyArr = m["body"] as? JsonArray
            val parts = bodyArr?.filterIsInstance<JsonObject>() ?: emptyList()
            val from = when (val f = m["from"]) {
                is JsonArray -> f.firstNotNullOfOrNull { e ->
                    val o = e as? JsonObject
                    ProviderUtil.str(o, "full").takeIf { it.isNotEmpty() }
                } ?: ""
                is JsonPrimitive -> f.content
                else -> ""
            }
            val row = mapOf(
                "id" to ProviderUtil.str(m, "id"),
                "from" to from,
                "to" to email,
                "subject" to cleanSubject(ProviderUtil.str(m, "subject")),
                "text" to bodyText(parts),
                "html" to bodyHTML(parts),
                "date" to ProviderUtil.str(m, "created_at"),
            )
            Normalize.fromMap(row, email)
        }
    }
}