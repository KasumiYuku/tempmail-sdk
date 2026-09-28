package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonArray;
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
 * ShitPost.email 渠道 — https://shitpost.email（克隆自 shamu4life/throwaway-email 公共实例）
 *
 * <p>无认证 REST：POST /api/create 建箱（username/domain/ttl → email/token/type/expires），
 * GET /api/inbox?email=&amp;token= 读信（messages[] 含 from/fromName/subject/text/html/date）。
 * 域名池：shitpost.email / letsfuckingpiss.party。</p>
 */
public final class ShitpostEmail {

    private static final String BASE_URL = "https://shitpost.email";
    private static final String CHANNEL = "shitpost-email";

    private static final String[] DOMAINS = {"shitpost.email", "letsfuckingpiss.party"};

    private ShitpostEmail() {
    }

    /**
     * 生成 "sdk" 前缀 + 10 位随机小写字母数字的本地名。
     *
     * @return 本地名
     */
    private static String localPart() {
        return "sdk" + ProviderUtil.randomString(10);
    }

    /**
     * 创建 shitpost.email 临时邮箱（POST /api/create）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        String domain = DOMAINS[ThreadLocalRandom.current().nextInt(DOMAINS.length)];
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("username", localPart());
        body.put("domain", domain);
        body.put("ttl", 3600);

        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.post(BASE_URL + "/api/create",
                Json.serialize(body), "application/json", headers);
        resp.ensureSuccess();

        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("shitpost-email: 创建邮箱响应解析失败");
        }
        String email = Json.str(data, "email").trim();
        String token = Json.str(data, "token").trim();
        if (email.isEmpty() || token.isEmpty()) {
            throw new RuntimeException("shitpost-email: 创建邮箱响应缺少 email 或 token");
        }
        String expires = Json.str(data, "expires").trim();
        // expires 为 Unix 秒时间戳，转毫秒
        Long expiresMs = null;
        if (!expires.isEmpty()) {
            try {
                expiresMs = Long.parseLong(expires) * 1000;
            } catch (NumberFormatException ignored) {
                // 保留 null
            }
        }
        return new EmailInfo(CHANNEL, email, token, expiresMs, null);
    }

    /**
     * 读取收件箱（GET /api/inbox?email=&amp;token=）。
     *
     * @param token 建箱返回的令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        String addr = (email != null ? email : "").trim();
        if (tok.isEmpty() || addr.isEmpty()) {
            throw new RuntimeException("shitpost-email: email 或 token 为空");
        }
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.get(BASE_URL + "/api/inbox?email="
                + ProviderUtil.urlEncode(addr) + "&token=" + ProviderUtil.urlEncode(tok), headers);
        resp.ensureSuccess();

        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            return new ArrayList<>();
        }
        JsonElement messagesEl = data.get("messages");
        if (messagesEl == null || !messagesEl.isJsonArray()) {
            return new ArrayList<>();
        }
        JsonArray messages = messagesEl.getAsJsonArray();
        List<Email> out = new ArrayList<>();
        for (JsonElement item : messages) {
            if (!item.isJsonObject()) {
                continue;
            }
            JsonObject msg = item.getAsJsonObject();
            Map<String, Object> flat = new LinkedHashMap<>();
            flat.put("from", Json.str(msg, "from"));
            flat.put("to", addr);
            flat.put("text", Json.str(msg, "text"));
            flat.put("html", Json.str(msg, "html"));
            flat.put("date", Json.str(msg, "date"));
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }
}