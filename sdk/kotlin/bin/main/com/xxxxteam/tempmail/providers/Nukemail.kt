package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import kotlin.random.Random
import java.security.MessageDigest

/**
 * Nukemail 渠道实现（nukemail.app）。
 *
 * 完整接入契约：
 *   挑战 GET /api/pow/challenge?difficulty=4 → 200 {"id","challenge","difficulty"}；
 *     求 nonce 使 SHA-256(challenge+nonce) 十六进制前 4 位为 0
 *     （前端 solvePow 逐 nonce 自 0 递增）。
 *   建箱 POST /api/inbox/create body {"address","domain","pow_id","pow_nonce"}
 *     → 200 {"token":"NUKE-xxxxxxxx","email":"名@域名"}，并
 *     Set-Cookie: nukemail_token=<token>（Secure; HttpOnly; SameSite=lax, 72h）。
 *   读信 GET /api/inbox（Cookie: nukemail_token=<token>）→ 200
 *     {"token","state","addresses":[...],"messages":[...],"is_premium"...}；
 *     messages 元素字段为 sender、sender_name、subject、body_html、body_text、
 *     received_at、read。
 *   会话恢复 POST /api/inbox/resume {"accessCode":<token>} 重设 Cookie，仅作兜底。
 * 会话隔离：nukemail_token 由生成结果持久化为 token，读信时以显式 Cookie 头携带。
 */
object Nukemail : Provider {

    private const val CHANNEL = "nukemail"
    private const val BASE_URL = "https://nukemail.app"

    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /** 求解 PoW：返回使 SHA-256(challenge+nonce) 十六进制前 difficulty 位为 0 的最小 nonce。 */
    private fun solvePow(challenge: String, difficulty: Int): Long {
        val prefix = "0".repeat(difficulty)
        val md = MessageDigest.getInstance("SHA-256")
        var nonce = 0L
        while (true) {
            md.reset()
            val digest = md.digest("$challenge$nonce".toByteArray(Charsets.UTF_8))
            val hex = digest.joinToString("") { "%02x".format(it.toInt() and 0xFF) }
            if (hex.startsWith(prefix)) return nonce
            nonce++
        }
    }

    /** 生成本地随机名（与前端 generateRandomName 等价形态）。 */
    private fun randomAddress(): String = "nuke" + ProviderUtil.randomString(10)

    /** 取第一个非 premium 的活跃域名。 */
    private suspend fun defaultDomain(): String {
        val resp = ProviderUtil.httpGet("$BASE_URL/api/domains", headers)
        if (!resp.isOk) throw RuntimeException("nukemail generate: domains http ${resp.statusCode}")
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("nukemail generate: domains 响应非对象")
        val domains = ProviderUtil.arr(data, "domains")
            ?: throw RuntimeException("nukemail generate: domains 响应非数组")
        for (d in domains.filterIsInstance<JsonObject>()) {
            val domain = ProviderUtil.str(d, "domain").trim()
            val premium = (d["is_premium_only"] as? JsonPrimitive)?.booleanOrNull ?: false
            if (domain.isNotEmpty() && !premium) return domain
        }
        throw RuntimeException("nukemail generate: 无可用非 premium 域名")
    }

    /** 创建临时邮箱（PoW 建箱）；token 为平台返回的 NUKE-<随机> 访问码。 */
    override suspend fun generate(): EmailInfo {
        // 1) 取 PoW 挑战
        val resp = ProviderUtil.httpGet("$BASE_URL/api/pow/challenge?difficulty=4", headers)
        if (!resp.isOk) throw RuntimeException("nukemail generate: challenge http ${resp.statusCode}")
        val ch = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("nukemail generate: challenge 响应非对象")
        val chId = ProviderUtil.str(ch, "id").trim()
        val challenge = ProviderUtil.str(ch, "challenge").trim()
        if (chId.isEmpty() || challenge.isEmpty()) {
            throw RuntimeException("nukemail generate: challenge 响应缺少 id/challenge")
        }
        var difficulty = ProviderUtil.str(ch, "difficulty").toIntOrNull() ?: 0
        if (difficulty <= 0) difficulty = 4

        // 2) 本地求 PoW 解（SHA-256 前缀 4 零）
        val nonce = solvePow(challenge, difficulty)

        // 3) 取域名并建箱
        val dom = defaultDomain()
        val body = buildJsonObject {
            put("address", randomAddress())
            put("domain", dom)
            put("pow_id", chId)
            put("pow_nonce", nonce.toString())
        }.toString()
        val resp2 = ProviderUtil.httpPost("$BASE_URL/api/inbox/create", body, "application/json", headers)
        if (!resp2.isOk) throw RuntimeException("nukemail generate: create http ${resp2.statusCode}")
        val data = ProviderUtil.parseObject(resp2.body)
            ?: throw RuntimeException("nukemail generate: create 响应非对象")
        val token = ProviderUtil.str(data, "token").trim()
        val email = ProviderUtil.str(data, "email").trim()
        if (token.isEmpty() || email.isEmpty()) {
            throw RuntimeException("nukemail generate: create 响应缺少 token/email")
        }
        return EmailInfo(email = email, channel = CHANNEL, token = token)
    }

    /**
     * 读取收件箱。
     * 主通道 GET /api/inbox 带 Cookie: nukemail_token=<token>；
     * 会话过期时经 POST /api/inbox/resume 显式重设会话后重试一次。
     */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val addr = info.email.trim()
        val tk = info.token.trim()
        if (tk.isEmpty()) throw RuntimeException("nukemail: token 为空")
        val cookie = "nukemail_token=$tk"

        suspend fun fetch(): JsonObject {
            val resp = ProviderUtil.httpGet("$BASE_URL/api/inbox", headers + ("Cookie" to cookie))
            if (!resp.isOk) throw RuntimeException("nukemail 读信: http ${resp.statusCode}")
            return ProviderUtil.parseObject(resp.body)
                ?: throw RuntimeException("nukemail 读信: 响应非对象")
        }

        var data = fetch()
        val state = ProviderUtil.str(data, "state")
        if (state.isEmpty() || state == "expired") {
            // 会话可能已过期：经 resume 重设会话后重试
            val resumeBody = buildJsonObject { put("accessCode", tk) }.toString()
            ProviderUtil.httpPost(
                "$BASE_URL/api/inbox/resume", resumeBody, "application/json",
                mapOf("User-Agent" to headers["User-Agent"].orEmpty()),
            )
            data = fetch()
        }

        val messages = ProviderUtil.arr(data, "messages") ?: return emptyList()
        return messages.filterIsInstance<JsonObject>().map { m ->
            val flat = m.toMutableMap()
            flat["to"] = JsonPrimitive(addr)
            // 平台消息字段为 body_html/body_text，补齐 text/html 候选（若原字段缺失）
            if (!flat.containsKey("text")) m["body_text"]?.let { flat["text"] = it }
            if (!flat.containsKey("html")) m["body_html"]?.let { flat["html"] = it }
            m["received_at"]?.let { flat["date"] = it }
            m["read"]?.let { flat["read"] = it }
            // sender 是小写发件人地址，sender_name 是展示名
            m["sender"]?.let { flat["sender_email"] = it }
            Normalize.fromJson(JsonObject(flat), addr)
        }
    }
}