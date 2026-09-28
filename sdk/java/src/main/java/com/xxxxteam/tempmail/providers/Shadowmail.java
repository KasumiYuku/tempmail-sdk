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
import java.util.concurrent.ThreadLocalRandom;

/**
 * ShadowMail 渠道 — https://shadowmail.win
 *
 * <p>固定注册密码 "Abcd1234!"：POST /api/register
 * {"email":"sdk"+8 位随机小写字母+"@gmail.com","password":密码}（幂等
 * "Email already in use"）；POST /api/login 同 body 返回 "Successfull Login"
 * （注意此拼写）+ Set-Cookie: sessionId=&lt;uuid&gt;（纯 uuid）；
 * POST /api/new-address（空 body，Cookie: sessionId=&lt;uuid&gt;）建箱
 * {"address":"&lt;id&gt;@shadowmail.win"}。</p>
 *
 * <p>凭据串 token = "shadowmail|&lt;account&gt;|&lt;password&gt;|&lt;sessionId&gt;"
 * （按 | 拆 3 段）。读信 POST /api/get-emails {"address":&lt;地址&gt;} 带 Cookie，
 * 401/404 时重新登录换 sessionId 重试一次；响应 message 必须为 "Emails read"。</p>
 */
public final class Shadowmail {

    private static final String BASE_URL = "https://shadowmail.win";
    private static final String PASSWORD = "Abcd1234!";
    private static final String DOMAIN = "shadowmail.win";
    private static final String TOKEN_PREFIX = "shadowmail|";
    private static final String CHANNEL = "shadowmail";

    private Shadowmail() {
    }

    /**
     * 生成随机注册邮箱前缀（sdk+8 位小写字母）。
     *
     * @return 注册账号前缀
     */
    private static String randomAccount() {
        StringBuilder sb = new StringBuilder(11);
        sb.append("sdk");
        ThreadLocalRandom r = ThreadLocalRandom.current();
        for (int i = 0; i < 8; i++) {
            sb.append((char) ('a' + r.nextInt(26)));
        }
        return sb.toString();
    }

    /**
     * 携带显式 Cookie 的 JSON POST 请求。
     *
     * @param path   请求路径（以 / 开头）
     * @param body   请求体对象（经 gson 序列化）
     * @param cookie 显式 Cookie 头，可为空
     * @return 请求结果（用于读取 Set-Cookie）
     */
    private static HttpResult postJson(String path, Map<String, Object> body, String cookie) {
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Content-Type", "application/json");
        headers.put("Accept", "application/json");
        if (cookie != null && !cookie.isEmpty()) {
            headers.put("Cookie", cookie);
        }
        return HttpClient.post(BASE_URL + path,
                Json.serialize(body), "application/json", headers);
    }

    /**
     * 从 Set-Cookie 列表中提取 sessionId 值（纯 uuid，不含键名）。
     *
     * @param resp 响应
     * @return sessionId 值，未下发返回空串
     */
    private static String sessionFromSetCookie(HttpResult resp) {
        for (String sc : resp.getSetCookies()) {
            String kv = sc;
            int semicolon = sc.indexOf(';');
            if (semicolon > 0) {
                kv = sc.substring(0, semicolon);
            }
            String trimmed = kv.trim();
            if (trimmed.startsWith("sessionId=")) {
                return trimmed.substring("sessionId=".length());
            }
        }
        return "";
    }

    /**
     * 注册或登录（POST /api/register、/api/login）。
     *
     * @param account   注册邮箱（平台外通信地址 + 账号主体）
     * @param password  固定密码
     * @param isLogin   为 true 走登录（要求 message=="Successfull Login" 并下发 sessionId）
     * @return 会话 sessionId（注册阶段可能为空串）
     */
    private static String registerOrLogin(String account, String password, boolean isLogin) {
        String path = isLogin ? "/api/login" : "/api/register";
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("email", account);
        body.put("password", password);
        HttpResult resp = postJson(path, body, "");
        if (!resp.isOk()) {
            throw new RuntimeException("shadowmail " + path + ": http " + resp.getStatusCode());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("shadowmail " + path + ": 响应解析失败");
        }
        String message = Json.str(data, "message");
        if (isLogin) {
            if (!"Successfull Login".equals(message)) {
                throw new RuntimeException("shadowmail login: " + message);
            }
            String session = sessionFromSetCookie(resp);
            if (session.isEmpty()) {
                throw new RuntimeException("shadowmail login: 未下发 sessionId Cookie");
            }
            return session;
        }
        // 重复注册（幂等）：消息为 Email already in use 时视为账号已存在，继续走登录
        if (!"Successfully Registered".equals(message) && !"Email already in use".equals(message)) {
            throw new RuntimeException("shadowmail register: " + message);
        }
        return sessionFromSetCookie(resp);
    }

    /**
     * 创建临时邮箱：注册账号（幂等）→ 登录取 sessionId → 创建地址。
     * token 凭据串："shadowmail|&lt;account&gt;|&lt;password&gt;|&lt;sessionId&gt;"。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        String account = randomAccount() + "@gmail.com";

        // 1) 注册（幂等：已存在同名账号则跳过）
        registerOrLogin(account, PASSWORD, false);
        // 2) 登录取得 sessionId（纯 uuid，不合成键值对）
        String session = registerOrLogin(account, PASSWORD, true);
        // 3) 创建地址（每账号 12 槽）：SDK 层显式 Cookie 头传 sessionId
        HttpResult resp = postJson("/api/new-address", new LinkedHashMap<>(), "sessionId=" + session);
        if (!resp.isOk()) {
            throw new RuntimeException("shadowmail new-address: http " + resp.getStatusCode());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("shadowmail new-address: 响应解析失败");
        }
        String address = Json.str(data, "address").trim();
        if (address.isEmpty() || !address.toLowerCase().endsWith("@" + DOMAIN)) {
            throw new RuntimeException("shadowmail new-address: 响应缺少有效地址");
        }

        String token = TOKEN_PREFIX + account + "|" + PASSWORD + "|" + session;
        return new EmailInfo(CHANNEL, address.toLowerCase().trim(), token, null, null);
    }

    /**
     * 解析凭据串为 account/password/sessionId 三元组。
     *
     * @param token 建箱下发的凭据串
     * @return [account, password, sessionId]
     */
    private static String[] parseToken(String token) {
        if (token == null || !token.startsWith(TOKEN_PREFIX)) {
            throw new RuntimeException("shadowmail: token 格式错误");
        }
        String[] parts = token.substring(TOKEN_PREFIX.length()).split("\\|", -1);
        if (parts.length != 3) {
            throw new RuntimeException("shadowmail: token 字段缺失");
        }
        if (parts[0].isEmpty() || parts[1].isEmpty() || parts[2].isEmpty()) {
            throw new RuntimeException("shadowmail: token 凭据字段为空");
        }
        return parts;
    }

    /**
     * 读取收件箱（POST /api/get-emails，Cookie sessionId）。401/404 时
     * 以凭据内 account/password 重新登录换新 sessionId 重试一次；
     * 响应 message 必须为 "Emails read"，否则报错。
     *
     * @param token 建箱下发的凭据串
     * @param email 平台新地址（&lt;id&gt;@shadowmail.win）
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        String[] cred = parseToken(token);
        String session = cred[2];

        Map<String, Object> body = new LinkedHashMap<>();
        body.put("address", addr);
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Content-Type", "application/json");
        headers.put("Accept", "application/json");
        headers.put("Cookie", "sessionId=" + session);
        HttpResult resp = HttpClient.post(BASE_URL + "/api/get-emails",
                Json.serialize(body), "application/json", headers);

        // sessionId 最长 1 小时，过期后重登录重试一次
        if (resp.getStatusCode() == 401 || resp.getStatusCode() == 404) {
            String newSession = registerOrLogin(cred[0], cred[1], true);
            headers.put("Cookie", "sessionId=" + newSession);
            resp = HttpClient.post(BASE_URL + "/api/get-emails",
                    Json.serialize(body), "application/json", headers);
        }
        if (!resp.isOk()) {
            throw new RuntimeException("shadowmail get-emails: http " + resp.getStatusCode());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("shadowmail get-emails: 响应解析失败");
        }
        String message = Json.str(data, "message");
        if (!"Emails read".equals(message)) {
            throw new RuntimeException("shadowmail get-emails: " + message);
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
            JsonObject msg = item.getAsJsonObject();
            Map<String, Object> flat = new LinkedHashMap<>();
            flat.put("from", Json.str(msg, "sender"));
            flat.put("to", addr);
            flat.put("date", Json.str(msg, "created_at"));
            // 平台无 text/html 区分，body 为正文（默认按纯文本处理，归一化可按需互转）
            flat.put("text", Json.str(msg, "body"));
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }
}