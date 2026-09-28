package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Zerodrop 渠道实现（zerodrop.dev）。
 *
 * 无认证 REST：建箱无需请求，本地生成 "sdk"+8 位随机名，地址为 <名>@zerodrop-sandbox.online；
 * 读信 GET /api/inbox/{name}?source=sdk，响应形如 {"emails":[...],"count":N}。
 * 平台邮件对象只有 id/from/to/subject/receivedAt/raw/otp/magicLink：正文仅存在于
 * raw（完整 MIME 原文，头部与 body 以空行分隔），须从 raw 中剥离头部提取纯文本 body。
 */
object Zerodrop : Provider {

    private const val CHANNEL = "zerodrop"
    private const val BASE_URL = "https://zerodrop.dev"
    private const val DOMAIN = "zerodrop-sandbox.online"
    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    )

    /** 从 raw（完整 MIME 原文）提取纯文本正文：首个空行（RFC 5322 分隔）之后即 body。 */
    private fun rawBody(raw: String): String {
        val iCRLF = raw.indexOf("\r\n\r\n")
        if (iCRLF >= 0) return raw.substring(iCRLF + 4)
        val iLF = raw.indexOf("\n\n")
        if (iLF >= 0) return raw.substring(iLF + 2)
        return ""
    }

    /** 多候选字段取值：返回第一个存在且非空的字符串值。 */
    private fun pickStr(m: JsonObject, vararg keys: String): String {
        for (k in keys) {
            val v = m[k] ?: continue
            val s = when (v) {
                is JsonPrimitive -> v.content
                else -> v.toString()
            }
            if (v !is JsonNull && s.isNotEmpty()) return s
        }
        return ""
    }

    /** 创建临时邮箱：本地生成，无需请求服务端。 */
    override suspend fun generate(): EmailInfo {
        val email = "sdk" + ProviderUtil.randomString(8) + "@" + DOMAIN
        return EmailInfo(email = email, channel = CHANNEL, token = email)
    }

    /** 读取收件箱：GET /api/inbox/{name}?source=sdk。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val email = info.email.trim()
        val local = email.substringBefore('@')
        val domain = email.substringAfter('@', "")
        if (local.isEmpty() || domain != DOMAIN) {
            throw RuntimeException("zerodrop: 非 $DOMAIN 域邮箱地址")
        }
        val resp = ProviderUtil.httpGet("$BASE_URL/api/inbox/" + ProviderUtil.urlEncode(local) + "?source=sdk", headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: return emptyList()
        val emails = ProviderUtil.arr(data, "emails") ?: return emptyList()

        return emails.filterIsInstance<JsonObject>().map { m ->
            val row = mapOf(
                "id" to ProviderUtil.str(m, "id"),
                "from" to ProviderUtil.str(m, "from"),
                "to" to email,
                "subject" to ProviderUtil.str(m, "subject"),
                "text" to rawBody(pickStr(m, "raw")),
                "date" to pickStr(m, "receivedAt", "date"),
            )
            Normalize.fromMap(row, email)
        }
    }
}