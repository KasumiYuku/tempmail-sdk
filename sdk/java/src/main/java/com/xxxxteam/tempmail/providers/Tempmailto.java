package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonArray;
import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.xxxxteam.tempmail.Email;
import com.xxxxteam.tempmail.EmailInfo;
import com.xxxxteam.tempmail.HttpClient;
import com.xxxxteam.tempmail.HttpResult;
import com.xxxxteam.tempmail.Json;

import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Tempmailto 渠道 — https://tempmailto.com（Laravel）。
 *
 * <p>首页由服务端渲染当前邮箱（{@code #mainEmail} 的 value），同一 Laravel 会话内不变，
 * 建箱只须 GET 首页 + 提取；POST /get_messages（表单 {@code _token} + captcha 留空）
 * 返回 {status, mailbox, email_token, messages, histories}；详情页为站内 GET /view/{id}。</p>
 *
 * <p>Java 的 HttpClient 默认无 Cookie 罐，本类在类内维护静态会话 Cookie 串，
 * 每次响应的 Set-Cookie 逐键合并（同键覆写）后随后续请求显式携带；
 * Laravel 每次请求都可能轮换会话 Cookie，遵循「逐响应覆写」语义。</p>
 *
 * <p>邮箱由会话 Cookie 承载、无独立密钥，SDK 层约定 Token=邮箱（注册表以 token
 * 非空作防御）；读信时当前邮箱与请求邮箱不一致则用 change 以请求邮箱名拉回。</p>
 */
public final class Tempmailto {

    private static final String BASE_URL = "https://tempmailto.com";
    private static final String CHANNEL = "tempmailto";

    /** 固定浏览器 UA（Chrome 154 / Linux，与各端一致）。 */
    private static final String UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36"
            + " (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /** 首页渲染邮箱正则（DOTALL + 忽略大小写）。 */
    private static final Pattern MAIN_EMAIL_RE =
            Pattern.compile("(?is)id=\"mainEmail\"[^>]*\\bvalue=\"([^\"]+)\"");

    /** 去 script/style 正则（DOTALL 语义）。 */
    private static final Pattern SCRIPT_STYLE_RE = Pattern.compile("(?is)<(script|style)[\\s\\S]*?</\\1>");

    /** 去标签正则。 */
    private static final Pattern TAG_RE = Pattern.compile("(?s)<[^>]+>");

    /** 空白压缩正则。 */
    private static final Pattern WS_RE = Pattern.compile("\\s+");

    /**
     * 类内静态会话 Cookie 状态（键值合并后的 "k=v; k2=v2" 串）。
     *
     * <p>Laravel 每次请求可能轮换会话 Cookie，须逐响应覆写并随后续
     * 请求显式携带。访问与覆写均持有 {@link #COOKIE_LOCK}，保证并发
     * 读写下会话串的可见性与原子更新。</p>
     */
    private static final Object COOKIE_LOCK = new Object();
    private static String sessionCookies = "";

    /**
     * 解析响应 Set-Cookie 头（取首分号之前的 name=value），与已有会话串
     * 按键合并（同键覆写），返回新的 Cookie 请求头值。
     *
     * @param existing 已有会话串
     * @param resp     响应
     * @return 合并后的 Cookie 请求头值
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
            if (eq > 0) {
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

    /** 私有构造，防止实例化。 */
    private Tempmailto() {
    }

    /**
     * 设置浏览器特征头（同站调用：Origin/Referer 指向首页）。
     *
     * @param h      请求头
     * @param accept Accept 值
     */
    private static void browserHeaders(Map<String, String> h, String accept) {
        h.put("User-Agent", UA);
        h.put("Accept", accept);
        h.put("Accept-Language", "en-US,en;q=0.9");
        h.put("Origin", BASE_URL);
        h.put("Referer", BASE_URL + "/");
    }

    /**
     * 读取会话 Cookie（线程安全，外部无需取锁）。
     *
     * @return 会话 Cookie 串，空串表示会话未建立
     */
    private static String currentCookies() {
        synchronized (COOKIE_LOCK) {
            return sessionCookies;
        }
    }

    /**
     * 写回会话 Cookie（线程安全，响应返回后调用）。
     *
     * @param cookies 最新合并结果
     */
    private static void updateCookies(String cookies) {
        synchronized (COOKIE_LOCK) {
            sessionCookies = cookies;
        }
    }

    /**
     * 创建 tempmailto 临时邮箱：GET 首页建立 Cookie 会话 + 提取服务端渲染的
     * 当前邮箱，Token 约定为邮箱本身（注册表需 token 非空）。邮箱约 10 分钟
     * 无活动过期。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        Map<String, String> h = new LinkedHashMap<>();
        browserHeaders(h, "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8");
        h.put("Cookie", currentCookies());
        HttpResult resp = HttpClient.get(BASE_URL, h);
        updateCookies(mergeCookies(currentCookies(), resp));
        if (!resp.isOk()) {
            throw new RuntimeException("tempmailto: 建立会话失败: http " + resp.getStatusCode());
        }
        String email = match1(MAIN_EMAIL_RE, resp.getBody()).trim();
        if (email.isEmpty()) {
            throw new RuntimeException("tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱");
        }
        return new EmailInfo(CHANNEL, email, email, null, null);
    }

    /**
     * 正则提取第一个捕获组（去空白）。
     *
     * @param re  正则
     * @param src 源文本
     * @return 捕获组内容，未命中返回空串
     */
    private static String match1(Pattern re, String src) {
        Matcher m = re.matcher(src == null ? "" : src);
        return m.find() ? m.group(1) : "";
    }

    /**
     * GET 首页并按 meta csrf-token 提取 CSRF _token（读信/换箱共用，
     * 响应 Set-Cookie 即时并入会话）。
     *
     * @return CSRF _token
     */
    private static String csrfFromHome() {
        Map<String, String> h = new LinkedHashMap<>();
        browserHeaders(h, "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8");
        h.put("Cookie", currentCookies());
        HttpResult resp = HttpClient.get(BASE_URL, h);
        updateCookies(mergeCookies(currentCookies(), resp));
        String csrf = match1(CSRF_RE, resp.getBody());
        if (csrf.isEmpty()) {
            throw new RuntimeException("tempmailto: 首页未找到 csrf-token");
        }
        return csrf;
    }

    /** CSRF meta 标签正则。 */
    private static final Pattern CSRF_RE =
            Pattern.compile("<meta\\s+name=\"csrf-token\"\\s+content=\"([^\"]+)\"");

    /**
     * POST /change 换箱（_token + name + domain），响应 Set-Cookie 并入会话。
     *
     * @param csrf  当前 CSRF _token
     * @param name  目标邮箱本地名
     * @param domain 目标邮箱域名
     * @return 变更后的当前邮箱
     */
    private static String change(String csrf, String name, String domain) {
        Map<String, String> h = new LinkedHashMap<>();
        browserHeaders(h, "application/json, text/plain, */*");
        h.put("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8");
        h.put("X-Requested-With", "XMLHttpRequest");
        h.put("Cookie", currentCookies());
        String form = "_token=" + ProviderUtil.urlEncode(csrf)
                + "&name=" + ProviderUtil.urlEncode(name)
                + "&domain=" + ProviderUtil.urlEncode(domain);
        HttpResult resp = HttpClient.post(BASE_URL + "/change",
                form, "application/x-www-form-urlencoded; charset=UTF-8", h);
        updateCookies(mergeCookies(currentCookies(), resp));
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("tempmailto: change 响应异常: " + resp.getBody().trim());
        }
        String mailbox = Json.str(data, "mailbox").trim();
        if (mailbox.isEmpty()) {
            throw new RuntimeException("tempmailto: change 响应异常");
        }
        return mailbox;
    }

    /**
     * POST /get_messages：_token=CSRF + captcha 留空（200 即成功）。
     * 响应 Set-Cookie 并入会话。
     *
     * @param csrf 当前 CSRF _token
     * @return 完整响应 JSON
     */
    private static JsonObject fetchMessages(String csrf) {
        Map<String, String> h = new LinkedHashMap<>();
        browserHeaders(h, "application/json, text/plain, */*");
        h.put("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8");
        h.put("X-Requested-With", "XMLHttpRequest");
        h.put("Cookie", currentCookies());
        String form = "_token=" + ProviderUtil.urlEncode(csrf) + "&captcha=";
        HttpResult resp = HttpClient.post(BASE_URL + "/get_messages",
                form, "application/x-www-form-urlencoded; charset=UTF-8", h);
        updateCookies(mergeCookies(currentCookies(), resp));
        if (!resp.isOk()) {
            throw new RuntimeException("tempmailto 读信: http " + resp.getStatusCode());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("tempmailto: 解析读信响应失败");
        }
        return data;
    }

    /**
     * GET /view/{id} 提取邮件正文 HTML（同 Cookie 会话）。
     * 平台视图页结构以候选 class 依次尝试，全失败回退 main/article 区块，
     * 仍失败返回空串（列表归一不因此中断）。
     *
     * @param id 邮件 id
     * @return 正文 HTML 片段，失败返回空串
     */
    private static String viewDetail(String id) {
        Map<String, String> h = new LinkedHashMap<>();
        browserHeaders(h, "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8");
        h.put("Cookie", currentCookies());
        HttpResult resp = HttpClient.get(BASE_URL + "/view/" + id, h);
        updateCookies(mergeCookies(currentCookies(), resp));
        if (!resp.isOk()) {
            return "";
        }
        String[] classNames = {"mail-body", "mail_content", "email-body",
                "content-body", "message-content", "mail-content"};
        for (String cls : classNames) {
            Pattern p = Pattern.compile("(?is)<(?:div|section|article)[^>]*class=\"[^\"]*\\b"
                    + Pattern.quote(cls) + "\\b[^\"]*\"[^>]*>([\\s\\S]*?)</(?:div|section|article)>");
            String got = match1(p, resp.getBody()).trim();
            if (!got.isEmpty()) {
                return got;
            }
        }
        Pattern fallback = Pattern.compile("(?is)<(main|article)[^>]*>([\\s\\S]*?)</\\1>");
        return match1(fallback, resp.getBody()).trim();
    }

    /**
     * 将 messages 列表单行组装为统一 Email。详情页 /view/{id} 提取正文 HTML，
     * 失败回退列表字段；text 兜底为 subject；html 兜底为 pre 包裹的 text。
     *
     * @param row   messages 单元素
     * @param email 请求邮箱
     * @return 标准化邮件；id 缺失返回 null
     */
    private static Email buildEmail(JsonObject row, String email) {
        String id = getStr(row, "id");
        if (id.isEmpty()) {
            return null;
        }
        String fromName = getStr(row, "from_name");
        if (fromName.isEmpty()) {
            fromName = getStr(row, "from_email", "from");
        }
        String date = getStr(row, "receivedAt", "received_at", "createdAt");
        if (date.isEmpty()) {
            date = Instant.now().toString();
        }
        Email ne = new Email();
        ne.setId(id);
        ne.setFrom(fromName);
        ne.setTo(email);
        ne.setSubject(getStr(row, "subject"));
        ne.setDate(date);
        ne.setRead(readBool(row, "is_seen"));
        String html = viewDetail(id);
        String text = htmlToText(html);
        if (!html.isEmpty()) {
            ne.setHtml(html);
        }
        if (!text.isEmpty()) {
            ne.setText(text);
        }
        if (ne.getText().isEmpty()) {
            ne.setText(getStr(row, "body", "text", "snippet", "preview"));
        }
        if (ne.getText().isEmpty()) {
            ne.setText(ne.getSubject());
        }
        if (ne.getHtml().isEmpty()) {
            ne.setHtml("<html><body><pre>" + escapeHtml(ne.getText()) + "</pre></body></html>");
        }
        return ne;
    }

    /**
     * 按候选键顺序提取字符串值。
     *
     * @param obj  JSON 对象
     * @param keys 候选键
     * @return 字符串值，未命中返回空串
     */
    private static String getStr(JsonObject obj, String... keys) {
        for (String key : keys) {
            if (obj != null && obj.has(key) && !obj.get(key).isJsonNull()) {
                String s = Json.nodeToString(obj.get(key)).trim();
                if (!s.isEmpty()) {
                    return s;
                }
            }
        }
        return "";
    }

    /**
     * 解析 is_seen 为布尔已读（bool / 数值非 0 / 字符串 "1"|"true" 忽略大小写）。
     *
     * @param obj  JSON 对象
     * @param keys 候选键
     * @return 布尔已读
     */
    private static boolean readBool(JsonObject obj, String... keys) {
        if (obj == null) {
            return false;
        }
        for (String key : keys) {
            if (!obj.has(key) || obj.get(key).isJsonNull()) {
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
                    String s = v.getAsString().trim();
                    return "1".equals(s) || "true".equalsIgnoreCase(s);
                }
            }
        }
        return false;
    }

    /**
     * 将 HTML 转为纯文本（去 script/style/标签、反转义、压缩空白）。
     *
     * @param src HTML 文本
     * @return 纯文本
     */
    private static String htmlToText(String src) {
        String cleaned = SCRIPT_STYLE_RE.matcher(src).replaceAll(" ");
        cleaned = TAG_RE.matcher(cleaned).replaceAll(" ");
        return WS_RE.matcher(unescapeHtml(cleaned)).replaceAll(" ").trim();
    }

    /**
     * 标准 HTML 实体反转义。
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

    /**
     * 读取 tempmailto 当前邮箱的收件箱。会话当前邮箱与请求邮箱不一致时
     * 用 change 以请求邮箱名分拆 name/domain 拉回；拉回后重新 get_messages。
     *
     * @param token GetEmails 接口传入的 token（与 email 一致时校验通过）
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("tempmailto: 邮箱为空，请重新 Generate");
        }
        JsonObject data = fetchMessages(csrfFromHome());

        // 会话当前邮箱与请求目标不一致 -> change 拉回并重新读信
        String mailbox = Json.str(data, "mailbox").trim();
        if (!mailbox.isEmpty() && !mailbox.equalsIgnoreCase(addr)) {
            int at = addr.lastIndexOf('@');
            String name = at > 0 ? addr.substring(0, at) : "TmSdk";
            String domain = at >= 0 && at + 1 < addr.length() ? addr.substring(at + 1) : "tempmailto.com";
            String changed = change(csrfFromHome(), name, domain);
            if (!changed.equalsIgnoreCase(addr)) {
                throw new RuntimeException("tempmailto: 会话邮箱无法拉回请求邮箱");
            }
            data = fetchMessages(csrfFromHome());
        }

        List<Email> out = new ArrayList<>();
        JsonArray messages = Json.arr(data, "messages");
        if (messages != null) {
            for (JsonElement item : messages) {
                if (!item.isJsonObject()) {
                    continue;
                }
                Email ne = buildEmail(item.getAsJsonObject(), addr);
                if (ne != null) {
                    out.add(ne);
                }
            }
        }
        return out;
    }
}