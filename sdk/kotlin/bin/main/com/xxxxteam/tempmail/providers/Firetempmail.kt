package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import kotlin.random.Random

/**
 * Firetempmail 渠道实现（firetempmail.com）。
 *
 * 无认证 REST：建箱无需请求，本地生成 随机词+0-999@<域>。
 * 读信 GET https://mail.firetempmail.com/mail/get?address=<邮箱 URL 编码>，
 * 必须携带 Header Origin: https://firetempmail.com（否则 403 'Origin not allowed'）。
 * 响应形如 {"status":"ok","code":200,"msg":"...","stats":{},"mails":[...]}，
 * 邮件字段以 sender/subject/date/recipient + content-html/content-text/content-plain 多候选归一化。
 */
object Firetempmail : Provider {

    private const val CHANNEL = "firetempmail"
    private const val API_BASE = "https://mail.firetempmail.com"
    private const val ORIGIN = "https://firetempmail.com"
    private val DOMAINS = listOf("offrework.click", "service-today.click", "jobsdeforyou.sa.com")
    private val CHARS = "abcdefghijklmnopqrstuvwxyz"
    private val headers = mapOf(
        "Accept" to "application/json",
        "Origin" to ORIGIN,
        "Referer" to "$ORIGIN/",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    )

    /** 随机 3-6 位小写字母词 + 0-999（与官网 faker 词 + 1e3 取整一致）。 */
    private fun localPart(): String {
        val n = 3 + Random.nextInt(4)
        val sb = StringBuilder(n + 3)
        repeat(n) { sb.append(CHARS[Random.nextInt(CHARS.length)]) }
        sb.append(Random.nextInt(1000))
        return sb.toString()
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
        val dom = DOMAINS[Random.nextInt(DOMAINS.size)]
        val email = localPart() + "@" + dom
        return EmailInfo(email = email, channel = CHANNEL, token = email)
    }

    /** 读取收件箱：GET /mail/get?address=<URL 编码完整邮箱>，必带 Origin 头。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val email = info.email.trim()
        if (email.isEmpty()) throw RuntimeException("firetempmail: 邮箱地址为空")
        val resp = ProviderUtil.httpGet("$API_BASE/mail/get?address=" + ProviderUtil.urlEncode(email), headers)
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body) ?: throw RuntimeException("firetempmail: 收件箱响应无效")
        val status = ProviderUtil.str(data, "status")
        if (status.isNotEmpty() && status != "ok") {
            throw RuntimeException("firetempmail: 读信失败 ${ProviderUtil.str(data, "msg")}")
        }
        val mails = ProviderUtil.arr(data, "mails") ?: return emptyList()

        return mails.filterIsInstance<JsonObject>().map { m ->
            val row = mapOf(
                "id" to ProviderUtil.str(m, "id"),
                "from" to pickStr(m, "sender", "from", "from_address"),
                "to" to email,
                "subject" to pickStr(m, "subject", "title"),
                "html" to pickStr(m, "content-html", "html"),
                "text" to pickStr(m, "content-text", "content-plain", "text"),
                "date" to pickStr(m, "date", "received_at", "created_at"),
            )
            Normalize.fromMap(row, email)
        }
    }
}