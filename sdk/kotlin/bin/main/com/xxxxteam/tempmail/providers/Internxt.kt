package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * Internxt 渠道实现（internxt.com/temporary-email，Next.js + OpenNext）。
 *
 * 与 Go 端 internxt.go 协议一致（2026-09-28 抓包实证）：
 * - 建箱 GET /api/temp-mail/create-email，读信 GET /api/temp-mail/get-inbox
 *   ?email=<e>&token=<t>，详情 GET /api/temp-mail/get-message
 *   ?email=<e>&token=<t>&messageId=<id>。create-email 仅接受 GET（POST 405）。
 * - CSRF：首次 GET /temporary-email 响应 Set-Cookie csrfSecret=... 与
 *   XSRF-TOKEN=...（均 HttpOnly）。数据接口校验请求头 csrf-token，
 *   其值必须与 Cookie 罐中 XSRF-TOKEN 一致（头=XSRF-TOKEN 值时 200；
 *   头=csrfSecret 值时恒定 500）。每个 API 响应均会刷新 XSRF-TOKEN 的
 *   Set-Cookie，故每次读信前都应从罐中重取最新值作为 csrf-token 头
 *   （实测 csrfSecret 恒定，XSRF-TOKEN 每次刷新）。
 * - 建箱响应：{"address":"<前缀>@uberip.com","token":"<十六进制>"}。
 *   get-inbox 正常返回顶层数组（空箱 []）；错误 token 返回 401
 *   {"message":"Email has expired"}。
 *
 * 会话粘性：本端无全局 Cookie 罐，object 内以 [LinkedHashMap] 维护私有会话 Cookie，
 * 请求以显式 Cookie 头回传、响应 Set-Cookie 逐次覆写；token 保存 {address,token} JSON。
 */
object Internxt : Provider {

    private const val CHANNEL = "internxt"
    private const val SITE = "https://internxt.com"
    private const val REF = "$SITE/temporary-email"
    private const val API_BASE = "$SITE/api/temp-mail"

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

    /** 从私有罐取当前 XSRF-TOKEN 值（无则空串）。 */
    private fun xsrfFromJar(): String = cookieStore.entries
        .firstOrNull { it.key == "XSRF-TOKEN" && it.value.isNotEmpty() }?.value.orEmpty()

    /**
     * 确保私有 Cookie 罐持有本域 csrfSecret 与 XSRF-TOKEN（无则 GET
     * 临时邮箱页夺取），并返回罐中最新 XSRF-TOKEN 值。每个 API 响应
     * 都会刷新 XSRF-TOKEN 的 Set-Cookie，故调用方每次请求前都应重新调用。
     *
     * @return XSRF-TOKEN 值
     */
    private suspend fun prepareXsrf(): String {
        val inJar = xsrfFromJar()
        if (inJar.isNotEmpty()) return inJar

        val resp = ProviderUtil.httpGet(REF, mapOf(
            "User-Agent" to UA,
            "Accept" to "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            "Accept-Language" to "en-US,en;q=0.9",
        ))
        updateCookies(resp)
        val xsrf = xsrfFromJar()
        if (xsrf.isEmpty()) throw RuntimeException("internxt: 未取得 XSRF-TOKEN Cookie")
        return xsrf
    }

    /**
     * 带 CSRF 头请求 internxt 数据接口（GET）。
     *
     * csrf-token 头取罐中 XSRF-TOKEN 最新值（无则通过 [prepareXsrf] 夺取）；
     * 响应 Set-Cookie（XSRF-TOKEN 每次刷新）自动落私有罐。
     *
     * @param path 接口路径（如 /create-email）
     * @param query 查询参数
     * @return HTTP 响应封装
     */
    private suspend fun apiGet(path: String, query: Map<String, String>): HttpResp {
        val csrfToken = prepareXsrf()
        val qs = query.entries.joinToString("&") { (k, v) ->
            ProviderUtil.urlEncode(k) + "=" + ProviderUtil.urlEncode(v)
        }
        val url = if (qs.isEmpty()) API_BASE + path else "$API_BASE$path?$qs"
        val h = HashMap<String, String>()
        h["User-Agent"] = UA
        h["Accept"] = "application/json, text/plain, */*"
        h["Origin"] = SITE
        h["Referer"] = REF
        h["csrf-token"] = csrfToken
        cookieHeader()?.let { h["Cookie"] = it }

        val resp = ProviderUtil.httpGet(url, h)
        updateCookies(resp)
        if (!resp.isOk) {
            throw RuntimeException("internxt: $path 失败 http ${resp.statusCode}: ${resp.body.trim()}")
        }
        return resp
    }

    /** 创建 internxt.com 临时邮箱（create-email，仅 GET），token 保存 {address,token} JSON。 */
    override suspend fun generate(): EmailInfo {
        prepareXsrf()
        val resp = apiGet("/create-email", emptyMap())
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("internxt: 解析建箱响应失败")
        val address = strOf(data, "address").trim()
        val token = strOf(data, "token").trim()
        if (address.isEmpty() || token.isEmpty() || !address.contains("@")) {
            throw RuntimeException("internxt: 创建邮箱响应缺少必要字段（address/token）")
        }
        val session = buildJsonObject {
            put("address", address)
            put("token", token)
        }.toString()
        return EmailInfo(email = address, channel = CHANNEL, token = session)
    }

    /** 获取 internxt.com 收件列表：get-inbox 顶层数组 + 逐封 get-message 拉详情。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val body = ProviderUtil.parse(info.token) as? JsonObject
            ?: throw RuntimeException("internxt: 会话凭据解析失败")
        val address = strOf(body, "address").trim()
        val token = strOf(body, "token").trim()
        if (address.isEmpty() || token.isEmpty()) {
            throw RuntimeException("internxt: 会话凭据缺少必要字段")
        }
        if (address != info.email) {
            throw RuntimeException("internxt: 会话邮箱与查询邮箱不匹配")
        }

        val inboxResp = apiGet("/get-inbox", mapOf("email" to address, "token" to token))
        val root = ProviderUtil.parse(inboxResp.body)
        val list = when {
            root == null -> JsonArray(emptyList())
            root is JsonArray -> root
            else -> throw RuntimeException("internxt: 收件箱响应不是顶层数组")
        }

        val out = ArrayList<Email>(list.size)
        for (item in list.filterIsInstance<JsonObject>()) {
            val merged = HashMap<String, Any?>()
            for ((k, v) in item) merged[k] = jsonToAny(v)

            // 列表元素携带 id 时逐封拉单封全文（含 html 渲染全文）；
            // 详情失败回退列表摘要（不中断整批）
            val mid = strOf(item, "id").ifEmpty { strOf(item, "messageId") }
            if (mid.isNotEmpty()) {
                try {
                    val detailResp = apiGet("/get-message", mapOf(
                        "email" to address, "token" to token, "messageId" to mid,
                    ))
                    val detail = ProviderUtil.parseObject(detailResp.body)
                    if (detail != null) {
                        for ((k, v) in detail) {
                            if (!merged.containsKey(k)) merged[k] = jsonToAny(v)
                        }
                    }
                } catch (_: RuntimeException) {
                    // 详情失败回退列表摘要
                }
            }
            out.add(Normalize.fromMap(merged, info.email))
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