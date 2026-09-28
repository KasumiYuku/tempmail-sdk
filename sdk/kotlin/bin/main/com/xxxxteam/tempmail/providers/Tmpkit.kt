package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Tmpkit 渠道实现（tmpkit.com，Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）。
 *
 * 与 Go 端 tmpkit.go 协议一致（2026-09-28 抓包实证）：
 * - POST /api/rpc/tempmail/initSession，body 为 tRPC 包裹 {"json":{}}，免凭据；
 *   响应 {"json":{"session":{sessionId,email,createdAt,expiresAt,extendedCount},
 *   "availableDomains":["tmpkit.com"]}}，同时 Set-Cookie: tempmail_session=sessionId
 *   （Max-Age=3600，与 sessionId 同值）。
 * - POST /api/rpc/tempmail/getEmails，body {"json":{"offset":0,"limit":20}}，
 *   需带 tempmail_session Cookie；列表元素键为 mailId/from/subject/excerpt/
 *   date/timestamp/hasAttach/isRead（无 id 键）。
 * - POST /api/rpc/tempmail/getEmailDetail，body {"json":{"mailId":数字}}，
 *   mailId 为数字（字符串会 zod 400）；响应为单封详情对象（body 为含 <br>
 *   的纯文本正文）。
 *
 * 会话粘性：本端无全局 Cookie 罐，object 内以 [LinkedHashMap] 维护私有会话 Cookie，
 * 请求以显式 Cookie 头回传、响应 Set-Cookie 逐次覆写；token 保存
 * tempMailSession=<sessionId>，读信前先覆写对域 Cookie 防串箱。
 */
object Tmpkit : Provider {

    private const val CHANNEL = "tmpkit"
    private const val BASE_URL = "https://tmpkit.com"
    private const val RPC_PREFIX = "$BASE_URL/api/rpc/tempmail"

    /** 固定浏览器 UA（与 Go 端 tls-client 指纹一致形态）。 */
    private const val UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
        "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

    /** 会话 Cookie 私罐：键为 cookie 名（LinkedHashMap 保序，后写覆盖同名键）。 */
    private val cookieStore = LinkedHashMap<String, String>()

    /** 将私有 Cookie 罐拼为 "k=v; k2=v2" 字符串，罐空返回 null。 */
    private fun cookieHeader(): String? = if (cookieStore.isEmpty()) {
        null
    } else {
        cookieStore.entries.joinToString("; ") { "${it.key}=${it.value}" }
    }

    /** 用响应 Set-Cookie 逐次覆写私有 Cookie 罐。 */
    private fun updateCookies(resp: HttpResp) {
        for (raw in resp.setCookies) {
            val pair = raw.substringBefore(';').trim()
            val eq = pair.indexOf('=')
            if (eq > 0) cookieStore[pair.substring(0, eq).trim()] = pair.substring(eq + 1).trim()
        }
    }

    /**
     * 对 tmpkit 发起 rpc 调用（body 为 tRPC 包裹 {"json":<reqBody>}）。
     *
     * 与前端 tRPC 客户端逐项对齐：Content-Type application/json、
     * Accept 为通配值、Referer https://tmpkit.com/en、Origin https://tmpkit.com；
     * Cookie 由私有罐提供（免验证 sessionId 先覆写进罐）。
     *
     * @param procedure rpc 过程名（initSession / getEmails / getEmailDetail）
     * @param reqBody 过程参数对象
     * @return 响应外层 {"json":{...}} 的 json 载荷
     */
    private suspend fun rpc(procedure: String, reqBody: JsonObject): JsonObject {
        val payload = buildJsonObject { put("json", reqBody) }.toString()
        val h = HashMap<String, String>()
        h["User-Agent"] = UA
        h["Accept"] = "*/*"
        h["Content-Type"] = "application/json"
        h["Origin"] = BASE_URL
        h["Referer"] = "$BASE_URL/en"
        cookieHeader()?.let { h["Cookie"] = it }

        val resp = ProviderUtil.httpPost("$RPC_PREFIX/$procedure", payload, "application/json", h)
        updateCookies(resp)
        if (!resp.isOk) {
            throw RuntimeException("tmpkit: $procedure 失败 http ${resp.statusCode}: ${resp.body.trim()}")
        }
        val outer = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("tmpkit: 解析 $procedure 响应失败")
        val json = outer["json"] as? JsonObject
            ?: throw RuntimeException("tmpkit: $procedure 响应缺 json 载荷")
        return json
    }

    /** 创建 tmpkit.com 临时邮箱（initSession），token 约定为 tempMailSession=<sessionId>。 */
    override suspend fun generate(): EmailInfo {
        val data = rpc("initSession", JsonObject(emptyMap()))
        val sess = data["session"] as? JsonObject
            ?: throw RuntimeException("tmpkit: 创建会话响应缺 session 字段")
        val email = strOf(sess, "email").trim()
        val sessionId = strOf(sess, "sessionId").trim()
        if (email.isEmpty() || sessionId.isEmpty() || !email.contains("@")) {
            throw RuntimeException("tmpkit: 创建会话响应缺少必要字段（email/sessionId）")
        }
        return EmailInfo(email = email, channel = CHANNEL, token = "tempMailSession=$sessionId")
    }

    /**
     * 获取 tmpkit.com 邮件列表：getEmails（offset 0 / limit 20）取摘要，
     * 逐封 getEmailDetail 拉详情并并入摘要（详情失败回退列表摘要）；
     * getEmails 返回的 session.email 与收信邮箱不符时报错，防止会话被覆盖。
     */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val email = info.email.trim()
        var mailbox = info.token.trim()
        if (mailbox.startsWith("tempMailSession=")) {
            mailbox = mailbox.removePrefix("tempMailSession=").trim()
        }
        if (mailbox.isEmpty()) throw RuntimeException("tmpkit: 会话 token 为空")
        // 覆写对域会话 Cookie，防全局罐被并行会话覆盖后串箱
        cookieStore["tempmail_session"] = mailbox

        val data = rpc("getEmails", buildJsonObject {
            put("offset", 0)
            put("limit", 20)
        })

        // 会话指向校验：getEmails 响应自带 session.email（无 Cookie 时
        // session 为 null，同样视为会话失效）
        val sess = data["session"] as? JsonObject
        if (sess != null) {
            val got = strOf(sess, "email").trim()
            if (got.isNotEmpty() && got != email) {
                throw RuntimeException("tmpkit: 会话邮箱不匹配（响应 $got，请求 $email）")
            }
        } else {
            throw RuntimeException("tmpkit: 会话已失效（getEmails 返回空会话）")
        }

        val emails = data["emails"] as? JsonArray
            ?: throw RuntimeException("tmpkit: 邮件列表响应缺 emails 字段")

        val out = ArrayList<Email>(emails.size)
        for (item in emails.filterIsInstance<JsonObject>()) {
            val merged = HashMap<String, Any?>()
            for ((k, v) in item) merged[k] = jsonToAny(v)

            // mailId 为数字：合法时逐封拉详情（字符串会 zod 400，跳过详情）
            val mailId = (item["mailId"] as? JsonPrimitive)?.longOrNull ?: 0L
            if (mailId > 0) {
                try {
                    val detail = rpc("getEmailDetail", buildJsonObject { put("mailId", mailId) })
                    for ((k, v) in detail) {
                        if (k == "session" || k == "emails" || k == "total" || k == "error") continue
                        merged[k] = jsonToAny(v)
                    }
                } catch (_: RuntimeException) {
                    // 详情失败回退列表摘要
                }
            }
            out.add(Normalize.fromMap(merged, email))
        }
        return out
    }

    /** JsonElement 容错转原生值（对象转 Map、数组转 List、标量转基本类型）。 */
    private fun jsonToAny(v: JsonElement): Any? = when (v) {
        is JsonNull -> null
        is JsonObject -> v.entries.associate { it.key to jsonToAny(it.value) }
        is JsonArray -> v.map { jsonToAny(it) }
        is JsonPrimitive -> if (v is JsonNull) null else if (v.isString) v.content
        else v.booleanOrNull ?: v.longOrNull ?: v.doubleOrNull
    }

    /** 从 JsonObject 取字符串（数字/布尔自动转文本），缺失返回空串。 */
    private fun strOf(m: JsonObject, vararg keys: String): String {
        for (key in keys) {
            val v = m[key] ?: continue
            if (v is JsonNull) continue
            if (v is JsonPrimitive) return v.content.trim().ifEmpty { v.content }
            return v.toString()
        }
        return ""
    }
}