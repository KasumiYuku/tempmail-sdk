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

/**
 * 渠道实现 — https://tmpkit.com（Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）。
 *
 * <p>所有操作均走 POST /api/rpc/tempmail/{procedure}，body 为 tRPC 包裹
 * {@code {"json":{...}}}，免凭据。initSession 响应带 Set-Cookie
 * tempmail_session=sessionId（Max-Age=3600），token 保存该 sessionId
 * 并以显式 Cookie 头逐请求携带；getEmails 响应 {"json":{"emails":[...],
 * total, session:{email, expiresIn}}}，列表元素键为 mailId/from/subject/
 * excerpt/date/timestamp/hasAttach/isRead（无 id 键）；getEmailDetail 的
 * body 为 {"json":{"mailId":数字}}（mailId 必须为数字，字符串会 zod 400），
 * 响应为单封详情对象（含 body 正文）。</p>
 *
 * <p>Java 的 HttpClient 默认无 Cookie 罐，本类在类内维护静态会话 Cookie 串，
 * 每次响应的 Set-Cookie 逐键合并（同键覆写）后随后续请求显式携带。</p>
 */
public final class Tmpkit {

    private static final String BASE_URL = "https://tmpkit.com";
    private static final String RPC_PREFIX = BASE_URL + "/api/rpc/tempmail";
    private static final String CHANNEL = "tmpkit";

    /** 固定浏览器 UA（Chrome 154 / Linux，与各端一致）。 */
    private static final String UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36"
            + " (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /** 类内静态会话 Cookie 串（"k=v; k2=v2"），响应 Set-Cookie 逐次覆写合并。 */
    private static final Object COOKIE_LOCK = new Object();
    private static String sessionCookies = "";

    /** 私构：静态工具类不可实例化。 */
    private Tmpkit() {
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

    /**
     * tRPC 请求头（同站 fetch 形态）。
     *
     * @return 请求头映射
     */
    private static Map<String, String> rpcHeaders() {
        Map<String, String> h = new LinkedHashMap<>();
        h.put("User-Agent", UA);
        h.put("Accept", "*/*");
        h.put("Content-Type", "application/json");
        h.put("Origin", BASE_URL);
        h.put("Referer", BASE_URL + "/en");
        return h;
    }

    /**
     * 对 tmpkit 发起 rpc 调用（body 为 tRPC 包裹 {"json":<reqBody>}）。
     * 响应外层 {"json":{...}} 解析为字典返回（数字转 Long）。
     *
     * @param procedure rpc 过程名（initSession / getEmails / getEmailDetail）
     * @param reqBody   过程参数 map，可为空 map
     * @return 响应的 json 载荷（map）
     */
    private static Map<String, Object> rpc(String procedure, Map<String, Object> reqBody) {
        String payload = Json.serialize(mapOf("json", reqBody));
        Map<String, String> h = rpcHeaders();
        h.put("Cookie", currentCookies());
        HttpResult resp = HttpClient.post(RPC_PREFIX + "/" + procedure,
                payload, "application/json", h);
        updateCookies(mergeCookies(currentCookies(), resp));
        if (!resp.isOk()) {
            throw new RuntimeException("tmpkit: " + procedure + " 失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject outer = Json.parseObject(resp.getBody());
        if (outer == null || outer.get("json") == null || !outer.get("json").isJsonObject()) {
            throw new RuntimeException("tmpkit: " + procedure + " 响应缺 json 载荷: "
                    + resp.getBody().trim());
        }
        return Json.toDict(outer.get("json"));
    }

    /** 构造键值 map（保持插入顺序）。 */
    private static Map<String, Object> mapOf(String key, Object value) {
        Map<String, Object> map = new LinkedHashMap<>();
        map.put(key, value);
        return map;
    }

    /**
     * 创建 tmpkit.com 临时邮箱（initSession，tRPC 包裹 {"json":{}}）。
     * sessionId 与邮箱地址同返，token 约定为 tempMailSession=<sessionId>。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        Map<String, Object> data = rpc("initSession", new LinkedHashMap<>());
        Object sessEl = data.get("session");
        if (!(sessEl instanceof Map)) {
            throw new RuntimeException("tmpkit: 创建会话响应缺 session 字段");
        }
        Map<?, ?> sess = (Map<?, ?>) sessEl;
        String email = strOf(sess.get("email"), "").trim();
        String sessionId = strOf(sess.get("sessionId"), "").trim();
        if (email.isEmpty() || sessionId.isEmpty() || !email.contains("@")) {
            throw new RuntimeException("tmpkit: 创建会话响应缺少必要字段（email/sessionId）");
        }
        return new EmailInfo(CHANNEL, email, "tempMailSession=" + sessionId, null, null);
    }

    /**
     * 获取 tmpkit.com 邮件列表。getEmails（offset 0 / limit 20）取摘要，
     * 逐封 getEmailDetail 拉详情并并入摘要（详情失败回退列表摘要）；
     * getEmails 返回的 session.email 与收信邮箱不符时报错，防止会话被覆盖。
     *
     * @param token 会话凭据串（tempMailSession=<sessionId>）
     * @param email 邮箱地址
     * @return 邮件列表
     */
    @SuppressWarnings("unchecked")
    public static List<Email> getEmails(String token, String email) {
        String mailbox = (token != null ? token : "").trim();
        if (mailbox.startsWith("tempMailSession=")) {
            mailbox = mailbox.substring("tempMailSession=".length()).trim();
        }
        if (mailbox.isEmpty()) {
            throw new RuntimeException("tmpkit: 会话 token 为空");
        }
        // 显式覆写对域会话 Cookie，防全局罐被并行会话覆盖后串箱
        updateCookies("tempmail_session=" + mailbox);

        Map<String, Object> q = new LinkedHashMap<>();
        q.put("offset", 0L);
        q.put("limit", 20L);
        Map<String, Object> data = rpc("getEmails", q);

        // 会话指向校验：getEmails 响应自带 session.email（无 Cookie 时
        // session 为 null，同样视为会话失效）
        Object sessEl = data.get("session");
        if (sessEl instanceof Map) {
            Map<?, ?> sess = (Map<?, ?>) sessEl;
            String got = strOf(sess.get("email"), "").trim();
            if (!got.isEmpty() && !got.equals(email)) {
                throw new RuntimeException("tmpkit: 会话邮箱不匹配（响应 " + got + "，请求 " + email + "）");
            }
        } else {
            throw new RuntimeException("tmpkit: 会话已失效（getEmails 返回空会话）");
        }

        Object rowsEl = data.get("emails");
        if (!(rowsEl instanceof List)) {
            throw new RuntimeException("tmpkit: 邮件列表响应缺 emails 字段");
        }
        List<?> rows = (List<?>) rowsEl;
        List<Email> out = new ArrayList<>();
        for (Object item : rows) {
            if (!(item instanceof Map)) {
                continue;
            }
            Map<String, Object> m = new LinkedHashMap<>((Map<String, Object>) item);
            long mailId = longOf(m.get("mailId"));
            if (mailId > 0) {
                Map<String, Object> dq = new LinkedHashMap<>();
                dq.put("mailId", mailId);
                try {
                    Map<String, Object> detail = rpc("getEmailDetail", dq);
                    for (Map.Entry<String, Object> kv : detail.entrySet()) {
                        String key = kv.getKey();
                        if ("session".equals(key) || "emails".equals(key)
                                || "total".equals(key) || "error".equals(key)) {
                            continue;
                        }
                        m.put(key, kv.getValue());
                    }
                } catch (RuntimeException ignored) {
                    // 详情失败回退列表摘要
                }
            }
            out.add(Normalizer.normalizeEmail(m, email));
        }
        return out;
    }

    /** 对象容错转字符串（数字取十进制整数，布尔取 true/false，其余 String.valueOf）。 */
    private static String strOf(Object v, String fallback) {
        if (v == null) {
            return fallback;
        }
        if (v instanceof Number) {
            double d = ((Number) v).doubleValue();
            if (d == Math.floor(d) && !Double.isInfinite(d)) {
                return Long.toString((long) d);
            }
            return Double.toString(d);
        }
        return String.valueOf(v);
    }

    /** 对象容错转 long（非数字返回 0）。 */
    private static long longOf(Object v) {
        if (v instanceof Number) {
            return ((Number) v).longValue();
        }
        return 0L;
    }
}