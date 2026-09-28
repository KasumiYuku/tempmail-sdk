package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.xxxxteam.tempmail.Email;
import com.xxxxteam.tempmail.EmailInfo;
import com.xxxxteam.tempmail.HttpResult;
import com.xxxxteam.tempmail.HttpClient;
import com.xxxxteam.tempmail.Json;
import com.xxxxteam.tempmail.Normalizer;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ThreadLocalRandom;

/**
 * NukeMail 渠道 — https://nukemail.app
 *
 * <p>SHA-256 PoW 建箱流程：
 * 1) GET /api/pow/challenge?difficulty=4 取挑战 {"id","challenge","difficulty"}；
 * 2) 本地求最小 nonce 使 SHA-256(challenge+nonce) 十六进制前缀为
 *    "0" 重复 difficulty 位（MessageDigest，nonce 自 0 递增）；
 * 3) GET /api/domains 取首个非 premium 域名；
 * 4) POST /api/inbox/create {"address","domain","pow_id","pow_nonce"} 建箱。</p>
 *
 * <p>读信 GET /api/inbox 带 Cookie: nukemail_token=&lt;token&gt;；
 * state 为 expired 或空时经 POST /api/inbox/resume 重设会话后重试一次。</p>
 */
public final class Nukemail {

    private static final String BASE_URL = "https://nukemail.app";
    private static final String CHANNEL = "nukemail";

    private Nukemail() {
    }

    /**
     * 求解 PoW：返回使 SHA-256(challenge+nonce) 十六进制前 difficulty 位
     * 为 0 的最小 nonce（与前端 solvePow 逐字对齐，无随机起点）。
     *
     * @param challenge  挑战串
     * @param difficulty 难度（十六进制前缀 0 位数）
     * @return nonce（十进制字符串）
     */
    private static String solvePow(String challenge, int difficulty) {
        String prefix = "0".repeat(difficulty);
        try {
            MessageDigest md = MessageDigest.getInstance("SHA-256");
            long nonce = 0;
            while (true) {
                byte[] digest = md.digest((challenge + nonce).getBytes(StandardCharsets.UTF_8));
                md.reset();
                if (toHex(digest).startsWith(prefix)) {
                    return String.valueOf(nonce);
                }
                nonce++;
            }
        } catch (Exception e) {
            throw new RuntimeException("nukemail: PoW 计算失败", e);
        }
    }

    /**
     * 字节数组转小写十六进制串。
     *
     * @param bytes 摘要字节
     * @return 十六进制串
     */
    private static String toHex(byte[] bytes) {
        StringBuilder sb = new StringBuilder(bytes.length * 2);
        for (byte b : bytes) {
            sb.append(Character.forDigit((b >> 4) & 0xF, 16));
            sb.append(Character.forDigit(b & 0xF, 16));
        }
        return sb.toString();
    }

    /**
     * 生成本地随机名（"nuke"+10 位随机 [a-z0-9]，与前端 generateRandomName 同形态）。
     *
     * @return 本地名
     */
    private static String randomAddress() {
        return "nuke" + ProviderUtil.randomString(10);
    }

    /**
     * 获取平台当前第一个非 premium 的活跃域名（GET /api/domains）。
     *
     * @return 域名
     */
    private static String activeDomain() {
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.get(BASE_URL + "/api/domains", headers);
        resp.ensureSuccess();
        JsonObject data = Json.parseObject(resp.getBody());
        JsonElement domainsEl = data != null ? data.get("domains") : null;
        if (domainsEl == null || !domainsEl.isJsonArray()) {
            throw new RuntimeException("nukemail generate: 解析域名列表失败");
        }
        for (JsonElement item : domainsEl.getAsJsonArray()) {
            if (!item.isJsonObject()) {
                continue;
            }
            JsonObject d = item.getAsJsonObject();
            if ("true".equals(Json.str(d, "is_premium_only"))) {
                continue;
            }
            String domain = Json.str(d, "domain").trim();
            if (!domain.isEmpty()) {
                return domain;
            }
        }
        throw new RuntimeException("nukemail generate: 无可用非 premium 域名");
    }

    /**
     * 创建临时邮箱（PoW 建箱）。token 为平台返回的 NUKE-&lt;随机&gt; 访问码，
     * 读信时转成 nukemail_token Cookie 携带。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        // 1) 取 PoW 挑战
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult chResp = HttpClient.get(BASE_URL + "/api/pow/challenge?difficulty=4", headers);
        if (!chResp.isOk()) {
            throw new RuntimeException("nukemail generate: challenge http "
                    + chResp.getStatusCode());
        }
        JsonObject ch = Json.parseObject(chResp.getBody());
        if (ch == null) {
            throw new RuntimeException("nukemail generate: challenge 响应解析失败");
        }
        String powId = Json.str(ch, "id").trim();
        String challenge = Json.str(ch, "challenge").trim();
        if (powId.isEmpty() || challenge.isEmpty()) {
            throw new RuntimeException("nukemail generate: challenge 响应缺少 id/challenge");
        }
        int difficulty = 4;
        try {
            int d = Integer.parseInt(Json.str(ch, "difficulty").trim());
            if (d > 0) {
                difficulty = d;
            }
        } catch (NumberFormatException ignored) {
            // difficulty 缺失/非法时使用默认 4
        }

        // 2) 本地求 PoW 解
        String nonce = solvePow(challenge, difficulty);

        // 3) 取域名并建箱
        String domain = activeDomain();
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("address", randomAddress());
        body.put("domain", domain);
        body.put("pow_id", powId);
        body.put("pow_nonce", nonce);
        HttpResult createResp = HttpClient.post(BASE_URL + "/api/inbox/create",
                Json.serialize(body), "application/json", headers);
        if (!createResp.isOk()) {
            throw new RuntimeException("nukemail generate: create http "
                    + createResp.getStatusCode() + ": " + createResp.getBody().trim());
        }
        JsonObject data = Json.parseObject(createResp.getBody());
        if (data == null) {
            throw new RuntimeException("nukemail generate: create 响应解析失败");
        }
        String token = Json.str(data, "token").trim();
        String email = Json.str(data, "email").trim();
        if (token.isEmpty() || email.isEmpty()) {
            throw new RuntimeException("nukemail generate: create 响应缺少 token/email");
        }
        return new EmailInfo(CHANNEL, email, token, null, null);
    }

    /**
     * 读取收件箱（GET /api/inbox，Cookie: nukemail_token=&lt;token&gt;）。
     * state 为 expired 或空时经 POST /api/inbox/resume 重设会话后重试一次。
     *
     * @param token 建箱返回的 NUKE-&lt;随机&gt; 访问码
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("nukemail: token 为空");
        }
        String addr = (email != null ? email : "").trim();
        String cookie = "nukemail_token=" + tok;

        JsonObject data = fetchInbox(cookie);
        boolean needRetry = data == null
                || data.get("state") == null
                || data.get("state").isJsonNull()
                || "expired".equals(Json.str(data, "state").trim())
                || Json.str(data, "state").trim().isEmpty();
        if (needRetry) {
            // 会话可能已过期：经 resume 重设会话后重试一次
            Map<String, Object> resumeBody = new LinkedHashMap<>();
            resumeBody.put("accessCode", tok);
            Map<String, String> resumeHeaders = new LinkedHashMap<>();
            resumeHeaders.put("Content-Type", "application/json");
            try {
                HttpClient.post(BASE_URL + "/api/inbox/resume",
                        Json.serialize(resumeBody), "application/json", resumeHeaders);
            } catch (RuntimeException ignored) {
                // resume 失败仍按主通道重试一次
            }
            data = fetchInbox(cookie);
        }
        if (data == null) {
            throw new RuntimeException("nukemail 读信: 解析收件箱响应失败");
        }

        JsonElement messagesEl = data.get("messages");
        if (messagesEl == null || !messagesEl.isJsonArray()) {
            return new ArrayList<>();
        }
        List<Email> out = new ArrayList<>();
        for (JsonElement item : messagesEl.getAsJsonArray()) {
            if (!item.isJsonObject()) {
                continue;
            }
            JsonObject msg = item.getAsJsonObject();
            Map<String, Object> flat = Json.toDict(msg);
            flat.put("to", addr);
            // 平台正文字段为 body_text/body_html，补到 text/html 候选（原字段缺失时）
            if (!flat.containsKey("text")) {
                Object bodyText = flat.get("body_text");
                if (bodyText != null) {
                    flat.put("text", bodyText);
                }
            }
            if (!flat.containsKey("html")) {
                Object bodyHtml = flat.get("body_html");
                if (bodyHtml != null) {
                    flat.put("html", bodyHtml);
                }
            }
            flat.put("date", toRaw(msg, "received_at"));
            flat.put("read", toRaw(msg, "read"));
            // sender 是小写发件人地址（Normalizer 用 sender_email/sender 提取）
            flat.put("sender_email", toRaw(msg, "sender"));
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }

    /**
     * 读取收件箱（GET /api/inbox，带显式 Cookie），失败返回 null。
     *
     * @param cookie 会话 Cookie
     * @return 收件箱 JSON 对象，失败返回 null
     */
    private static JsonObject fetchInbox(String cookie) {
        try {
            Map<String, String> headers = new LinkedHashMap<>();
            headers.put("Accept", "application/json");
            headers.put("Cookie", cookie);
            HttpResult resp = HttpClient.get(BASE_URL + "/api/inbox", headers);
            if (!resp.isOk()) {
                return null;
            }
            return Json.parseObject(resp.getBody());
        } catch (RuntimeException ignored) {
            return null;
        }
    }

    /**
     * 将 JSON 节点递归转为原生对象（经 Json.toRaw），缺失返回 null。
     *
     * @param obj JSON 对象
     * @param key 字段名
     * @return 原生值，缺失返回 null
     */
    private static Object toRaw(JsonObject obj, String key) {
        return Json.toRaw(obj.get(key));
    }
}