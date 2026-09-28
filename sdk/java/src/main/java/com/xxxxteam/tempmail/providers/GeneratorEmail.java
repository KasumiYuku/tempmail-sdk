package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonObject;
import com.xxxxteam.tempmail.Email;
import com.xxxxteam.tempmail.EmailInfo;
import com.xxxxteam.tempmail.HttpClient;
import com.xxxxteam.tempmail.HttpResult;
import com.xxxxteam.tempmail.Json;
import com.xxxxteam.tempmail.Normalizer;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * GeneratorEmail 渠道 — https://generator.email（PHP SSR 网页型）。
 *
 * <p>无独立建箱 API：服务器端渲染直接生成随机邮箱并写进页面内联
 * window.SITE_DATA（cur_user / cur_domain，邮箱=user@domain），同页
 * Set-Cookie inbox_ctx 选中该邮箱会话。读信同为 SSR：GET /inbox4/
 * （轮换别名 /inbox{1..4}/ 同构）带 inbox_ctx Cookie 返回渲染页，
 * 信件列表在 #email-table，每条 div.list-group-item 内三个子 div：
 * from_div_45g45gg（From）、subj_div_45g45gg（Subject）、
 * time_div_45g45gg（Time (UTC)）。</p>
 *
 * <p>限制：本站邮件正文不提供纯文本/HTML 原文（默认渲染摘要），SDK 按
 * 列表三要素归一。token 保存 {email, domain, user} JSON 快照。</p>
 *
 * <p>Java 的 HttpClient 默认无 Cookie 罐，本类在类内维护静态会话 Cookie 串，
 * 每次响应的 Set-Cookie 逐键合并（同键覆写）后随后续请求显式携带。</p>
 */
public final class GeneratorEmail {

    private static final String BASE_URL = "https://generator.email";
    private static final String INBOX_URL = BASE_URL + "/inbox4/";
    private static final String CHANNEL = "generator-email";

    /** 固定浏览器 UA（Chrome 154 / Linux，与各端一致）。 */
    private static final String UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36"
            + " (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /** SITE_DATA 快照提取（cur_user / cur_domain）。 */
    private static final Pattern USER_RE = Pattern.compile("cur_user:\"([^\"]*)\"");
    private static final Pattern DOMAIN_RE = Pattern.compile("cur_domain:\"([^\"]*)\"");

    /** 列表条目与三要素块（g8r 前缀可变，按类名后缀锚定）。 */
    private static final Pattern ITEM_RE = Pattern.compile(
            "(?s)<div[^>]*class=\"[^\"]*list-group-item[^\"]*\"[^>]*>(.*?)(?:</div>\\s*){3}");
    private static final Pattern FROM_RE = Pattern.compile(
            "(?s)class=\"[^\"]*from_div_45g45gg[^\"]*\"[^>]*>(.*?)</div>");
    private static final Pattern SUBJ_RE = Pattern.compile(
            "(?s)class=\"[^\"]*subj_div_45g45gg[^\"]*\"[^>]*>(.*?)</div>");
    private static final Pattern TIME_RE = Pattern.compile(
            "(?s)class=\"[^\"]*time_div_45g45gg[^\"]*\"[^>]*>(.*?)</div>");

    /** 去 script/style / 去标签 / 空白压缩正则。 */
    private static final Pattern SCRIPT_RE = Pattern.compile("(?is)<(script|style)[\\s\\S]*?</\\1>");
    private static final Pattern TAG_RE = Pattern.compile("<[^>]+>");
    private static final Pattern WS_RE = Pattern.compile("\\s+");

    /** 类内静态会话 Cookie 串（"k=v; k2=v2"），响应 Set-Cookie 逐次覆写合并。 */
    private static final Object COOKIE_LOCK = new Object();
    private static String sessionCookies = "";

    /** 私构：静态工具类不可实例化。 */
    private GeneratorEmail() {
    }

    /**
     * 解析响应 Set-Cookie 并按键合并进会话串（同键覆写）。
     *
     * @param existing 现有会话串
     * @param resp     响应
     * @return 合并后的会话串
     */
    private static String mergeCookies(String existing, HttpResult resp) {
        Map<String, String> map = new LinkedHashMap<>();
        for (String part : existing.split(";")) {
            int eq = part.trim().indexOf('=');
            if (eq > 0) {
                map.put(part.trim().substring(0, eq).trim(), part.trim().substring(eq + 1).trim());
            }
        }
        for (String raw : resp.getSetCookies()) {
            String head = raw.split(";", 2)[0].trim();
            int eq = head.indexOf('=');
            if (eq >= 0) {
                map.put(head.substring(0, eq).trim(), head.substring(eq + 1).trim());
            }
        }
        StringBuilder sb = new StringBuilder();
        for (Map.Entry<String, String> e : map.entrySet()) {
            if (sb.length() > 0) {
                sb.append("; ");
            }
            sb.append(e.getKey()).append('=').append(e.getValue());
        }
        return sb.toString();
    }

    /** 读取类内会话 Cookie 串（线程安全）。 */
    private static String currentCookies() {
        synchronized (COOKIE_LOCK) {
            return sessionCookies;
        }
    }

    /** 写回类内会话 Cookie 串（线程安全）。 */
    private static void updateCookies(String cookies) {
        synchronized (COOKIE_LOCK) {
            sessionCookies = cookies;
        }
    }

    /**
     * 请求收件箱渲染页并返回 HTML（附带浏览器级头部与类内会话 Cookie；
     * 邮箱由服务端依据 inbox_ctx Cookie 选择）。
     *
     * @return 页面 HTML
     */
    private static String fetchPage() {
        Map<String, String> h = new LinkedHashMap<>();
        h.put("User-Agent", UA);
        h.put("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8");
        h.put("Accept-Language", "en-US,en;q=0.9");
        h.put("Referer", BASE_URL + "/");
        h.put("Cookie", currentCookies());
        HttpResult resp = HttpClient.get(INBOX_URL, h);
        updateCookies(mergeCookies(currentCookies(), resp));
        if (!resp.isOk()) {
            throw new RuntimeException("generator-email: 请求失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        return resp.getBody();
    }

    /** 正则提取第一个捕获组（源文本空时返回空串）。 */
    private static String match1(Pattern re, String src) {
        if (src == null || src.isEmpty()) {
            return "";
        }
        Matcher m = re.matcher(src);
        return m.find() ? m.group(1) : "";
    }

    /** 去标签与脚本/样式块，压缩空白。 */
    private static String stripTags(String s) {
        String cleaned = SCRIPT_RE.matcher(s).replaceAll(" ");
        cleaned = TAG_RE.matcher(cleaned).replaceAll(" ");
        return WS_RE.matcher(cleaned).replaceAll(" ").trim();
    }

    /**
     * 创建 generator.email 临时邮箱：解析首页 SITE_DATA 快照
     * （cur_user/cur_domain）得到邮箱地址，token 保存
     * {email, domain, user} JSON 快照。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        String src = fetchPage();
        String user = match1(USER_RE, src).trim();
        String domain = match1(DOMAIN_RE, src).trim();
        if (user.isEmpty() || domain.isEmpty()) {
            throw new RuntimeException("generator-email: 首页未携带邮箱快照（cur_user/cur_domain）");
        }
        String email = user + "@" + domain;
        Map<String, Object> sess = new LinkedHashMap<>();
        sess.put("email", email);
        sess.put("domain", domain);
        sess.put("user", user);
        return new EmailInfo(CHANNEL, email, Json.serialize(sess), null, null);
    }

    /**
     * 获取 generator.email 收件箱：解析收件箱渲染页的列表条目
     * （from/subj/time 三要素）；本站不提供原文正文，SDK 按摘要归一。
     *
     * @param token 会话凭据 JSON（{email, domain, user}）
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        JsonObject sess;
        try {
            sess = Json.parseObject(token != null ? token : "");
        } catch (RuntimeException e) {
            throw new RuntimeException("generator-email: 会话凭据解析失败", e);
        }
        if (sess == null) {
            throw new RuntimeException("generator-email: 会话凭据解析失败");
        }
        if (!Json.str(sess, "email").equals(email)) {
            throw new RuntimeException("generator-email: 会话邮箱与查询邮箱不匹配");
        }
        String domain = Json.str(sess, "domain");

        String src = fetchPage();
        // 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换
        if (!match1(DOMAIN_RE, src).equals(domain)) {
            throw new RuntimeException("generator-email: 会话域名已切换（token " + domain
                    + "，服务端 " + match1(DOMAIN_RE, src) + "）");
        }

        // 列表区域锚定（#email-table ... #markodile 之间）
        int listStart = src.indexOf("id=\"email-table\"");
        int listEnd = src.indexOf("id=\"markodile\"");
        String region = "";
        if (listStart >= 0 && listEnd > listStart) {
            region = src.substring(listStart, listEnd);
        }

        List<Email> out = new ArrayList<>();
        Matcher m = ITEM_RE.matcher(src);
        while (m.find()) {
            String raw = m.group(1);
            // 跳过列表容器外的候选：要求条目文本来自列表区域
            if (!region.isEmpty() && !region.contains(raw)) {
                continue;
            }
            String from = stripTags(match1(FROM_RE, raw));
            String subject = stripTags(match1(SUBJ_RE, raw));
            String when = stripTags(match1(TIME_RE, raw));
            if (from.isEmpty() && subject.isEmpty() && when.isEmpty()) {
                continue;
            }
            Map<String, Object> flat = new LinkedHashMap<>();
            flat.put("from", from);
            flat.put("to", email);
            flat.put("subject", subject);
            flat.put("date", when);
            out.add(Normalizer.normalizeEmail(flat, email));
        }
        return out;
    }
}