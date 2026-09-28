package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import kotlin.random.Random

/**
 * ShitpostEmail 渠道实现（shitpost.email 公共实例）。
 *
 * 无认证 REST：POST /api/create 建箱（username/domain/ttl → email/token/type/expires），
 * GET /api/inbox?email=&token= 读信（messages[] 含 from/fromName/subject/text/html/date）。
 * 域名池：shitpost.email / letsfuckingpiss.party。
 */
object ShitpostEmail : Provider {

    private const val CHANNEL = "shitpost-email"
    private const val BASE_URL = "https://shitpost.email"
    private val DOMAINS = listOf("shitpost.email", "letsfuckingpiss.party")
    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    )

    /** 生成 "sdk"+10 位随机本地名。 */
    private fun localPart(): String = "sdk" + ProviderUtil.randomString(10)

    /** 创建临时邮箱：POST /api/create。 */
    override suspend fun generate(): EmailInfo {
        val dom = DOMAINS[Random.nextInt(DOMAINS.size)]
        val payload = buildJsonObject {
            put("username", localPart())
            put("domain", dom)
            put("ttl", 3600)
        }.toString()
        val resp = ProviderUtil.httpPost("$BASE_URL/api/create", payload, "application/json", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("shitpost-email: 建箱响应无效")
        val email = ProviderUtil.str(data, "email").trim()
        val token = ProviderUtil.str(data, "token").trim()
        if (email.isEmpty() || token.isEmpty()) {
            throw RuntimeException("shitpost-email: 建箱响应缺少 email 或 token")
        }
        return EmailInfo(
            email = email,
            channel = CHANNEL,
            token = token,
            expiresAt = ProviderUtil.str(data, "expires"),
        )
    }

    /** 读取收件箱：GET /api/inbox?email=&token=。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val email = info.email.trim()
        val token = info.token.trim()
        if (email.isEmpty() || token.isEmpty()) throw RuntimeException("shitpost-email: email 或 token 为空")
        val url = "$BASE_URL/api/inbox?email=" + ProviderUtil.urlEncode(email) +
            "&token=" + ProviderUtil.urlEncode(token)
        val resp = ProviderUtil.httpGet(url, headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val messages = ProviderUtil.arr(data, "messages") ?: return emptyList()
        return messages.filterIsInstance<JsonObject>().map { m ->
            val row = mapOf(
                "id" to ProviderUtil.str(m, "id"),
                "from" to ProviderUtil.firstNonEmpty(ProviderUtil.str(m, "from"), ProviderUtil.str(m, "fromName")),
                "to" to email,
                "subject" to ProviderUtil.str(m, "subject"),
                "text" to ProviderUtil.str(m, "text"),
                "html" to ProviderUtil.str(m, "html"),
                "date" to ProviderUtil.str(m, "date"),
            )
            Normalize.fromMap(row, email)
        }
    }
}