package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*

/**
 * 30minemail 渠道实现（30minemail.com）。
 *
 * 官网邮箱由服务端 16 位 hex 本地名识别：
 *   GET /?generate 返回完整 HTML 页面，从中解析 <地址>@30minemail.com；
 *   本地名无效邮箱访问 messages.php 时返回 ok:false/expired:true，故必须经服务端建箱。
 * 读信 GET /messages.php?email=<完整地址>&_=<unix毫秒>，响应
 *   {"ok":true,"expired":false,"count":0,"emails":[],"expires_in":...}。
 * emails 元素字段实测：{id,from,to,subject,date,html}（html 为完整正文）。
 * 无认证、无 Cookie、无 CSRF。
 */
object ThirtyMinEmail : Provider {

    private const val CHANNEL = "30minemail"
    private const val BASE_URL = "https://30minemail.com"
    private const val DOMAIN = "30minemail.com"

    private val headers = mapOf(
        "Accept" to "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /**
     * 创建 30minemail.com 临时邮箱。
     * GET /?generate 返回 HTML 页面，从其中解析 16 位 hex 本地名地址；
     * token 复用完整地址（服务端以地址定位收件箱）。
     */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpGet("$BASE_URL/?generate", headers)
        resp.ensureSuccess()
        val page = resp.body
        val idx = page.indexOf("@$DOMAIN")
        if (idx < 0) throw RuntimeException("30minemail: 创建页面未找到邮箱地址")
        // 向前查找本地名起点：空白或 > 之后
        var start = idx
        while (start > 0 && !" \n\t>\"".contains(page[start - 1])) start--
        val local = page.substring(start, idx).trim()
        if (local.length < 8) throw RuntimeException("30minemail: 创建页面解析地址异常")
        val addr = "$local@$DOMAIN"
        return EmailInfo(email = addr, channel = CHANNEL, token = addr)
    }

    /** 读取 30minemail.com 收件箱（GET /messages.php，模拟官方轮询参数）。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val addr = info.email.trim()
        if (addr.isEmpty()) throw RuntimeException("30minemail: 邮箱地址为空")
        val ts = System.currentTimeMillis()
        val resp = ProviderUtil.httpGet(
            "$BASE_URL/messages.php?email=" + ProviderUtil.urlEncode(addr) + "&_=$ts",
            mapOf(
                "Accept" to "application/json",
                "User-Agent" to headers["User-Agent"].orEmpty(),
            ),
        )
        resp.ensureSuccess()
        val data = ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("30minemail: 收件箱响应非对象")
        val ok = (data["ok"] as? JsonPrimitive)?.booleanOrNull ?: false
        val expired = (data["expired"] as? JsonPrimitive)?.booleanOrNull ?: false
        if (!ok || expired) throw RuntimeException("30minemail: 收件箱不可用或已过期")
        val emails = ProviderUtil.arr(data, "emails") ?: return emptyList()

        return emails.filterIsInstance<JsonObject>().map { m ->
            val row = m.toMutableMap()
            // 列表元素无 to 字段时注入收件人地址
            if (!row.containsKey("to")) row["to"] = JsonPrimitive(addr)
            Normalize.fromJson(JsonObject(row), addr)
        }
    }
}