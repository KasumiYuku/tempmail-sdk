package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.xxxxteam.tempmail.Email;
import com.xxxxteam.tempmail.EmailInfo;
import com.xxxxteam.tempmail.HttpResult;
import com.xxxxteam.tempmail.HttpClient;
import com.xxxxteam.tempmail.Json;
import com.xxxxteam.tempmail.Normalizer;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Pattern;

/**
 * Temporarymail 渠道 — https://temporarymail.com
 *
 * <p>无认证 REST（key 为空即随机建箱）：
 * GET /api/?action=requestEmailAccess&amp;key=&amp;value=random 建箱，
 * 响应 {"address","secretKey"}；secretKey 用于后续读信。</p>
 *
 * <p>读信 GET /api/?action=checkInbox&amp;value=&lt;secretKey&gt;，响应双形态：
 * 空箱为 []，有信为 map[id]→对象（值序输出）；逐封 POST /api/?action=getEmail
 * 覆盖真实 subject/from（失败兜底），再 GET /view/?i=&lt;id&gt;&amp;width=800
 * 取全文并剥标签还原纯文本。</p>
 *
 * <p>键控风控说明：403/404 疑似 UA 键控风控，换备用 UA 重试一次；
 * 429 平台限流报出 Retry-After（交给 SDK 外层退避重试）。</p>
 */
public final class TemporarymailCom {

    private static final String BASE_URL = "https://temporarymail.com";
    private static final String CHANNEL = "temporarymail-com";

    /** 主浏览器 UA（Java 端无共享 UA 池，固定主备两个指纹形状）。 */
    private static final String MAIN_UA = "Mozilla/5.0 (X11; Linux x86_64)"
            + " AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36";

    /** 备用浏览器 UA（403 重试用，规避共享池随机 UA 耗尽）。 */
    private static final String ALT_UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"
            + " AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36";

    /** 剥除 HTML 标签（含属性与 script/style 整段）的正则。 */
    private static final Pattern TAG_RE =
            Pattern.compile("<script[\\s\\S]*?</script>|<style[\\s\\S]*?</style>|<[^>]+>");

    private TemporarymailCom() {
    }

    /**
     * 构造 /api/ 请求头集（与官网 mainScanner.js 浏览器形态一致）。
     *
     * @param ua 用户代理（调用方决定主用/备用）
     * @return 请求头
     */
    private static Map<String, String> apiHeaders(String ua) {
        Map<String, String> h = new LinkedHashMap<>();
        h.put("Accept", "application/json, text/plain, */*");
        h.put("Accept-Language", "en-US,en;q=0.9");
        h.put("Sec-Fetch-Site", "same-origin");
        h.put("Sec-Fetch-Mode", "cors");
        h.put("Sec-Fetch-Dest", "empty");
        h.put("Referer", BASE_URL + "/");
        h.put("Origin", BASE_URL);
        h.put("User-Agent", ua);
        return h;
    }

    /**
     * 创建 temporarymail.com 临时邮箱（GET /api/?action=requestEmailAccess），
     * token 复用响应的 secretKey。429 时携带 Retry-After 报错。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        HttpResult resp = HttpClient.get(BASE_URL
                + "/api/?action=requestEmailAccess&key=&value=random", apiHeaders(currentUa()));
        if (resp.getStatusCode() == 429) {
            String ra = resp.getHeaders().getOrDefault("Retry-After", "");
            throw new RuntimeException("temporarymail: 创建邮箱平台限流(429 Retry-After="
                    + ra + ")，请稍后重试: " + resp.getBody());
        }
        if (!resp.isOk()) {
            throw new RuntimeException("temporarymail: 创建邮箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("temporarymail: 解析创建响应失败");
        }
        String address = Json.str(data, "address").trim();
        String secretKey = Json.str(data, "secretKey").trim();
        if (address.isEmpty() || secretKey.isEmpty()) {
            throw new RuntimeException("temporarymail: 创建响应缺少 address 或 secretKey: "
                    + resp.getBody());
        }
        return new EmailInfo(CHANNEL, address, secretKey, null, null);
    }

    /**
     * 读取收件箱（GET /api/?action=checkInbox）。响应双形态：空箱 []，
     * 有信为 map[id]→对象（值序输出）。列表主题常为 "[No Subject]"：
     * 逐封详情覆盖真实主题/发件人，逐封 /view/ 渲染端点抓取全文。
     *
     * @param token 建箱返回的 secretKey
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("temporarymail: token 为空");
        }
        String body = checkInbox(tok);

        List<JsonObject> list = new ArrayList<>();
        JsonElement parsed = Json.parse(body);
        if (parsed == null) {
            throw new RuntimeException("temporarymail: 解析收件箱响应失败（secretKey 可能已失效）");
        }
        if (parsed.isJsonArray()) {
            for (JsonElement item : parsed.getAsJsonArray()) {
                if (item.isJsonObject()) {
                    list.add(item.getAsJsonObject());
                }
            }
        } else if (parsed.isJsonObject()) {
            // 平台响应两种合法形态之一：map[id]→对象，按值序输出
            for (Map.Entry<String, JsonElement> kv : parsed.getAsJsonObject().entrySet()) {
                if (kv.getValue() != null && kv.getValue().isJsonObject()) {
                    list.add(kv.getValue().getAsJsonObject());
                }
            }
        }

        List<Email> out = new ArrayList<>();
        for (JsonObject m : list) {
            Map<String, Object> flat = Json.toDict(m);
            // 列表元素无 to 字段，注入收件人地址
            flat.put("to", email);
            String id = Json.str(m, "id");
            if (!id.isEmpty()) {
                // 详情覆盖真实主题/发件人（失败不致命：列表元数据兜底）
                JsonObject det = fetchDetail(id);
                if (det != null) {
                    String subject = Json.str(det, "subject");
                    if (!subject.isEmpty()) {
                        flat.put("subject", subject);
                    }
                    String from = Json.str(det, "from");
                    if (!from.isEmpty()) {
                        flat.put("from", from);
                    }
                }
                // /view/ 渲染端点全文（失败不致命：列表元数据兜底）
                String text = fetchView(id);
                if (!text.isEmpty()) {
                    flat.put("text", text);
                }
            }
            out.add(Normalizer.normalizeEmail(flat, email));
        }
        return out;
    }

    /**
     * 拉取 checkInbox 响应体。403/404 换备用 UA 重试一次；
     * 429 报出平台限流（含 Retry-After）；其余非 2xx 携带响应体报错。
     *
     * @param token secretKey
     * @return 响应正文
     */
    private static String checkInbox(String token) {
        for (String ua : new String[]{currentUa(), ALT_UA}) {
            HttpResult resp = HttpClient.get(BASE_URL + "/api/?action=checkInbox&value="
                    + ProviderUtil.urlEncode(token), apiHeaders(ua));
            if (resp.getStatusCode() == 429) {
                String ra = resp.getHeaders().getOrDefault("Retry-After", "");
                throw new RuntimeException("temporarymail: 读取收件箱平台限流(429 Retry-After="
                        + ra + ")，请拉大轮询间隔");
            }
            if (resp.isOk()) {
                return resp.getBody();
            }
            // 403/404 疑似 UA 键控风控，换备用 UA 重试一次
            if (resp.getStatusCode() != 403 && resp.getStatusCode() != 404) {
                throw new RuntimeException("temporarymail: 读取收件箱失败 http "
                        + resp.getStatusCode() + ": " + resp.getBody());
            }
        }
        throw new RuntimeException("temporarymail: 读取收件箱失败 http 403（两次尝试均被拒）");
    }

    /**
     * 拉取单封邮件详情（POST /api/?action=getEmail），响应为
     * {id: {...}} 单元素对象；429/captcha 风控时返回 null（列表元数据兜底）。
     *
     * @param id 邮件 ID
     * @return 详情 JSON 对象，失败或限流返回 null
     */
    private static JsonObject fetchDetail(String id) {
        try {
            HttpResult resp = HttpClient.post(BASE_URL + "/api/?action=getEmail&value="
                    + ProviderUtil.urlEncode(id), null, null, apiHeaders(currentUa()));
            if (resp.getStatusCode() == 429 || !resp.isOk()) {
                return null;
            }
            JsonObject obj = Json.parseObject(resp.getBody());
            if (obj == null) {
                return null;
            }
            for (Map.Entry<String, JsonElement> kv : obj.entrySet()) {
                if (kv.getValue() != null && kv.getValue().isJsonObject()) {
                    return kv.getValue().getAsJsonObject();
                }
            }
            return null;
        } catch (RuntimeException ignored) {
            return null;
        }
    }

    /**
     * 抓取 /view/ 渲染端点全文并还原纯文本（响应为平台已 HTML 化的
     * text/plain，正文逐行带 &lt;br /&gt;）：&lt;br&gt;/&lt;p&gt; 转换行、
     * 剥除其余标签、HTML 实体反转义、逐行 trim。
     *
     * @param id 邮件 ID
     * @return 纯文本正文，失败返回空串
     */
    private static String fetchView(String id) {
        try {
            Map<String, String> headers = new LinkedHashMap<>();
            headers.put("Accept", "text/html, */*");
            headers.put("User-Agent", currentUa());
            headers.put("Referer", BASE_URL + "/");
            HttpResult resp = HttpClient.get(BASE_URL + "/view/?i="
                    + ProviderUtil.urlEncode(id) + "&width=800", headers);
            if (!resp.isOk()) {
                return "";
            }
            return viewToText(resp.getBody());
        } catch (RuntimeException ignored) {
            return "";
        }
    }

    /**
     * 将 /view/ 响应剥标签还原为纯文本（与 Go 端 temporarymailViewToText 保持一致）。
     *
     * @param src HTML 化正文
     * @return 纯文本
     */
    private static String viewToText(String src) {
        String s = src.replace("<br />", "\n")
                .replace("<br/>", "\n")
                .replace("<br>", "\n")
                .replace("<p>", "\n")
                .replace("</p>", "\n")
                .replace("&nbsp;", " ")
                .replace("&gt;", ">")
                .replace("&lt;", "<")
                .replace("&amp;", "&")
                .replace("&quot;", "\"");
        s = TAG_RE.matcher(s).replaceAll(" ");
        s = unescapeHtmlFull(s);
        String[] lines = s.split("\n", -1);
        StringBuilder sb = new StringBuilder(s.length());
        for (String ln : lines) {
            if (sb.length() > 0) {
                sb.append('\n');
            }
            sb.append(ln.trim());
        }
        return sb.toString().trim();
    }

    /**
     * 反转义 HTML 实体后对常见命名实体二次回转（与 Go htmlUnescapeFull 一致）。
     *
     * @param s 含实体的文本
     * @return 反转义后的文本
     */
    private static String unescapeHtmlFull(String s) {
        String u = s.replace("&#39;", "'")
                .replace("&#x27;", "'")
                .replace("&#x60;", "`");
        u = unescapeEntities(u);
        return u.replace("&quot;", "\"").replace("&apos;", "'");
    }

    /**
     * 数字实体（十进制/十六进制）与常见命名实体反转义（手动实现，
     * 与 Go html.UnescapeString 的常用命中集合对齐）。
     *
     * @param s 含实体的文本
     * @return 反转义后的文本
     */
    private static String unescapeEntities(String s) {
        Map<String, String> named = new LinkedHashMap<>();
        named.put("&ndash;", "–");
        named.put("&mdash;", "—");
        named.put("&lsquo;", "‘");
        named.put("&rsquo;", "’");
        named.put("&ldquo;", "“");
        named.put("&rdquo;", "”");
        named.put("&hellip;", "…");
        named.put("&eacute;", "é");
        named.put("&ouml;", "ö");
        named.put("&uuml;", "ü");
        named.put("&auml;", "ä");
        named.put("&szlig;", "ß");
        named.put("&copy;", "©");
        named.put("&reg;", "®");
        named.put("&trade;", "™");
        named.put("&lt;", "<");
        named.put("&gt;", ">");
        named.put("&amp;", "&");
        StringBuilder sb = new StringBuilder(s.length());
        int i = 0;
        while (i < s.length()) {
            char c = s.charAt(i);
            if (c != '&' || i + 2 >= s.length()) {
                sb.append(c);
                i++;
                continue;
            }
            int semi = s.indexOf(';', i + 1);
            if (semi < 0 || semi - i > 12) {
                sb.append(c);
                i++;
                continue;
            }
            String entity = s.substring(i + 1, semi);
            String rep = null;
            if (entity.length() >= 2
                    && (entity.charAt(0) == '#')) {
                try {
                    int cp;
                    if (entity.charAt(1) == 'x' || entity.charAt(1) == 'X') {
                        cp = Integer.parseInt(entity.substring(2), 16);
                    } else {
                        cp = Integer.parseInt(entity.substring(1), 10);
                    }
                    rep = new String(Character.toChars(cp));
                } catch (RuntimeException ignored) {
                    // 非法数字实体原样保留
                }
            } else {
                rep = named.get("&" + entity + ";");
            }
            if (rep == null) {
                sb.append(c);
                i++;
            } else {
                sb.append(rep);
                i = semi + 1;
            }
        }
        return sb.toString();
    }

    /**
     * 当前用户代理（主用 UA）。
     *
     * @return 用户代理串
     */
    private static String currentUa() {
        return MAIN_UA;
    }
}