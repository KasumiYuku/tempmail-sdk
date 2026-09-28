package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.xxxxteam.tempmail.Email;
import com.xxxxteam.tempmail.EmailInfo;
import com.xxxxteam.tempmail.HttpResult;
import com.xxxxteam.tempmail.HttpClient;
import com.xxxxteam.tempmail.Json;
import com.xxxxteam.tempmail.Normalizer;

import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * NoxenDe5Net 渠道 — UniMail Bot 公共实例 https://tempmail.noxen.de5.net
 *
 * <p>固定访客账号登录（username guest / password 123456）：
 * POST /api/login → Set-Cookie: iding-session=&lt;JWT&gt;（HttpOnly），
 * GET /api/session 校验 {"authenticated":true}，GET /api/generate 建箱
 * （响应 email/expires 毫秒时间戳）。</p>
 *
 * <p>建档 token 凭据串：&lt;渠道前缀&gt;&lt;URL 编码会话 Cookie&gt;（prefix|base|）。
 * 读信：解析凭据 Cookie → GET /api/session 校验（否则重登录）→
 * GET /api/emails?mailbox=&lt;地址&gt;&amp;limit=20 取列表 →
 * 每封 GET /api/email/{id} 取详情（含 download 下载路径）→
 * GET 基址+相对路径 拉原始 EML（Accept: message/rfc822,*&#47;*），
 * 本地解析 MIME：CRLF 归一为 LF，拆头部与正文（支持折行续行、跳过 "From " 首行），
 * 按 boundary 切 part（multipart 递归、message/rfc822 整体递归），
 * Content-Transfer-Encoding base64（java.util.Base64 去空白解码）与
 * quoted-printable（软换行与 =XX hex）解码；解析失败或 download 空时
 * 正文=合成占位（"验证码: &lt;code&gt;" 置顶 + preview）。</p>
 */
public final class NoxenDe5Net {

    private static final String BASE_URL = "https://tempmail.noxen.de5.net";
    private static final String USERNAME = "guest";
    private static final String PASSWORD = "123456";
    private static final String CHANNEL = "noxen-de5-net";

    /** 本渠道凭据串前缀。 */
    private static final String TOKEN_PREFIX = "noxen-de5-net|";

    /** 提取 Content-Type 中的 multipart boundary（引号可选）。 */
    private static final Pattern BOUNDARY_RE = Pattern.compile("(?i)boundary=\"?([^\";\\s]+)\"?");

    private NoxenDe5Net() {
    }

    /**
     * 配置带 UA 的基础请求头。
     *
     * @param headers 待填充的请求头
     */
    private static void setBaseHeaders(Map<String, String> headers) {
        headers.put("Accept", "application/json");
        headers.put("User-Agent",
                "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36"
                        + " (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36");
    }

    /**
     * 登录取得会话 Cookie（iding-session=&lt;JWT&gt;）。
     *
     * @return "iding-session=&lt;JWT&gt;" 键值对
     */
    private static String sessionCookie() {
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("username", USERNAME);
        body.put("password", PASSWORD);
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Content-Type", "application/json");
        setBaseHeaders(headers);
        HttpResult resp = HttpClient.post(BASE_URL + "/api/login",
                Json.serialize(body), "application/json", headers);
        if (!resp.isOk()) {
            throw new RuntimeException("noxen-de5-net login: http " + resp.getStatusCode());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null || !"true".equals(Json.str(data, "success"))) {
            throw new RuntimeException("noxen-de5-net login: 登录失败");
        }
        for (String sc : resp.getSetCookies()) {
            String kv = sc;
            int semicolon = sc.indexOf(';');
            if (semicolon > 0) {
                kv = sc.substring(0, semicolon);
            }
            if (kv.startsWith("iding-session=")) {
                return kv;
            }
        }
        throw new RuntimeException("noxen-de5-net login: 未下发会话 Cookie");
    }

    /**
     * 校验会话 Cookie 是否仍有效（GET /api/session 返回 authenticated:true）。
     *
     * @param cookie "iding-session=&lt;JWT&gt;" 键值对
     * @return 有效返回 true
     */
    private static boolean cookieStillValid(String cookie) {
        try {
            Map<String, String> headers = new LinkedHashMap<>();
            setBaseHeaders(headers);
            headers.put("Cookie", cookie);
            HttpResult resp = HttpClient.get(BASE_URL + "/api/session", headers);
            JsonObject data = Json.parseObject(resp.getBody());
            return resp.isOk() && data != null
                    && "true".equals(Json.str(data, "authenticated"));
        } catch (RuntimeException ignored) {
            return false;
        }
    }

    /**
     * 创建临时邮箱（登录 + GET /api/generate 建箱）。token 凭据串：
     * "noxen-de5-net|&lt;URL 编码会话 Cookie&gt;|base=https://tempmail.noxen.de5.net"。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        String session = sessionCookie();
        Map<String, String> headers = new LinkedHashMap<>();
        setBaseHeaders(headers);
        headers.put("Cookie", session);
        HttpResult resp = HttpClient.get(BASE_URL + "/api/generate", headers);
        if (!resp.isOk()) {
            throw new RuntimeException("noxen-de5-net generate: http " + resp.getStatusCode());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("noxen-de5-net generate: 响应解析失败");
        }
        String email = Json.str(data, "email").trim();
        if (email.isEmpty()) {
            throw new RuntimeException("noxen-de5-net generate: 响应缺少 email");
        }
        long expires = 0;
        try {
            expires = Long.parseLong(Json.str(data, "expires").trim());
        } catch (NumberFormatException ignored) {
            // 响应缺少 expires 时保留 null
        }
        String token = TOKEN_PREFIX + ProviderUtil.urlEncode(session) + "|base=" + BASE_URL;
        Long expiresMs = null;
        if (expires > 0) {
            expiresMs = expires;
        }
        return new EmailInfo(CHANNEL, email, token, expiresMs, null);
    }

    /**
     * 解析凭据串：去前缀、URL 解码、去后缀（|base=基址），返回会话 Cookie。
     *
     * @param token 建箱下发的凭据串
     * @return "iding-session=&lt;JWT&gt;" 键值对
     */
    private static String parseToken(String token) {
        if (token == null || !token.startsWith(TOKEN_PREFIX)) {
            throw new RuntimeException("noxen-de5-net: token 格式错误");
        }
        String stripped = token.substring(TOKEN_PREFIX.length());
        stripped = stripSuffix(stripped, "|base=" + BASE_URL);
        try {
            return URLDecoder.decode(stripped, StandardCharsets.UTF_8);
        } catch (IllegalArgumentException e) {
            throw new RuntimeException("noxen-de5-net: token 解码失败");
        }
    }

    /**
     * 去除结尾后缀（存在则移除）。
     *
     * @param s      原字符串
     * @param suffix 后缀
     * @return 去后缀后的字符串
     */
    private static String stripSuffix(String s, String suffix) {
        return s.endsWith(suffix) ? s.substring(0, s.length() - suffix.length()) : s;
    }

    /**
     * 读取收件箱。会话校验失败自动重登录；列表 → 详情 → EML 下载全文解析，
     * 全文不可得时以 verification_code/preview 合成占位正文。
     *
     * @param token 建箱下发的凭据串
     * @param email 信箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        String cookie = parseToken(token);
        if (!cookieStillValid(cookie)) {
            cookie = sessionCookie();
        }

        Map<String, String> headers = new LinkedHashMap<>();
        setBaseHeaders(headers);
        headers.put("Cookie", cookie);
        HttpResult listResp = HttpClient.get(BASE_URL + "/api/emails?mailbox="
                + ProviderUtil.urlEncode(addr) + "&limit=20", headers);
        if (!listResp.isOk()) {
            if (listResp.getStatusCode() == 401) {
                throw new RuntimeException("noxen-de5-net 读信: http 401（会话失效或非本会话邮箱）");
            }
            throw new RuntimeException("noxen-de5-net 读信: http " + listResp.getStatusCode());
        }
        JsonElement parsed = Json.parse(listResp.getBody());
        if (parsed == null) {
            throw new RuntimeException("noxen-de5-net 读信: 列表响应解析失败");
        }
        List<JsonObject> list = new ArrayList<>();
        if (parsed.isJsonArray()) {
            for (JsonElement item : parsed.getAsJsonArray()) {
                if (item.isJsonObject()) {
                    list.add(item.getAsJsonObject());
                }
            }
        }

        List<Email> out = new ArrayList<>();
        for (JsonObject m : list) {
            String id = Json.str(m, "id").trim();
            Map<String, Object> flat = Json.toDict(m);
            flat.put("from", Json.str(m, "sender"));
            flat.put("to", addr);
            flat.put("date", Json.str(m, "received_at"));
            flat.put("text", Json.str(m, "preview"));
            flat.put("isRead", Json.toRaw(m.get("is_read")));
            boolean full = false;
            if (!id.isEmpty() && !"0".equals(id)) {
                // 详情：取 download 定位 + 兜底 content/html_content（平台恒空）
                JsonObject detail = fetchDetail(cookie, id);
                if (detail != null) {
                    flat.put("content", Json.str(detail, "content"));
                    flat.put("html_content", Json.str(detail, "html_content"));
                    flat.put("to_addrs", Json.str(detail, "to_addrs"));
                    String download = Json.str(detail, "download").trim();
                    if (!download.isEmpty()) {
                        // 全文：download 端点原始 EML → 本地拆分 text/html
                        String eml = fetchEml(cookie, download);
                        if (!eml.isEmpty()) {
                            String[] parsedBody = parseEml(eml);
                            if (!parsedBody[0].isEmpty() || !parsedBody[1].isEmpty()) {
                                flat.put("text", parsedBody[0]);
                                flat.put("html_content", parsedBody[1]);
                                full = true;
                            }
                        }
                    }
                }
                if (!full) {
                    flat.put("text", composePlaceholder(m));
                }
            }
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }

    /**
     * 拉取单封邮件详情（GET /api/email/{id}），失败返回 null。
     *
     * @param cookie 会话凭据
     * @param id     邮件 ID
     * @return 详情 JSON 对象，失败返回 null
     */
    private static JsonObject fetchDetail(String cookie, String id) {
        try {
            Map<String, String> headers = new LinkedHashMap<>();
            setBaseHeaders(headers);
            headers.put("Cookie", cookie);
            HttpResult resp = HttpClient.get(BASE_URL + "/api/email/"
                    + ProviderUtil.urlEncode(id), headers);
            if (!resp.isOk()) {
                return null;
            }
            return Json.parseObject(resp.getBody());
        } catch (RuntimeException ignored) {
            return null;
        }
    }

    /**
     * 拉取原始 EML 报文（详情 download 字段指向的相对路径），失败返回空串。
     *
     * @param cookie 会话凭据
     * @param path   下载相对路径（如 /api/email/3880/download）
     * @return EML 原文，失败返回空串
     */
    private static String fetchEml(String cookie, String path) {
        try {
            String url = path;
            if (!url.startsWith("http://") && !url.startsWith("https://")) {
                url = BASE_URL + url;
            }
            Map<String, String> headers = new LinkedHashMap<>();
            headers.put("Accept", "message/rfc822, */*");
            headers.put("User-Agent",
                    "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36"
                            + " (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36");
            headers.put("Cookie", cookie);
            HttpResult resp = HttpClient.get(url, headers);
            if (!resp.isOk()) {
                return "";
            }
            return resp.getBody();
        } catch (RuntimeException ignored) {
            return "";
        }
    }

    /**
     * 全文不可得时的合成占位正文：verification_code 置顶（"验证码: xxx"），
     * preview 附后为正文近况；二者均空时返回空串。
     *
     * @param m 列表元素（verification_code/preview）
     * @return 合成正文
     */
    private static String composePlaceholder(JsonObject m) {
        String code = Json.str(m, "verification_code").trim();
        String preview = Json.str(m, "preview").trim();
        StringBuilder sb = new StringBuilder();
        if (!code.isEmpty()) {
            sb.append("验证码: ").append(code);
        }
        if (!preview.isEmpty()) {
            if (sb.length() > 0) {
                sb.append("\n\n");
            }
            sb.append(preview);
        }
        return sb.toString();
    }

    /**
     * 解析 EML 原始报文 → [纯文本正文, HTML 正文]。先做 CRLF→LF 归一化，
     * 再拆顶层头部与正文，递归解析 MIME 实体（与上游 parseEmailBody 同构）。
     *
     * @param raw EML 原文
     * @return [纯文本正文, HTML 正文]
     */
    private static String[] parseEml(String raw) {
        String payload = raw.replace("\r\n", "\n").replace("\r", "");
        String[] split = splitEml(payload, 0);
        return parseEntity(split[0], split[1]);
    }

    /**
     * 将原始报文切分为首部 map（键小写）与正文块。
     *
     * @param payload 已做 CRLF→LF 归一化的原始报文
     * @param offset  起始行号（外包 "#participant"+part 时置 1）
     * @return [首部键值对, 正文块]
     */
    private static String[] splitEml(String payload, int offset) {
        String[] lines = payload.split("\n", -1);
        Map<String, String> headers = new LinkedHashMap<>();
        int i = offset;
        if (i < lines.length && lines[i].startsWith("From ")) {
            i++;
        }
        String curKey = "";
        for (; i < lines.length; i++) {
            String line = lines[i];
            if (line.isEmpty()) {
                i++;
                break;
            }
            // 折行续行（RFC 5322）：以空白开头且已有当前头键时拼接到上一条
            if (!line.isEmpty() && (line.charAt(0) == ' ' || line.charAt(0) == '\t')
                    && !curKey.isEmpty()) {
                headers.put(curKey, headers.get(curKey) + " " + line.trim());
                continue;
            }
            int colon = line.indexOf(':');
            if (colon > 0) {
                curKey = line.substring(0, colon).trim().toLowerCase();
                headers.put(curKey, line.substring(colon + 1).trim());
            }
        }
        if (i > lines.length) {
            i = lines.length;
        }
        StringBuilder body = new StringBuilder();
        for (int j = i; j < lines.length; j++) {
            if (body.length() > 0) {
                body.append('\n');
            }
            body.append(lines[j]);
        }
        StringBuilder headerCsv = new StringBuilder();
        for (Map.Entry<String, String> kv : headers.entrySet()) {
            headerCsv.append(kv.getKey()).append('\u0001').append(kv.getValue()).append('\u0000');
        }
        return new String[]{headerCsv.toString(), body.toString()};
    }

    /**
     * 从序列化首部块重建首部 map。
     *
     * @param csv 首部块（键\u0001值\u0000 分隔）
     * @return 首部 map
     */
    private static Map<String, String> headersFrom(String csv) {
        Map<String, String> out = new LinkedHashMap<>();
        if (csv.isEmpty()) {
            return out;
        }
        for (String entry : csv.split("\u0000", -1)) {
            int sep = entry.indexOf('\u0001');
            if (sep > 0) {
                out.put(entry.substring(0, sep), entry.substring(sep + 1));
            }
        }
        return out;
    }

    /**
     * 递归解析单个 MIME 实体。multipart 按 boundary 切 part 递归；
     * message/rfc822 整体递归；rfc822-headers part 跳过；
     * 单体按 Content-Transfer-Encoding base64 / quoted-printable 解码，
     * text/html 入 HTML 槽、其余入纯文本槽（各取首个非空）；
     * 无 HTML 命中时在整体原文中抓取 &lt;html&gt;…&lt;/html&gt; 片段。
     *
     * @param headerCsv 首部块
     * @param body      实体正文块
     * @return [纯文本正文, HTML 正文]
     */
    private static String[] parseEntity(String headerCsv, String body) {
        Map<String, String> headers = headersFrom(headerCsv);
        String ctRaw = headers.getOrDefault("content-type", "");
        String ct = ctRaw.toLowerCase();
        String cte = headers.getOrDefault("content-transfer-encoding", "").toLowerCase();

        // 单体：text/html 或 text/plain（含无 Content-Type 时按纯文本处理）
        if (!ct.startsWith("multipart/")) {
            String decoded = decodePart(body, cte);
            if (ct.contains("text/html")) {
                return new String[]{"", decoded};
            }
            return new String[]{decoded, ""};
        }

        // 复合：递归拆分，text 槽与 html 槽各自取第一个非空命中
        String text = "";
        String html = "";
        String boundary = boundaryOf(ctRaw);
        if (!boundary.isEmpty()) {
            for (String part : splitMultipart(body, boundary)) {
                String[] split = splitEml("#participant\n" + part, 1);
                Map<String, String> ph = headersFrom(split[0]);
                String pct = ph.getOrDefault("content-type", "").toLowerCase();
                String[] merged;
                if (pct.contains("rfc822-headers")) {
                    // 纯头部 part 跳过，正文在后续 part 中抓取
                    continue;
                }
                if (pct.startsWith("message/rfc822")) {
                    // 转发的原始邮件整体作为 part：递归整封解析
                    String[] nested = splitEml(split[1], 0);
                    merged = parseEntity(nested[0], nested[1]);
                } else {
                    merged = parseEntity(split[0], split[1]);
                }
                if (text.isEmpty()) {
                    text = merged[0];
                }
                if (html.isEmpty()) {
                    html = merged[1];
                }
                if (!text.isEmpty() && !html.isEmpty()) {
                    break;
                }
            }
        }
        // 无 HTML 命中时从整体原文兜底抓取 HTML 片段
        if (html.isEmpty()) {
            html = guessHtml(body);
        }
        return new String[]{text, html};
    }

    /**
     * 从原始 Content-Type 头值提取 boundary。
     *
     * @param ctRaw Content-Type 原始值
     * @return boundary，未命中返回空串
     */
    private static String boundaryOf(String ctRaw) {
        Matcher m = BOUNDARY_RE.matcher(ctRaw);
        return m.find() ? m.group(1) : "";
    }

    /**
     * 按 boundary 切出各 part（含各自首部行）。与 Go
     * noxenDe5NetSplitMultipart 同构：按 "--"+boundary 切分、TrimPrefix "\n"、
     * TrimSuffix "--\n"/"--"，剔除全空白段。
     *
     * @param body     实体正文
     * @param boundary 边界串
     * @return part 列表
     */
    private static List<String> splitMultipart(String body, String boundary) {
        List<String> parts = new ArrayList<>();
        for (String seg : body.split(Pattern.quote("--" + boundary), -1)) {
            seg = stripPrefixOneNewline(seg);
            seg = stripSuffix(seg, "--\n");
            seg = stripSuffix(seg, "--");
            if (!seg.trim().isEmpty()) {
                parts.add(seg);
            }
        }
        return parts;
    }

    /**
     * 去除开头的单个换行符（与 Go TrimPrefix "\n" 一致）。
     *
     * @param s 原字符串
     * @return 处理后字符串
     */
    private static String stripPrefixOneNewline(String s) {
        return s.startsWith("\n") ? s.substring(1) : s;
    }

    /**
     * 按 Content-Transfer-Encoding 解码 part 内容：
     * base64（java.util.Base64 去空白解码）与 quoted-printable
     * （软换行与 =XX hex，与 Go mime/quotedprintable 等价），
     * 其余（7bit/8bit/binary）原样返回。非 UTF-8 字符集不做转码
     * （与上游 TextDecoder(fatal:false) 行为同理），解码失败原样返回。
     *
     * @param data part 内容
     * @param cte  Content-Transfer-Encoding 值
     * @return 解码后的文本
     */
    private static String decodePart(String data, String cte) {
        switch (cte.toLowerCase().trim()) {
            case "base64": {
                String joined = data.replaceAll("[\\n\\r\\t ]", "");
                try {
                    byte[] decoded = Base64.getDecoder().decode(joined);
                    return new String(decoded, StandardCharsets.UTF_8).trim();
                } catch (IllegalArgumentException ignored) {
                    return data;
                }
            }
            case "quoted-printable":
                return decodeQuotedPrintable(data).trim();
            default:
                return data.trim();
        }
    }

    /**
     * 解码 quoted-printable 内容（=XX 十六进制转义 + 行尾 '=' 软换行）。
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
     * 从整体原文中抓取 &lt;html&gt;…&lt;/html&gt; 片段（大小写不敏感，
     * 无则回退 &lt;!doctype html&gt;），覆盖 HTML 正文裸置的情况。
     *
     * @param body 整体原文
     * @return HTML 片段，未命中返回空串
     */
    private static String guessHtml(String body) {
        if (body.isEmpty()) {
            return "";
        }
        String lower = body.toLowerCase();
        int hs = lower.indexOf("<html");
        if (hs == -1) {
            hs = lower.indexOf("<!doctype html");
        }
        if (hs == -1) {
            return "";
        }
        int he = lower.lastIndexOf("</html>");
        if (he == -1 || he < hs) {
            return "";
        }
        return body.substring(hs, he + 7);
    }
}