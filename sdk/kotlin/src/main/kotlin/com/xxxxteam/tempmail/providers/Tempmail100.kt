package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * TempMail100 渠道实现（tempmail100.com）。
 *
 * POST /init 建箱（空 body，响应 code/data.token（JWT），无 Cookie），
 * POST /web/generate 建随机地址（Header Authorization: <token>，响应 data.address），
 * GET /web/emails 读信列表（Header Authorization: <token>，响应 data.list[]/data.total）。
 *
 * 平台限制（实测确认，非 SDK 缺陷）：
 * 列表元素 content 恒为空字符串，详情端点对真实 token 返回 HTTP 200 + 空 body，
 * 因此正文永久不可得，本渠道客观为「列表-only」：subject/fromAddress/fromName/
 * timestamp/read 可正确输出，正文恒为空属平台限制。
 */
object Tempmail100 : Provider {

    private const val CHANNEL = "tempmail100"
    private const val BASE_URL = "https://tempmail100.com"

    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /** 设置 tempmail100 请求的通用请求头（前端使用 Authorization: <token> 不带 Bearer）。 */
    private fun authHeaders(token: String): Map<String, String> {
        return if (token.isEmpty()) headers else headers + ("Authorization" to token)
    }

    /** 创建 tempmail100.com 临时邮箱：POST /init 取 JWT，再 POST /web/generate 创建地址。 */
    override suspend fun generate(): EmailInfo {
        // 第一步：初始化取得 token
        val resp = ProviderUtil.httpPost("$BASE_URL/init", headers = authHeaders(""))
        resp.ensureSuccess()
        val init = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("tempmail100: 初始化响应非对象")
        val code = ProviderUtil.str(init, "code")
        val token = (init["data"] as? JsonObject)?.let { ProviderUtil.str(it, "token").trim() }.orEmpty()
        if (code != "0" || token.isEmpty()) {
            throw RuntimeException("tempmail100: 初始化响应异常")
        }

        // 第二步：创建随机地址（Authorization: <token> 不带 Bearer）
        val resp2 = ProviderUtil.httpPost("$BASE_URL/web/generate", headers = authHeaders(token))
        resp2.ensureSuccess()
        val gen = ProviderUtil.parseObject(resp2.body)
            ?: throw RuntimeException("tempmail100: 创建地址响应非对象")
        val genCode = ProviderUtil.str(gen, "code")
        val address = (gen["data"] as? JsonObject)?.let { ProviderUtil.str(it, "address").trim() }.orEmpty()
        if (genCode != "0" || address.isEmpty() || !address.contains("@")) {
            throw RuntimeException("tempmail100: 创建地址响应异常")
        }
        return EmailInfo(email = address, channel = CHANNEL, token = token)
    }

    /** 获取 tempmail100.com 邮件列表（GET /web/emails，Authorization 头为裸 token）。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val addr = info.email.trim()
        val token = info.token.trim()
        val resp = ProviderUtil.httpGet("$BASE_URL/web/emails", authHeaders(token))
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("tempmail100: 邮件列表响应非对象")
        if (ProviderUtil.str(data, "code") != "0") {
            throw RuntimeException("tempmail100: 获取邮件列表响应异常: ${ProviderUtil.str(data, "message")}")
        }
        val list = (data["data"] as? JsonObject)?.let { ProviderUtil.arr(it, "list") }
            ?: return emptyList()

        return list.filterIsInstance<JsonObject>().map { m ->
            Normalize.fromMap(normalizeItem(m, addr), addr)
        }
    }

    /**
     * 将 /web/emails 列表元素归一为统一邮件结构。
     * fromName+fromAddress 组合为 "Name <address>" 填入 from；
     * content 平台恒为空（正文端点不存在），如实留空；read 为布尔已读标记。
     */
    private fun normalizeItem(item: JsonObject, email: String): Map<String, Any?> {
        val fromName = ProviderUtil.str(item, "fromName").trim()
        var fromAddress = ProviderUtil.str(item, "fromAddress").trim()
        if (fromName.isNotEmpty() && !fromName.equals(fromAddress, ignoreCase = true) &&
            fromAddress.contains("@")
        ) {
            fromAddress = "$fromName <$fromAddress>"
        }

        return mapOf(
            "id" to ProviderUtil.str(item, "uuid"),
            "from" to fromAddress,
            "to" to ProviderUtil.str(item, "toAddress"),
            "subject" to ProviderUtil.str(item, "subject"),
            "content" to ProviderUtil.str(item, "content"),
            "timestamp" to strOf(item["timestamp"]),
            "isRead" to readOf(item["read"]),
        )
    }

    /** 将接口字段值安全转换为字符串（非标量返回 null）。 */
    private fun strOf(v: JsonElement?): String? {
        if (v == null || v is JsonNull) return null
        return if (v is JsonPrimitive) v.content else null
    }

    /** 将 read 字段归一为布尔已读标记，兼容 bool / 数字(0|1) / string("true"|"1")。 */
    private fun readOf(v: JsonElement?): Boolean {
        val p = v as? JsonPrimitive ?: return false
        p.booleanOrNull?.let { return it }
        if (p.content == "1" || p.content.equals("true", ignoreCase = true)) return true
        return p.content.toDoubleOrNull()?.let { it != 0.0 } ?: false
    }
}