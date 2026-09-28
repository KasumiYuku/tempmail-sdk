package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import kotlin.random.Random

/**
 * LinshiXYZ 渠道实现（linshi.xyz）。
 *
 * 无建箱请求：本地随机 6 位 hex 前缀（官网 client 即用短 id）+ @linshi.xyz。
 * 读信 GET https://linshi.xyz/api/mails/{前缀}，无邮件时返回空数组 []，
 *   有邮件时为邮件对象数组（元素含 headers{from,to,subject,date} 与 html 正文）。
 * 归一化时将 headers 平铺并注入收件人地址；非数组骨架（如 {ok:false}）整体失败，
 * 交由上层 fallback 渠道处理。
 */
object LinshiXyz : Provider {

    private const val CHANNEL = "linshi-xyz"
    private const val BASE_URL = "https://linshi.xyz"
    private const val DOMAIN = "linshi.xyz"
    private const val HEX_DIGITS = "0123456789abcdef"

    private val headers = mapOf(
        "Accept" to "application/json",
        "User-Agent" to "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
            "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36",
    )

    /** 生成本地随机 6 位 hex 前缀（与官网 client 相同格式）。 */
    private fun localName(): String {
        val sb = StringBuilder(6)
        repeat(6) { sb.append(HEX_DIGITS[Random.nextInt(HEX_DIGITS.length)]) }
        return sb.toString()
    }

    /** 创建 linshi.xyz 临时邮箱：无需服务端建箱，token 复用完整地址。 */
    override suspend fun generate(): EmailInfo {
        val addr = "${localName()}@$DOMAIN"
        return EmailInfo(email = addr, channel = CHANNEL, token = addr)
    }

    /** 读取 linshi.xyz 收件箱（GET /api/mails/{前缀}，响应为邮件对象数组）。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val addr = info.email.trim()
        if (addr.isEmpty() || !addr.contains("@")) {
            throw RuntimeException("linshi-xyz: 邮箱地址无效: \"$addr\"")
        }
        val local = addr.substringBefore("@")
        val resp = ProviderUtil.httpGet("$BASE_URL/api/mails/" + ProviderUtil.urlEncode(local), headers)
        resp.ensureSuccess()
        val list = ProviderUtil.parse(resp.body) as? JsonArray
            ?: throw RuntimeException("linshi-xyz: 收件箱响应非数组")

        return list.filterIsInstance<JsonObject>().map { m ->
            val flat = mutableMapOf<String, JsonElement>()
            // headers 对象平铺为顶层字段
            (m["headers"] as? JsonObject)?.let { flat.putAll(it) }
            flat.putAll(m)
            // 注入收件人地址（headers.to 存在时归一化优先使用）
            if (!flat.containsKey("to")) flat["to"] = JsonPrimitive(addr)
            Normalize.fromJson(JsonObject(flat), addr)
        }
    }
}