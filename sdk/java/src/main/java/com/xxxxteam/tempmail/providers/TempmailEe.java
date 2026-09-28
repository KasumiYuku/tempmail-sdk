package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.xxxxteam.tempmail.Email;
import com.xxxxteam.tempmail.EmailInfo;
import com.xxxxteam.tempmail.HttpResult;
import com.xxxxteam.tempmail.HttpClient;
import com.xxxxteam.tempmail.Json;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Pattern;

/**
 * TempMail.ee 渠道 — https://tempmail.ee
 *
 * <p>出口风控实证结论：平台读信端 /api/mails 的 403 Access denied 不是 TLS 指纹或
 * 请求头顺序问题，而是「会话 Cookie 绑定校验」：change（换箱）返回的 Set-Cookie 中
 * temp_mail_session（形如 tmapi_sess_xxx）与 temp_email 共同构成读信凭据；带齐
 * temp_email + temp_mail_session 即 200。本类在同一事务内完成
 * change → 提取 Set-Cookie → 读信，并用显式 Cookie 请求头逐请求携带会话凭据
 * （Java 的 HttpClient 默认无 Cookie 罐，天然实现会话隔离）。</p>
 *
 * <p>已验证通行配方：POST /api/mailbox/change 若不带 sec-ch-ua 头会被平台拒绝
 * （403 Browser request required），带齐即 200，并下发会话 Cookie；
 * 建箱同时提交浏览器指纹 browserIntegrity（与官方前端一致）。</p>
 */
public final class TempmailEe {

    private static final String BASE_URL = "https://tempmail.ee";

    /** 与 sec-ch-ua 品牌版本一致的固定 UA（Chrome 154 / Linux）。 */
    private static final String UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36"
            + " (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /** Token 前缀，用于识别本渠道会话凭据串。 */
    private static final String TOKEN_PREFIX = "tempmail-ee|";

    private static final String CHANNEL = "tempmail-ee";

    /** 去 script 正则（DOTALL 语义）。 */
    private static final Pattern SCRIPT_RE = Pattern.compile("(?is)<script[\\s\\S]*?</script>");

    /** 去标签正则。 */
    private static final Pattern TAG_RE = Pattern.compile("(?s)<[^>]+>");

    /** 空白压缩正则。 */
    private static final Pattern WS_RE = Pattern.compile("\\s+");

    private TempmailEe() {
    }

    /**
     * 建箱提交的浏览器指纹（与官方前端一致）。
     *
     * @return 浏览器完整性标志字典
     */
    private static Map<String, Object> browserIntegrity() {
        Map<String, Object> integrity = new LinkedHashMap<>();
        integrity.put("webdriver", false);
        integrity.put("languagesMissing", false);
        integrity.put("languageMissing", false);
        integrity.put("pluginsMissing", false);
        integrity.put("pluginsUndefined", false);
        integrity.put("outerSizeMissing", false);
        integrity.put("innerSizeMissing", false);
        integrity.put("screenMissing", false);
        integrity.put("screenDepthMissing", false);
        integrity.put("timezoneMissing", false);
        integrity.put("timezoneOffsetMissing", false);
        integrity.put("userAgentDataPresent", true);
        integrity.put("userAgentMissing", false);
        integrity.put("platformClass", "Linux");
        integrity.put("mobile", false);
        integrity.put("collectionFailed", false);
        return integrity;
    }

    /**
     * 设置浏览器特征安全头（同站 fetch 全套）。
     *
     * @param withSecCh 是否携带 sec-ch-ua 三件套（change 必须，平台据此放行）
     */
    private static void setBrowserHeaders(Map<String, String> headers, boolean withSecCh) {
        headers.put("Accept", "application/json");
        headers.put("Content-Type", "application/json");
        headers.put("X-Requested-With", "XMLHttpRequest");
        headers.put("Origin", BASE_URL);
        headers.put("Referer", BASE_URL + "/");
        headers.put("Sec-Fetch-Site", "same-origin");
        headers.put("Sec-Fetch-Mode", "cors");
        headers.put("Sec-Fetch-Dest", "empty");
        headers.put("Accept-Language", "en-US,en;q=0.9");
        if (withSecCh) {
            headers.put("Sec-Ch-Ua",
                    "\"Chromium\";v=\"154\", \"Google Chrome\";v=\"154\", \"Not.A/Brand\";v=\"99\"");
            headers.put("Sec-Ch-Ua-Mobile", "?0");
            headers.put("Sec-Ch-Ua-Platform", "\"Linux\"");
        }
    }

    /**
     * 从 change 响应提取会话 Cookie 键值对（temp_email / temp_mail_session）。
     *
     * @param resp change 响应
     * @return [temp_email, temp_mail_session]
     */
    private static String[] cookieFromResponse(HttpResult resp) {
        String email = "";
        String session = "";
        for (String sc : resp.getSetCookies()) {
            String kv = sc;
            int semicolon = sc.indexOf(';');
            if (semicolon > 0) {
                kv = sc.substring(0, semicolon);
            }
            if (kv.startsWith("temp_email=")) {
                email = kv.substring("temp_email=".length());
            } else if (kv.startsWith("temp_mail_session=")) {
                session = kv.substring("temp_mail_session=".length());
            }
        }
        return new String[]{email, session};
    }

    /**
     * 由邮箱与 temp_mail_session 组装渠道内部凭据串。
     *
     * @param email   邮箱地址
     * @param session 会话 Cookie
     * @return 内部凭据串
     */
    private static String tokenBuild(String email, String session) {
        return TOKEN_PREFIX + "temp_email=" + email + "; temp_mail_session=" + session;
    }

    /**
     * 解析读信凭据：从 token 中提取 temp_mail_session，并以请求邮箱为准重拼 Cookie，防止凭据与邮箱错配。
     *
     * @param token 建箱时下发的渠道凭据串
     * @param email 请求读取的邮箱
     * @return 显式 Cookie 请求头；凭据无效返回 null
     */
    private static String parseToken(String token, String email) {
        if (token == null || !token.startsWith(TOKEN_PREFIX)) {
            return null;
        }
        String cred = token.substring(TOKEN_PREFIX.length());
        String session = "";
        for (String part : cred.split(";")) {
            String kv = part.trim();
            if (kv.startsWith("temp_mail_session=")) {
                session = kv.substring("temp_mail_session=".length());
            }
        }
        if (session.isEmpty()) {
            return null;
        }
        // 会话绑定邮箱：以请求邮箱为准重拼 cookie，防止凭据与邮箱错配
        return "temp_email=" + email + "; temp_mail_session=" + session;
    }

    /**
     * 创建 tempmail.ee 临时邮箱：GET / 面熟首访 → POST /api/mailbox/change 换新邮箱
     * → 提取 Set-Cookie 中的 temp_mail_session，随 token 透传给读信。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        // 步骤 1：GET / 建立 Cookie 会话（面熟首访）
        Map<String, String> bootHeaders = new LinkedHashMap<>();
        bootHeaders.put("Accept",
                "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8");
        bootHeaders.put("User-Agent", UA);
        try {
            HttpClient.get(BASE_URL, bootHeaders);
        } catch (RuntimeException ignored) {
            // 首访失败不阻断换箱请求
        }

        // 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua）
        Map<String, Object> changeBody = new LinkedHashMap<>();
        changeBody.put("turnstileToken", null);
        changeBody.put("browserIntegrity", browserIntegrity());
        Map<String, String> changeHeaders = new LinkedHashMap<>();
        setBrowserHeaders(changeHeaders, true);

        HttpResult changeResp = HttpClient.post(BASE_URL + "/api/mailbox/change",
                Json.serialize(changeBody), "application/json", changeHeaders);
        JsonObject change = Json.parseObject(changeResp.getBody());
        if (change == null) {
            throw new RuntimeException("tempmail-ee: 解析 change 响应失败（status "
                    + changeResp.getStatusCode() + "）");
        }
        String newEmail = Json.str(change, "newEmail").trim();
        if (!"true".equals(Json.str(change, "success")) || newEmail.isEmpty()) {
            throw new RuntimeException("tempmail-ee: 建箱失败: "
                    + changeResp.getBody().trim() + "（status " + changeResp.getStatusCode() + "）");
        }

        // 步骤 3：从 Set-Cookie 接管会话凭据，供读信校验使用
        String[] cookies = cookieFromResponse(changeResp);
        String cookieEmail = cookies[0];
        String session = cookies[1];
        String email = newEmail;
        // 以防万一以 Cookie 为准（Cookie temp_email 为空时保持响应体邮箱）
        if (!cookieEmail.isEmpty()) {
            email = cookieEmail;
        }
        if (session.isEmpty()) {
            throw new RuntimeException("tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信");
        }

        String expiresAt = Json.str(change, "expiresAt").trim();
        Long expiresMs = null;
        if (!expiresAt.isEmpty()) {
            try {
                expiresMs = java.time.OffsetDateTime.parse(expiresAt.replace("Z", "+00:00"))
                        .toInstant().toEpochMilli();
            } catch (RuntimeException ignored) {
                // 保留 null
            }
        }
        return new EmailInfo(CHANNEL, email, tokenBuild(email, session), expiresMs, null);
    }

    /**
     * 读取 tempmail.ee 收件箱。凭据来自 Generate 时从 change 响应提取的
     * temp_mail_session，逐请求以显式 Cookie 头携带；列表只含元数据，
     * 正文逐封 GET /api/mails/{id} 拉取详情（content 为 MIME multipart 原文，
     * HTML 实体已转义），解析后写入 text/html。
     *
     * @param token 建箱时下发的渠道凭据串
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("tempmail-ee: 邮箱为空");
        }
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("tempmail-ee: token 为空");
        }

        // 解析会话凭据；无有效 temp_mail_session 无法通过平台会话校验
        String cookie = parseToken(tok, addr);
        if (cookie == null) {
            throw new RuntimeException("tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱");
        }

        Map<String, Object> body = new LinkedHashMap<>();
        body.put("email", addr);
        Map<String, String> headers = new LinkedHashMap<>();
        setBrowserHeaders(headers, true);
        headers.put("Cookie", cookie);
        HttpResult resp = HttpClient.post(BASE_URL + "/api/mails",
                Json.serialize(body), "application/json", headers);
        if (resp.getStatusCode() == 403) {
            throw new RuntimeException(
                    "tempmail-ee inbox: http 403（会话 Cookie 校验失败，邮箱可能已过期，请重新 Generate）");
        }
        if (!resp.isOk()) {
            throw new RuntimeException("tempmail-ee inbox: http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            return new ArrayList<>();
        }
        JsonElement mailsEl = data.get("mails");
        if (mailsEl == null || !mailsEl.isJsonArray()) {
            return new ArrayList<>();
        }

        List<Email> out = new ArrayList<>();
        for (JsonElement item : mailsEl.getAsJsonArray()) {
            if (!item.isJsonObject()) {
                continue;
            }
            JsonObject row = item.getAsJsonObject();
            Email ne = rowToNorm(row, addr);
            if (ne == null) {
                continue;
            }
            boolean ok = fetchDetail(ne, cookie);
            if (ok) {
                out.add(ne);
            }
            // 单封详情拉取失败不阻塞列表其余邮件（详情偶发 4xx/网络抖动）
        }
        return out;
    }

    /**
     * 将 /api/mails 列表行（仅元数据）转换为 Email 骨架。
     *
     * @param row   列表元素（id/fromAddress/toAddress/subject/createdAt/isRead）
     * @param email 当前邮箱（toAddress 缺失时的回退）
     * @return Email 骨架；id 缺失视为无效行返回 null
     */
    private static Email rowToNorm(JsonObject row, String email) {
        String id = getStr(row, "id");
        if (id.isEmpty()) {
            return null;
        }
        String to = getStr(row, "toAddress", "to");
        if (to.isEmpty()) {
            to = email;
        }
        String created = getStr(row, "createdAt", "date", "receivedAt");
        if (created.isEmpty()) {
            created = java.time.Instant.now().toString();
        }
        Email ne = new Email();
        ne.setId(id);
        ne.setFrom(getStr(row, "fromAddress", "from", "sender"));
        ne.setTo(to);
        ne.setSubject(getStr(row, "subject"));
        ne.setDate(created);
        ne.setRead(readBool(row, "isRead"));
        return ne;
    }

    /**
     * 按候选键顺序从 JSON 对象提取字符串值（数值转十进制字符串）。
     *
     * @param obj  JSON 对象
     * @param keys 候选键
     * @return 命中字符串，未命中返回空串
     */
    private static String getStr(JsonObject obj, String... keys) {
        for (String key : keys) {
            if (obj.has(key) && !obj.get(key).isJsonNull()) {
                String s = Json.nodeToString(obj.get(key)).trim();
                if (!s.isEmpty()) {
                    return s;
                }
            }
        }
        return "";
    }

    /**
     * 从 JSON 对象提取布尔值（兼容 bool / 0|1 数值与字符串）。
     *
     * @param obj  JSON 对象
     * @param keys 候选键
     * @return 布尔值，未命中返回 false
     */
    private static boolean readBool(JsonObject obj, String... keys) {
        for (String key : keys) {
            if (!obj.has(key)) {
                continue;
            }
            JsonElement v = obj.get(key);
            if (v.isJsonPrimitive()) {
                if (v.getAsJsonPrimitive().isBoolean()) {
                    return v.getAsBoolean();
                }
                if (v.getAsJsonPrimitive().isNumber()) {
                    return v.getAsLong() != 0;
                }
                if (v.getAsJsonPrimitive().isString()) {
                    String s = v.getAsString();
                    return "1".equals(s) || "true".equalsIgnoreCase(s);
                }
            }
        }
        return false;
    }

    /**
     * 拉取单封详情并填充正文（GET /api/mails/{id}，带显式会话 Cookie）。
     * content 为平台包装后的 MIME multipart 原文（HTML 实体转义版），
     * 解析后写入 ne.text / ne.html；详情缺失 from/subject 时补全。
     *
     * @param ne     邮件骨架（入参出参）
     * @param cookie 显式会话 Cookie
     * @return 成功返回 true
     */
    private static boolean fetchDetail(Email ne, String cookie) {
        HttpResult resp;
        try {
            Map<String, String> headers = new LinkedHashMap<>();
            setBrowserHeaders(headers, false);
            headers.put("Cookie", cookie);
            resp = HttpClient.get(BASE_URL + "/api/mails/" + ne.getId(), headers);
        } catch (RuntimeException e) {
            return false;
        }
        if (!resp.isOk()) {
            return false;
        }
        JsonObject detail = Json.parseObject(resp.getBody());
        if (detail == null) {
            return false;
        }
        String content = getStr(detail, "content");
        if (content.isEmpty()) {
            // content 缺失时回退兜底候选键，避免平台字段演进后正文丢失
            content = getStr(detail, "text", "body", "html");
            if (content.isEmpty()) {
                return false;
            }
        }
        String[] parsed = parseContent(content);
        if (ne.getText().isEmpty()) {
            ne.setText(parsed[0]);
        }
        if (ne.getHtml().isEmpty()) {
            ne.setHtml(parsed[1]);
        }
        // 详情缺失 from/subject 时补全（列表行已带则不动）
        if (ne.getFrom().isEmpty()) {
            ne.setFrom(getStr(detail, "fromAddress", "from"));
        }
        if (ne.getSubject().isEmpty()) {
            ne.setSubject(getStr(detail, "subject"));
        }
        return true;
    }

    /**
     * 将 HTML 转为纯文本（去脚本、去标签、反转义、压缩空白）。
     *
     * @param src HTML 文本
     * @return 纯文本
     */
    private static String htmlToText(String src) {
        String cleaned = SCRIPT_RE.matcher(src).replaceAll(" ");
        cleaned = TAG_RE.matcher(cleaned).replaceAll(" ");
        return WS_RE.matcher(unescapeHtml(cleaned)).replaceAll(" ").trim();
    }

    /**
     * 解析详情 content：
     * 1) 平台已做 HTML 实体转义（= 写成 &#61;、+ 写成 &#43;、/ 写成 &#47;），
     *    先反转义；之后补充标准 HTML 反转义（&amp;lt; &amp;gt; &amp;amp; &amp;quot; 等）。
     * 2) content 是 MIME multipart 原文（含顶部边界装饰头），按首个边界行切块，
     *    各 part 依据 Content-Transfer-Encoding 做 base64 / quoted-printable 解码，
     *    text part 入文本、html part 入 HTML。
     *
     * @param raw 原始 content 字符串
     * @return [纯文本正文, HTML 正文]（互为兜底合成）
     */
    private static String[] parseContent(String raw) {
        String payload = unescapeHtml(raw);
        payload = payload.replace("&#61;", "=").replace("&#43;", "+").replace("&#47;", "/");
        payload = payload.replace("\r\n", "\n");

        String[] lines = payload.split("\n", -1);
        String boundary = boundaryOf(lines);
        String text = "";
        String html = "";
        if (!boundary.isEmpty()) {
            String[] result = parts(lines, boundary);
            text = result[0];
            html = result[1];
        }
        if (boundary.isEmpty() || (text.isEmpty() && html.isEmpty())) {
            // 无有效 multipart 结构（如邮件 body 仅单个 part）：整个 content 去壳后作为正文
            String[] result = singleton(payload);
            text = result[0];
            html = result[1];
        }
        if (text.isEmpty() && !html.isEmpty()) {
            text = htmlToText(html);
        }
        if (html.isEmpty() && !text.isEmpty()) {
            html = "<html><body><pre>" + escapeHtml(text) + "</pre></body></html>";
        }
        return new String[]{text, html};
    }

    /**
     * 单 part（无边界或拆不出内容）降级解析：依次按「头部区隔
     * （首个空行之后）→ 整体」取内容，并通过关键字识别 Content-Transfer-Encoding 做解码。
     *
     * @param payload 完整内容
     * @return [纯文本正文, ""]
     */
    private static String[] singleton(String payload) {
        String body = payload.trim();
        if (!body.contains("\n")) {
            return new String[]{body, ""};
        }
        String[] lines = payload.split("\n", -1);
        // 头部以 RFC822 形式出现（首行含冒号键值）时，正文从首个空行后开始
        for (int i = 0; i < lines.length; i++) {
            String ln = lines[i];
            if (ln.trim().isEmpty()) {
                body = String.join("\n", java.util.Arrays.copyOfRange(lines, i + 1, lines.length));
                break;
            }
            if (i > 40 || (i >= 3 && !ln.contains(":"))) {
                break;
            }
        }
        String lower = payload.toLowerCase();
        String cte = "";
        if (lower.contains("base64")) {
            cte = "base64";
        } else if (lower.contains("quoted-printable")) {
            cte = "quoted-printable";
        }
        return new String[]{decodePart(body, cte), ""};
    }

    /**
     * 扫描首块寻找边界行（默认 multipart 边界行处于块首）。
     *
     * @param lines 整体内容按行拆分
     * @return 边界字符串，未命中返回空串
     */
    private static String boundaryOf(String[] lines) {
        for (int i = 0; i < lines.length && i < 120; i++) {
            String ln = lines[i];
            if (ln.startsWith("--") && ln.length() > 2) {
                String trimmed = ln.endsWith("\r") ? ln.substring(0, ln.length() - 1) : ln;
                return trimmed.substring(2);
            }
        }
        return "";
    }

    /**
     * 按 boundary 拆分 multipart 并解码归并 text/html 两个 part。
     *
     * @param lines    整体内容按行拆分
     * @param boundary 边界字符串
     * @return [纯文本正文, HTML 正文]
     */
    private static String[] parts(String[] lines, String boundary) {
        String text = "";
        String html = "";
        for (int i = 0; i < lines.length; i++) {
            if (!lines[i].startsWith("--" + boundary)) {
                continue;
            }
            if (lines[i].startsWith("--" + boundary + "--")) {
                break;
            }
            Map<String, String> headersMap = new LinkedHashMap<>();
            int j = i + 1;
            // part 头部：直到首个空行（RFC 空行在 Content-* 头之后）
            while (j < lines.length && !lines[j].isEmpty() && !lines[j].startsWith("--" + boundary)) {
                String ln = lines[j];
                int colon = ln.indexOf(':');
                if (colon > 0) {
                    headersMap.put(ln.substring(0, colon).trim().toLowerCase(),
                            ln.substring(colon + 1).trim());
                }
                j++;
            }
            if (j < lines.length && lines[j].isEmpty()) {
                j++;
            }
            // part 正文：到下一个边界行为止，内部空行属于正文内容
            List<String> partBody = new ArrayList<>();
            while (j < lines.length && !lines[j].startsWith("--" + boundary)) {
                partBody.add(lines[j]);
                j++;
            }
            String[] merged = mergePart(partBody, headersMap, text, html);
            text = merged[0];
            html = merged[1];
            i = j - 1;
        }
        return new String[]{text, html};
    }

    /**
     * 单个 part 的解码归并：按 content-type 归类，缺省回退 text 槽。
     *
     * @param partBody   part 正文行
     * @param headersMap part 头部
     * @param text       已有纯文本
     * @param html       已有 HTML
     * @return [纯文本正文, HTML 正文]
     */
    private static String[] mergePart(List<String> partBody, Map<String, String> headersMap,
                                      String text, String html) {
        String ct = headersMap.getOrDefault("content-type", "").toLowerCase();
        int semicolon = ct.indexOf(';');
        if (semicolon > 0) {
            ct = ct.substring(0, semicolon).trim();
        }
        String cte = headersMap.getOrDefault("content-transfer-encoding", "").toLowerCase();
        String content = String.join("\n", partBody);
        if (ct.contains("text/plain")) {
            if (text.isEmpty()) {
                text = decodePart(content, cte);
            }
            return new String[]{text, html};
        }
        if (ct.contains("text/html")) {
            if (html.isEmpty()) {
                html = decodePart(content, cte);
            }
            return new String[]{text, html};
        }
        if (text.isEmpty()) {
            text = decodePart(content, cte);
        }
        return new String[]{text, html};
    }

    /**
     * 按 Content-Transfer-Encoding 解码 part 内容。
     *
     * @param data part 内容
     * @param cte  编码类型（base64 / quoted-printable）
     * @return 解码后的文本，解码失败原样返回
     */
    private static String decodePart(String data, String cte) {
        String trimmed = data.trim();
        switch (cte) {
            case "base64": {
                String joined = trimmed.replaceAll("[\\n\\r\\t ]", "");
                try {
                    byte[] decoded = Base64.getDecoder().decode(joined);
                    return new String(decoded, StandardCharsets.UTF_8).trim();
                } catch (IllegalArgumentException ignored) {
                    return trimmed;
                }
            }
            case "quoted-printable":
                return decodeQuotedPrintable(trimmed);
            default:
                return trimmed;
        }
    }

    /**
     * 解码 quoted-printable 内容（=XX 十六进制转义 + 软换行处理）。
     *
     * @param src quoted-printable 文本
     * @return 解码后的文本
     */
    private static String decodeQuotedPrintable(String src) {
        StringBuilder out = new StringBuilder(src.length());
        int i = 0;
        while (i < src.length()) {
            char c = src.charAt(i);
            if (c == '=' && i + 1 < src.length()) {
                char n1 = src.charAt(i + 1);
                if (n1 == '\r' || n1 == '\n') {
                    // 软换行：跳过 = 与后续换行
                    i += 2;
                    if (i < src.length() && src.charAt(i) == '\n') {
                        i++;
                    }
                    continue;
                }
                if (i + 2 < src.length()) {
                    String hex = src.substring(i + 1, i + 3);
                    try {
                        out.append((char) Integer.parseInt(hex, 16));
                        i += 3;
                        continue;
                    } catch (NumberFormatException ignored) {
                        // 非法转义原样保留
                    }
                }
            }
            out.append(c);
            i++;
        }
        return out.toString();
    }

    /**
     * 标准 HTML 实体反转义（&amp;lt; &amp;gt; &amp;amp; &amp;quot; &amp;#39; &amp;nbsp;）。
     *
     * @param s 含实体的文本
     * @return 反转义后的文本
     */
    private static String unescapeHtml(String s) {
        return s.replace("&amp;", "&")
                .replace("&lt;", "<")
                .replace("&gt;", ">")
                .replace("&quot;", "\"")
                .replace("&#39;", "'")
                .replace("&apos;", "'")
                .replace("&nbsp;", " ");
    }

    /**
     * HTML 转义，用于 text 兜底合成 HTML。
     *
     * @param s 纯文本
     * @return 转义后的文本
     */
    private static String escapeHtml(String s) {
        return s.replace("&", "&amp;")
                .replace("<", "&lt;")
                .replace(">", "&gt;")
                .replace("\"", "&quot;")
                .replace("'", "&#39;");
    }
}