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

/**
 * TempMails.io 渠道 — https://tempmails.io
 *
 * <p>无认证 REST：POST /api/temp-mail/generate 建箱（响应 email/token/expires_at），
 * GET /api/temp-mail/inbox/{token} 读信（messages[] 含
 * from_email/text_body/html_body/attachments）。邮箱借用 uberip.com 等公共域（10 分钟自动过期）。</p>
 */
public final class TempmailsIo {

    private static final String BASE_URL = "https://tempmails.io";
    private static final String CHANNEL = "tempmails-io";

    private TempmailsIo() {
    }

    /**
     * 创建 tempmails.io 临时邮箱（POST /api/temp-mail/generate，空 JSON body）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Content-Type", "application/json");
        headers.put("Accept", "application/json");

        HttpResult resp = HttpClient.post(BASE_URL + "/api/temp-mail/generate",
                "{}", "application/json", headers);
        resp.ensureSuccess();

        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null || !Json.str(data, "success").equals("true")) {
            throw new RuntimeException("tempmails-io: 创建邮箱响应 success 缺失或为 false");
        }
        JsonElement dataEl = data.get("data");
        if (dataEl == null || !dataEl.isJsonObject()) {
            throw new RuntimeException("tempmails-io: 创建邮箱响应缺少 data 字段");
        }
        JsonObject inner = dataEl.getAsJsonObject();
        String email = Json.str(inner, "email").trim();
        String token = Json.str(inner, "token").trim();
        if (email.isEmpty() || token.isEmpty()) {
            throw new RuntimeException("tempmails-io: 创建邮箱响应缺少 email 或 token");
        }
        String expiresAt = Json.str(inner, "expires_at").trim();
        return new EmailInfo(CHANNEL, email, token,
                expiresAt.isEmpty() ? null : parseIsoMillis(expiresAt), null);
    }

    /**
     * 读取收件箱。先 POST /api/temp-mail/fetch-emails/{token} 触发平台对上游信箱的
     * 主动同步（失败不致命），再 GET /api/temp-mail/inbox/{token} 读取静态收件箱。
     *
     * @param token 建箱返回的令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("tempmails-io: token 为空");
        }

        // 1) 触发上游同步（失败不致命，仍尝试静态读）
        Map<String, String> fetchHeaders = new LinkedHashMap<>();
        fetchHeaders.put("Accept", "application/json");
        try {
            HttpClient.post(BASE_URL + "/api/temp-mail/fetch-emails/" + tok,
                    null, null, fetchHeaders);
        } catch (RuntimeException ignored) {
            // 同步失败不阻断后续读取
        }

        // 2) 读静态收件箱
        Map<String, String> inboxHeaders = new LinkedHashMap<>();
        inboxHeaders.put("Accept", "application/json");
        HttpResult resp = HttpClient.get(BASE_URL + "/api/temp-mail/inbox/" + tok, inboxHeaders);
        resp.ensureSuccess();

        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            return new ArrayList<>();
        }
        JsonElement dataEl = data.get("data");
        List<Email> out = new ArrayList<>();
        if (dataEl == null || !dataEl.isJsonObject()) {
            return out;
        }
        JsonElement messagesEl = dataEl.getAsJsonObject().get("messages");
        if (messagesEl == null || !messagesEl.isJsonArray()) {
            return out;
        }
        JsonArray messages = messagesEl.getAsJsonArray();
        for (JsonElement item : messages) {
            if (!item.isJsonObject()) {
                continue;
            }
            JsonObject msg = item.getAsJsonObject();
            Map<String, Object> flat = new LinkedHashMap<>();
            flat.put("from", Json.str(msg, "from_email"));
            flat.put("to", email);
            flat.put("text", Json.str(msg, "text_body"));
            flat.put("html", Json.str(msg, "html_body"));
            flat.put("date", Json.str(msg, "received_at"));
            if (msg.has("attachments")) {
                flat.put("attachments", Json.toRaw(msg.get("attachments")));
            }
            out.add(Normalizer.normalizeEmail(flat, email));
        }
        return out;
    }

    /**
     * 将 ISO 8601 时间串转为毫秒时间戳。
     *
     * @param iso ISO 时间串
     * @return 毫秒时间戳，解析失败返回 null
     */
    private static Long parseIsoMillis(String iso) {
        try {
            return java.time.OffsetDateTime.parse(iso.replace("Z", "+00:00"))
                    .toInstant().toEpochMilli();
        } catch (RuntimeException ignored) {
            return null;
        }
    }
}