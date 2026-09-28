package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import kotlin.random.Random

/**
 * Shadowmail 渠道实现（shadowmail.win）。
 *
 * 完整接入契约：
 *   注册 POST /api/register {"email":"<随机前缀>@gmail.com","password":"Abcd1234!"}
 *     → 200 {"message":"Successfully Registered"}；
 *   登录 POST /api/login → 200 {"message":"Successfull Login"}
 *     并 Set-Cookie: sessionId=<uuid>（HttpOnly; Secure; Max-Age 3600）；
 *   建箱 POST /api/new-address → 200 {...,"address":"<id>@shadowmail.win","id":<id>}；
 *   读信 POST /api/get-emails {"address":"<地址>"} → 200 {"message":"Emails read",
 *     "mails":[...]}；mails 元素字段 id/address_id/sender/subject/body/created_at。
 * 会话隔离：全域使用显式 Cookie 头（sessionId=<uuid>），凭据串 token 持久化
 *   注册邮箱/密码/会话 id，便于会话过期后自动重生。
 */
object Shadowmail : Provider {

    private const val CHANNEL = "shadowmail"
    private const val BASE_URL = "https://shadowmail.win"

    /** 固定注册密码（平台无自选密码入口，注册即固定）。 */
    private const val PW = "Abcd1234!"

    /** 平台唯一收信域。 */
    private const val DOMAIN = "shadowmail.win"

    /** 本渠道凭据串前缀。 */
    private const val TOKEN_PREFIX = "shadowmail|"

    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /** 生成随机注册邮箱前缀（sdk+8 位小写字母）。 */
    private fun randomAccount(): String {
        val chars = "abcdefghijklmnopqrstuvwxyz"
        val sb = StringBuilder(8)
        repeat(8) { sb.append(chars[Random.nextInt(chars.length)]) }
        return "sdk" + sb
    }

    /** 携带显式 Cookie 的 JSON POST 请求，返回 [响应对象, 状态码, Set-Cookie 列表]。 */
    private suspend fun doPost(path: String, body: JsonObject, cookie: String): Triple<JsonObject?, Int, List<String>> {
        val hdrs = if (cookie.isEmpty()) headers else headers + ("Cookie" to cookie)
        val resp = ProviderUtil.httpPost("$BASE_URL$path", body.toString(), "application/json", hdrs)
        return Triple(ProviderUtil.parseObject(resp.body), resp.statusCode, resp.setCookies)
    }

    /** 从 Set-Cookie 中提取 sessionId 值（纯 uuid，不含键名）。 */
    private fun sessionFrom(setCookies: List<String>): String {
        for (sc in setCookies) {
            val kv = sc.split(";", limit = 2)[0].trim()
            if (kv.startsWith("sessionId=")) return kv.removePrefix("sessionId=")
        }
        return ""
    }

    /** 注册或登录（POST /api/register、/api/login），返回会话 sessionId。 */
    private suspend fun registerLogin(account: String, password: String, isLogin: Boolean): String {
        val path = if (isLogin) "/api/login" else "/api/register"
        val body = buildJsonObject {
            put("email", account)
            put("password", password)
        }
        val (data, status, cookies) = doPost(path, body, "")
        if (status < 200 || status >= 300) throw RuntimeException("shadowmail $path: http $status")
        val msg = ProviderUtil.str(data, "message")
        if (isLogin) {
            if (msg != "Successfull Login") throw RuntimeException("shadowmail login: $msg")
        } else {
            // 重复注册（幂等）：消息为 Email already in use 时视为账号已存在，继续走登录
            if (msg != "Successfully Registered" && msg != "Email already in use") {
                throw RuntimeException("shadowmail register: $msg")
            }
        }
        val session = sessionFrom(cookies)
        if (isLogin && session.isEmpty()) throw RuntimeException("shadowmail login: 未下发 sessionId Cookie")
        return session
    }

    /**
     * 注册账号并创建临时邮箱地址。
     * token 凭据串格式："shadowmail|<account>|<password>|<sessionId>"。
     */
    override suspend fun generate(): EmailInfo {
        val account = "${randomAccount()}@gmail.com"

        // 1) 注册（幂等：已存在同名账号则跳过）
        registerLogin(account, PW, false)
        // 2) 登录取得 sessionId
        val session = registerLogin(account, PW, true)
        // 3) 创建地址（每账号 12 槽）：SDK 层显式 Cookie 头传 sessionId
        val (data, status) = doPost("/api/new-address", buildJsonObject {}, "sessionId=$session")
        if (status < 200 || status >= 300) throw RuntimeException("shadowmail new-address: http $status")
        val address = ProviderUtil.str(data, "address").trim()
        if (address.isEmpty() || !address.endsWith("@$DOMAIN")) {
            throw RuntimeException("shadowmail new-address: 响应缺少有效地址")
        }

        // Token 持久化：account|password|sessionId（sessionId 为 uuid，无分隔符冲突）
        val token = TOKEN_PREFIX + listOf(account, PW, session).joinToString("|")
        return EmailInfo(email = address.lowercase().trim(), channel = CHANNEL, token = token)
    }

    /** 解析凭据串为 account/password/sessionId 三元组。 */
    private fun parseToken(token: String): Triple<String, String, String> {
        if (!token.startsWith(TOKEN_PREFIX)) throw RuntimeException("shadowmail: token 格式错误")
        val parts = token.removePrefix(TOKEN_PREFIX).split("|")
        if (parts.size != 3) throw RuntimeException("shadowmail: token 字段缺失")
        val (account, password, session) = parts
        if (account.isEmpty() || password.isEmpty() || session.isEmpty()) {
            throw RuntimeException("shadowmail: token 凭据字段为空")
        }
        return Triple(account, password, session)
    }

    /** 读取收件箱；会话失效时自动以凭据内 account/password 重新登录换新 sessionId。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val (account, password, session0) = parseToken(info.token)
        var session = session0
        val addr = info.email.trim()

        suspend fun doFetch(): Triple<JsonObject?, Int, List<String>> {
            val body = buildJsonObject { put("address", addr) }
            return doPost("/api/get-emails", body, "sessionId=$session")
        }

        var (data, status) = doFetch()
        // sessionId 最长 1 小时（Max-Age 3600），过期后重登录重试一次
        if (status == 401 || status == 404) {
            val newSession = registerLogin(account, password, true)
            if (newSession.isNotEmpty()) {
                session = newSession
                val retry = doFetch()
                data = retry.first
                status = retry.second
            }
        }
        if (status < 200 || status >= 300) throw RuntimeException("shadowmail get-emails: http $status")
        val msg = ProviderUtil.str(data, "message")
        if (msg != "Emails read") throw RuntimeException("shadowmail get-emails: $msg")
        val mails = ProviderUtil.arr(data, "mails") ?: return emptyList()

        return mails.filterIsInstance<JsonObject>().map { m ->
            val flat = m.toMutableMap()
            m["sender"]?.let { flat["from"] = it }
            flat["to"] = JsonPrimitive(addr)
            m["created_at"]?.let { flat["date"] = it }
            // 平台无 text/html 区分，body 为正文（默认按纯文本处理，普通化可按需互转）
            m["body"]?.let { flat["text"] = it }
            Normalize.fromJson(JsonObject(flat), addr)
        }
    }
}