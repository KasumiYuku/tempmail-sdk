package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonElement;
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

/**
 * Internxt 渠道 — https://internxt.com/temporary-email（Next.js + OpenNext）。
 *
 * <p>建箱 GET /api/temp-mail/create-email，读信 GET /api/temp-mail/get-inbox
 * ?email=&amp;token=，详情 GET /api/temp-mail/get-message
 * ?email=&amp;token=&amp;messageId=。create-email 仅接受 GET（POST 405）。</p>
 *
 * <p>CSRF：首次 GET /temporary-email 响应 Set-Cookie csrfSecret=... 与
 * XSRF-TOKEN=...（均 HttpOnly）。数据接口校验请求头 csrf-token，其值必须与
 * Cookie 罐中 XSRF-TOKEN 一致（头=XSRF-TOKEN 值时 200；头=csrfSecret 值时
 * 恒定 500）。每个 API 响应均会刷新 XSRF-TOKEN 的 Set-Cookie，故每次读信前
 * 都应从罐中重取最新值作为 csrf-token 头（实测 csrfSecret 恒定，
 * XSRF-TOKEN 每次刷新）。</p>
 *
 * <p>Java 的 HttpClient 默认无 Cookie 罐，本类在类内维护静态会话 Cookie 串，
 * 每次响应的 Set-Cookie 逐键合并（同键覆写）后随后续请求显式携带。</p>
 */
public final class Internxt {

    private static final String SITE = "https://internxt.com";
    private static final String REF = SITE + "/temporary-email";
    private static final String API_BASE = SITE + "/api/temp-mail";
    private static final String CHANNEL = "internxt";

    /** 固定浏览器 UA（Chrome 154 / Linux，与各端一致）。 */
    private static final String UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36"
            + " (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /** 类内静态会话 Cookie 串（"k=v; k2=v2"），响应 Set-Cookie 逐次覆写合并。 */
    private static final Object COOKIE_LOCK = new Object();
    private static String sessionCookies = "";

    /** 私构：静态工具类不可实例化。 */
    private Internxt() {
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

    /** 从会话串中取指定 Cookie 名的最新值。 */
    private static String cookieValue(String name) {
        for (String part : currentCookies().split(";")) {
            String pair = part.trim();
            int eq = pair.indexOf('=');
            if (eq > 0 && name.equals(pair.substring(0, eq).trim())) {
                return pair.substring(eq + 1).trim();
            }
        }
        return "";
    }

    /**
     * 确保 Cookie 罐持有本域 csrfSecret 与 XSRF-TOKEN（无则 GET
     * 临时邮箱页夺取），并返回罐中最新 XSRF-TOKEN 值。每次 API 响应
     * 都会刷新该 Cookie，故调用方每次请求前都应重新调用。
     *
     * @return XSRF-TOKEN 值
     */
    private static String prepareXsrf() {
        String xsrf = cookieValue("XSRF-TOKEN");
        if (!xsrf.isEmpty()) {
            return xsrf;
        }
        Map<String, String> h = new LinkedHashMap<>();
        h.put("User-Agent", UA);
        h.put("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8");
        h.put("Accept-Language", "en-US,en;q=0.9");
        h.put("Cookie", currentCookies());
        try {
            HttpResult resp = HttpClient.get(REF, h);
            updateCookies(mergeCookies(currentCookies(), resp));
        } catch (RuntimeException ignored) {
            // 页面夺取失败不致命：仍尝试从罐中取 XSRF-TOKEN
        }
        xsrf = cookieValue("XSRF-TOKEN");
        if (xsrf.isEmpty()) {
            throw new RuntimeException("internxt: 未取得 XSRF-TOKEN Cookie");
        }
        return xsrf;
    }

    /**
     * 带 CSRF 头请求 internxt 数据接口（GET），返回响应。
     * csrf-token 头取罐中 XSRF-TOKEN 最新值（无则通过 prepareXsrf 夺取），
     * 请求后响应 Set-Cookie 覆写会话罐。
     *
     * @param path  接口路径（如 /create-email）
     * @param query 查询参数，可为 null
     * @return 响应
     */
    private static HttpResult get(String path, Map<String, String> query) {
        String csrfToken = prepareXsrf();
        StringBuilder url = new StringBuilder(API_BASE + path);
        if (query != null && !query.isEmpty()) {
            url.append('?');
            boolean first = true;
            for (Map.Entry<String, String> kv : query.entrySet()) {
                if (!first) {
                    url.append('&');
                }
                first = false;
                url.append(ProviderUtil.urlEncode(kv.getKey()))
                        .append('=')
                        .append(ProviderUtil.urlEncode(kv.getValue()));
            }
        }
        Map<String, String> h = new LinkedHashMap<>();
        h.put("User-Agent", UA);
        h.put("Accept", "application/json, text/plain, */*");
        h.put("Origin", SITE);
        h.put("Referer", REF);
        h.put("csrf-token", csrfToken);
        h.put("Cookie", currentCookies());
        HttpResult resp = HttpClient.get(url.toString(), h);
        updateCookies(mergeCookies(currentCookies(), resp));
        if (!resp.isOk()) {
            throw new RuntimeException("internxt: " + path + " 失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        return resp;
    }

    /**
     * 创建 internxt.com 临时邮箱：prepareXsrf 确保罐中 XSRF-TOKEN，再
     * GET /api/temp-mail/create-email（带 csrf-token 头），响应
     * {"address","token"}（token 为收信令牌），两者打包进会话凭据 JSON。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        prepareXsrf();
        HttpResult resp = get("/create-email", null);
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("internxt: 解析建箱响应失败");
        }
        String address = Json.str(data, "address").trim();
        String token = Json.str(data, "token").trim();
        if (address.isEmpty() || token.isEmpty() || !address.contains("@")) {
            throw new RuntimeException("internxt: 创建邮箱响应缺少必要字段（address/token）");
        }
        Map<String, Object> sessionMap = new LinkedHashMap<>();
        sessionMap.put("address", address);
        sessionMap.put("token", token);
        String session = Json.serialize(sessionMap);
        return new EmailInfo(CHANNEL, address, session, null, null);
    }

    /** 构造单键值 map（保持插入顺序）。 */
    private static Map<String, Object> mapOf(String key, Object value) {
        Map<String, Object> map = new LinkedHashMap<>();
        map.put(key, value);
        return map;
    }

    /** 从列表元素中提取邮件 ID（首选 id，回退 messageId/message_id）。 */
    private static String messageIdOf(Map<String, Object> m) {
        for (String key : new String[]{"id", "messageId", "message_id"}) {
            Object v = m.get(key);
            if (v != null) {
                String s = String.valueOf(v).trim();
                if (!s.isEmpty()) {
                    return s;
                }
            }
        }
        return "";
    }

    /**
     * 获取 internxt.com 收件箱：GET /api/temp-mail/get-inbox 返回顶层数组
     * （列表元素含 id/from/subject/date/seen 等）；逐条 GET get-message 拉
     * 单封全文（响应为单封对象，含 html 渲染全文），详情失败回退列表摘要。
     *
     * @param token 会话凭据 JSON（address/token）
     * @param email 邮箱地址
     * @return 邮件列表
     */
    @SuppressWarnings("unchecked")
    public static List<Email> getEmails(String token, String email) {
        JsonObject sess;
        try {
            sess = Json.parseObject(token != null ? token : "");
        } catch (RuntimeException e) {
            throw new RuntimeException("internxt: 会话凭据解析失败", e);
        }
        if (sess == null) {
            throw new RuntimeException("internxt: 会话凭据解析失败");
        }
        String address = Json.str(sess, "address").trim();
        String apiToken = Json.str(sess, "token").trim();
        if (address.isEmpty() || apiToken.isEmpty()) {
            throw new RuntimeException("internxt: 会话凭据缺少必要字段");
        }
        if (!address.equals(email)) {
            throw new RuntimeException("internxt: 会话邮箱与查询邮箱不匹配");
        }

        Map<String, String> q = new LinkedHashMap<>();
        q.put("email", address);
        q.put("token", apiToken);
        HttpResult resp = get("/get-inbox", q);

        // 收件箱为顶层数组（空箱 []），元素为列表摘要对象
        List<Object> rows = new ArrayList<>();
        JsonElement root = Json.parse(resp.getBody());
        if (root != null) {
            if (!root.isJsonArray()) {
                throw new RuntimeException("internxt: 收件箱响应不是顶层数组");
            }
            Object raw = Json.toRaw(root);
            if (raw instanceof List) {
                rows.addAll((List<?>) raw);
            }
        }

        List<Email> out = new ArrayList<>();
        for (Object rowObj : rows) {
            if (!(rowObj instanceof Map)) {
                continue;
            }
            Map<String, Object> m = new LinkedHashMap<>((Map<String, Object>) rowObj);
            String mid = messageIdOf(m);
            if (!mid.isEmpty()) {
                Map<String, String> dq = new LinkedHashMap<>();
                dq.put("email", address);
                dq.put("token", apiToken);
                dq.put("messageId", mid);
                try {
                    HttpResult dresp = get("/get-message", dq);
                    Map<String, Object> detail = Json.toDict(Json.parse(dresp.getBody()));
                    for (Map.Entry<String, Object> kv : detail.entrySet()) {
                        if (!m.containsKey(kv.getKey())) {
                            m.put(kv.getKey(), kv.getValue());
                        }
                    }
                } catch (RuntimeException ignored) {
                    // 详情失败回退列表摘要
                }
            }
            out.add(Normalizer.normalizeEmail(m, email));
        }
        return out;
    }
}