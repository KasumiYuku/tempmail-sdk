package com.xxxxteam.tempmail.providers

import com.xxxxteam.tempmail.Email
import com.xxxxteam.tempmail.EmailInfo
import com.xxxxteam.tempmail.Normalize
import com.xxxxteam.tempmail.Provider
import kotlinx.serialization.json.*
import java.time.Instant

/**
 * TempMailGG 渠道实现（temp-mail.gg，Laravel + Livewire v3）。
 *
 * 与 Go 端 temp_mail_gg.go 协议一致（2026-09-27 抓包实证）：
 * - GET 首页取 data-csrf 令牌与初始 wire:snapshot（属性内 JSON 为双转义态，需反转义一层）；
 * - POST /livewire/update 是唯一边界：JSON body 顶层 _token（=data-csrf）+
 *   components[0]{snapshot, updates:{}, calls:[{path:"",method:"<call>",params:[...]}]}；
 *   calls 为空即平台 20 秒轮询刷信形态；generateEmail/selectEmail 分别建箱/点开详情；
 *   Laravel 每次 update 轮换会话 Cookie，同值重放 419，必须以响应 Set-Cookie 覆写后续请求。
 *
 * 本端无全局 Cookie 罐，object 内以 [LinkedHashMap] 维护私有会话 Cookie，
 * 请求以显式 Cookie 头回传、响应 Set-Cookie 逐次覆写；token 内保存
 * {email, csrf, snapshot} 凭据串。邮箱约 30 分钟无活动过期。
 */
object TempMailGg : Provider {

    private const val CHANNEL = "temp-mail-gg"
    private const val BASE_URL = "https://temp-mail.gg"

    /** Token 前缀，用于识别本渠道会话凭据串。 */
    private const val TOKEN_PREFIX = "temp-mail-gg|"

    /** 固定浏览器 UA。 */
    private const val UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " +
        "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

    /** 会话 Cookie 私罐：键为 cookie 名（LinkedHashMap 保序，后写覆盖同名键）。 */
    private val cookieStore = LinkedHashMap<String, String>()

    /** wire:snapshot 属性值正则（HTML 属性内经 &quot; 转义的 JSON 串）。 */
    private val SNAPSHOT_RE = Regex("""wire:snapshot="([^"]*)"""")
    /** data-csrf 属性值正则。 */
    private val DATA_CSRF_RE = Regex("""data-csrf="([^"]*)"""")
    /** 相对时间正则（"N seconds/minutes/hours/days ago"）。 */
    private val RELATIVE_RE = Regex("""^\s*(\d+)\s+(second|minute|hour|day)s?\s+ago\s*$""", RegexOption.IGNORE_CASE)
    /** 列表条目容器正则（wire:click 数字 id div 块）。 */
    private val LIST_BLOCK_RE = Regex("""(?s)<div\s+wire:click="selectEmail\((\d+)\)"[\s\S]*?</div>""")

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

    /** HTML 属性值反转义一层（&quot; -> " 等）。 */
    private fun unescapeAttr(s: String): String =
        s.replace("&quot;", "\"").replace("&#039;", "'").replace("&apos;", "'")
            .replace("&lt;", "<").replace("&gt;", ">").replace("&amp;", "&")

    /**
     * 调用 livewire/update（响应 Set-Cookie 即时覆写私有 Cookie 罐）。
     *
     * @param snapshot 当前组件快照 JSON 串
     * @param csrf data-csrf 令牌（顶层 _token）
     * @param method 组件方法（"" 表示纯轮询）；非空时 params 为方法参数
     * @param params 方法参数（数字 id 列表，无参为空列表）
     * @return update 响应 JSON；419 或非 2xx 抛异常
     */
    private suspend fun update(snapshot: String, csrf: String, method: String, params: List<Int> = emptyList()): JsonObject {
        val payload = buildJsonObject {
            put("_token", csrf)
            putJsonArray("components") {
                addJsonObject {
                    put("snapshot", snapshot)
                    put("updates", JsonObject(emptyMap()))
                    putJsonArray("calls") {
                        if (method.isNotEmpty()) {
                            addJsonObject {
                                put("path", "")
                                put("method", method)
                                putJsonArray("params") { params.forEach { add(it) } }
                            }
                        }
                    }
                }
            }
        }.toString()

        val h = HashMap<String, String>()
        h["User-Agent"] = UA
        h["Accept"] = "text/html, application/xhtml+xml"
        h["Accept-Language"] = "en-US,en;q=0.9"
        h["Content-Type"] = "application/json"
        h["X-Livewire"] = ""
        h["X-Requested-With"] = "XMLHttpRequest"
        h["Origin"] = BASE_URL
        h["Referer"] = "$BASE_URL/"
        cookieHeader()?.let { h["Cookie"] = it }

        val resp = ProviderUtil.httpPost("$BASE_URL/livewire/update", payload, "application/json", h)
        updateCookies(resp)
        if (resp.statusCode == 419) {
            throw RuntimeException("temp-mail-gg: livewire 会话过期（419），请重新 Generate")
        }
        if (!resp.isOk) throw RuntimeException("temp-mail-gg: livewire/update http ${resp.statusCode}")
        return ProviderUtil.parseObject(resp.body)
            ?: throw RuntimeException("temp-mail-gg: 解析 update 响应失败")
    }

    /** 取 update 响应 components[0]，缺失抛异常。 */
    private fun firstComponent(resp: JsonObject): JsonObject {
        val comps = ProviderUtil.arr(resp, "components")
        return comps?.getOrNull(0) as? JsonObject
            ?: throw RuntimeException("temp-mail-gg: update 响应异常（components 缺失）")
    }

    /** 创建 temp-mail.gg 临时邮箱（约 30 分钟无活动过期）。 */
    override suspend fun generate(): EmailInfo {
        val resp = ProviderUtil.httpGet(BASE_URL, mapOf(
            "User-Agent" to UA,
            "Accept" to "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            "Accept-Language" to "en-US,en;q=0.9",
        ))
        updateCookies(resp)
        resp.ensureSuccess()

        val csrf = DATA_CSRF_RE.find(resp.body)?.groupValues?.get(1).orEmpty()
        val snapRaw = SNAPSHOT_RE.find(resp.body)?.groupValues?.get(1).orEmpty()
        if (csrf.isEmpty() || snapRaw.isEmpty()) {
            throw RuntimeException("temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱")
        }
        val snapshot = unescapeAttr(snapRaw)

        val upd = update(snapshot, csrf, "generateEmail")
        val snap2 = ProviderUtil.str(firstComponent(upd), "snapshot")
        if (snap2.isEmpty()) {
            throw RuntimeException("temp-mail-gg: generateEmail 响应异常（components 缺失）")
        }
        val email = (ProviderUtil.parse(snap2) as? JsonObject)
            ?.let { strOf(it, "email") }?.trim().orEmpty()
        if (email.isEmpty()) {
            throw RuntimeException("temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录配额（每时段 5 个）")
        }

        val token = TOKEN_PREFIX + buildJsonObject {
            put("email", email)
            put("csrf", csrf)
            put("snapshot", snap2)
        }.toString()
        return EmailInfo(email = email, channel = CHANNEL, token = token)
    }

    /** 获取 temp-mail.gg 收件列表：轮询取列表 -> 逐封 selectEmail 拉详情。 */
    override suspend fun getEmails(info: EmailInfo): List<Email> {
        val email = info.email.trim()
        if (email.isEmpty()) throw RuntimeException("temp-mail-gg: 邮箱为空，请重新 Generate")
        val token = info.token.trim()
        if (!token.startsWith(TOKEN_PREFIX)) {
            throw RuntimeException("temp-mail-gg: 凭据串前缀不符，请重新 Generate")
        }
        val body = ProviderUtil.parse(token.substring(TOKEN_PREFIX.length)) as? JsonObject
            ?: throw RuntimeException("temp-mail-gg: 解析凭据串失败")
        val sessMail = strOf(body, "email").trim()
        val sessSnap = strOf(body, "snapshot").trim()
        if (sessMail.isEmpty() || sessSnap.isEmpty()) {
            throw RuntimeException("temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate")
        }
        if (!sessMail.equals(email, ignoreCase = true)) {
            throw RuntimeException("temp-mail-gg: 邮箱与凭据不匹配")
        }

        // 轮询刷新（calls 为空 = 平台 20s 自动刷新形态）
        val poll = update(sessSnap, strOf(body, "csrf"), "")
        val c0 = firstComponent(poll)
        if (snapshotEmailSwitched(c0, email)) {
            throw RuntimeException("temp-mail-gg: 会话已被切换")
        }
        val effects = c0["effects"] as? JsonObject ?: JsonObject(emptyMap())
        val htmlBlock = ProviderUtil.str(effects, "html").trim()
        if (htmlBlock.isEmpty()) {
            throw RuntimeException("temp-mail-gg: 轮询响应无 effects.html，会话可能已失效")
        }

        var latestSnap = ProviderUtil.str(c0, "snapshot").trim().ifEmpty { sessSnap }
        val rows = parseList(htmlBlock)
        if (rows.isEmpty()) return emptyList()

        val out = ArrayList<Email>(rows.size)
        for (row in rows) {
            // 逐封拉详情正文；失败回退列表字段（不中断整批）
            val upd = update(latestSnap, strOf(body, "csrf"), "selectEmail", listOf(row.id))
            val comp = firstComponent(upd)
            val newSnap = ProviderUtil.str(comp, "snapshot").trim()
            if (newSnap.isNotEmpty()) latestSnap = newSnap
            val eff = comp["effects"] as? JsonObject ?: JsonObject(emptyMap())
            var from = row.from
            var subject = row.subject
            var text = row.preview
            parseDetail(ProviderUtil.str(eff, "html")).let { d ->
                if (d.from.isNotEmpty()) from = d.from
                if (d.subject.isNotEmpty()) subject = d.subject
                if (d.text.isNotEmpty()) text = d.text
            }

            val html = if (text.isNotEmpty()) {
                "<html><body><pre>" + htmlEscapeMin(text) + "</pre></body></html>"
            } else {
                ""
            }
            out.add(
                Normalize.fromMap(
                    mapOf(
                        "id" to row.id.toString(),
                        "from" to from,
                        "to" to email,
                        "subject" to subject,
                        "body" to text.ifEmpty { subject },
                        "html" to html,
                        "received_at" to parseRelative(row.when0),
                    ),
                    email,
                ),
            )
        }
        return out
    }

    /** 轮询响应快照 data.email 非空且与请求邮箱不符时返回 true（会话被切换）。 */
    private fun snapshotEmailSwitched(c0: JsonObject, email: String): Boolean {
        val snap = ProviderUtil.str(c0, "snapshot")
        if (snap.isEmpty()) return false
        val current = (ProviderUtil.parse(snap) as? JsonObject)
            ?.let { strOf(it, "email") }?.trim().orEmpty()
        return current.isNotEmpty() && !current.equals(email, ignoreCase = true)
    }

    /** 列表行（从 effects.html 解析的 Inbox 条目）。 */
    private data class Row(val id: Int, val from: String = "", val subject: String = "", val preview: String = "", val when0: String = "", val text: String = "")

    /**
     * 解析轮询 effects.html 的 Inbox 条目：
     * wire:click="selectEmail(<数字id>)" 容器内 h3(class 含 font-semibold)=from、
     * p(class 含 text-zinc-300)=subject、p(class 含 line-clamp-2)=preview、
     * span(class 含 text-xs)=when。
     */
    private fun parseList(htmlBlock: String): List<Row> {
        val out = ArrayList<Row>()
        for (m in LIST_BLOCK_RE.findAll(htmlBlock)) {
            val id = m.groupValues[1].toIntOrNull() ?: continue
            out.add(
                Row(
                    id = id,
                    from = tagged(m.value, "h3", "font-semibold"),
                    subject = tagged(m.value, "p", "text-zinc-300"),
                    preview = tagged(m.value, "p", "line-clamp-2"),
                    when0 = tagged(m.value, "span", "text-xs"),
                ),
            )
        }
        return out
    }

    /** 按标签名 + class 包含关系（可选）提取首个匹配元素的文本内容。 */
    private fun tagged(html: String, tag: String, cls: String): String {
        val clsCond = if (cls.isEmpty()) "" else """(?=[^>]*\b$cls\b)"""
        val open = Regex("""(?is)<$tag$clsCond[^>]*>""").find(html) ?: return ""
        val rest = html.substring(open.range.last + 1)
        val close = Regex("""(?is)</$tag\s*>""").find(rest) ?: return ""
        return htmlToText(rest.substring(0, close.range.first)).trim()
    }

    /**
     * 解析 selectEmail 详情视图（模态框）：
     * h3(class 含 text-xl)=subject；span 文本前缀 "From:" = from；
     * div(class 含 activeTab === 'text')=text（去标签）。
     */
    private fun parseDetail(htmlBlock: String): Row {
        var subject = ""
        var from = ""
        var text = ""
        // h3 文本可以直接取（无嵌套块元素），span/div 需要截取到闭合标签
        val h3Re = Regex("""(?is)<h3(?=[^>]*\btext-xl\b)[^>]*>([\s\S]*?)</h3>""")
        h3Re.find(htmlBlock)?.let { subject = htmlToText(it.groupValues[1]).trim() }
        val spanRe = Regex("""(?is)<span[^>]*>([\s\S]*?)</span>""")
        for (m in spanRe.findAll(htmlBlock)) {
            val t = htmlToText(m.groupValues[1]).trim()
            if (t.startsWith("From:")) from = t.removePrefix("From:").trim()
        }
        val divRe = Regex("""(?is)<div(?=[^>]*x-show="[^"]*activeTab === 'text'[^"]*")[^>]*>([\s\S]*?)</div>""")
        divRe.find(htmlBlock)?.let { text = htmlToText(it.groupValues[1]).trim() }
        return Row(id = 0, from = from, subject = subject, text = text)
    }

    /** 相对时间解析为 ISO 时间；失败返回当前时间。 */
    private fun parseRelative(s: String): String {
        val m = RELATIVE_RE.find(s.trim()) ?: return Instant.now().toString()
        val n = m.groupValues[1].toLongOrNull() ?: return Instant.now().toString()
        val unit = when (m.groupValues[2].lowercase()) {
            "second" -> 1L
            "minute" -> 60L
            "hour" -> 3600L
            "day" -> 86400L
            else -> return Instant.now().toString()
        }
        return Instant.now().minusSeconds(n * unit).toString()
    }

    // ==================== HTML / JSON 工具 ====================

    private val SCRIPT_RE = Regex("""(?is)<(script|style)[\s\S]*?</\1>""")
    private val TAG_RE = Regex("""(?s)<[^>]+>""")

    /** HTML 片段转纯文本（去 script/style/标签、反转义、压缩空白）。 */
    private fun htmlToText(src: String): String {
        val cleaned = TAG_RE.replace(SCRIPT_RE.replace(src, " "), " ")
        return cleaned.split(Regex("""\s+""")).filter { it.isNotEmpty() }.joinToString(" ")
    }

    /** 最小 HTML 转义（仅 & < >）。 */
    private fun htmlEscapeMin(s: String): String =
        s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")

    /** 从 JsonObject 取字符串（原始 JsonPrimitive 直接取 content），缺失返回空串。 */
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