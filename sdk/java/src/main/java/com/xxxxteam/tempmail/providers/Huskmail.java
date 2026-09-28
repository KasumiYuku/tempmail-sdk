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

/**
 * HuskMail 渠道 — https://huskmail.xyz（API 域 api.huskmail.space）
 *
 * <p>POST /api/v1/accounts 建箱（body {}，响应 id/address/password/token/expiresAt/tier，
 * token 为 JWT），GET /v1/messages 读信（Header Authorization: Bearer &lt;token&gt;，
 * 响应 {"messages":[...]}），GET /v1/messages/{id} 取单封详情（Bearer）。
 * 收信域固定为 @huskmail.xyz（huskmail.space 无 MX）。</p>
 */
public final class Huskmail {

    private static final String BASE_URL = "https://api.huskmail.space";
    private static final String CHANNEL = "huskmail";

    private Huskmail() {
    }

    /**
     * 构造 huskmail（huskmail.space API）请求的通用请求头（含 Bearer 认证）。
     *
     * @param token 认证令牌，可为空
     * @return 请求头
     */
    private static Map<String, String> authHeaders(String token) {
        Map<String, String> h = new LinkedHashMap<>();
        h.put("Accept", "application/json");
        if (token != null && !token.isEmpty()) {
            h.put("Authorization", "Bearer " + token);
        }
        return h;
    }

    /**
     * 创建 huskmail（@huskmail.xyz）临时邮箱（POST /api/v1/accounts，空 JSON body）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        HttpResult resp = HttpClient.post(BASE_URL + "/api/v1/accounts",
                "{}", "application/json", authHeaders(""));
        if (!resp.isOk()) {
            throw new RuntimeException("huskmail: 创建邮箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("huskmail: 解析创建响应失败");
        }
        String address = Json.str(data, "address").trim();
        String token = Json.str(data, "token").trim();
        if (address.isEmpty() || token.isEmpty()) {
            throw new RuntimeException("huskmail: 创建邮箱响应缺少 address 或 token");
        }
        String expiresAt = Json.str(data, "expiresAt").trim();
        // expiresAt 为 Unix 秒时间戳，转毫秒
        Long expiresMs = null;
        if (!expiresAt.isEmpty()) {
            try {
                expiresMs = Long.parseLong(expiresAt) * 1000;
            } catch (NumberFormatException ignored) {
                // 保留 null
            }
        }
        return new EmailInfo(CHANNEL, address, token, expiresMs, null);
    }

    /**
     * 获取邮件列表：GET /v1/messages 取列表（{"messages":[...]}），对每个元素按 id
     * 逐封 GET /v1/messages/{id} 合并详情；详情失败时回退为列表摘要归一。
     *
     * @param token 建箱返回的 JWT 认证令牌
     * @param email 邮箱地址（@huskmail.xyz）
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("huskmail: token 为空");
        }
        HttpResult resp = HttpClient.get(BASE_URL + "/v1/messages", authHeaders(tok));
        if (!resp.isOk()) {
            throw new RuntimeException("huskmail: 获取邮件列表失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            return new ArrayList<>();
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
            Map<String, Object> flat = Json.toDict(item);
            String id = messageIdOf(flat);
            if (id.isEmpty()) {
                out.add(Normalizer.normalizeEmail(flat, email));
                continue;
            }
            // 详情合并失败时回退为列表摘要
            JsonObject detail = fetchDetail(tok, id);
            if (detail != null) {
                for (Map.Entry<String, Object> kv : Json.toDict(detail).entrySet()) {
                    if (!flat.containsKey(kv.getKey())) {
                        flat.put(kv.getKey(), kv.getValue());
                    }
                }
            }
            out.add(Normalizer.normalizeEmail(flat, email));
        }
        return out;
    }

    /**
     * 获取单封邮件详情（GET /v1/messages/{id}），失败返回 null。
     *
     * @param token     认证令牌
     * @param messageId 邮件 ID
     * @return 详情 JSON 对象，失败返回 null
     */
    private static JsonObject fetchDetail(String token, String messageId) {
        try {
            HttpResult resp = HttpClient.get(BASE_URL + "/v1/messages/"
                    + ProviderUtil.urlEncode(messageId), authHeaders(token));
            if (!resp.isOk()) {
                return null;
            }
            return Json.parseObject(resp.getBody());
        } catch (RuntimeException ignored) {
            return null;
        }
    }

    /**
     * 从列表元素中提取邮件 ID，候选字段 id/Id/slug/messageId/message_id。
     *
     * @param flat 列表元素字典
     * @return 邮件 ID，未命中返回空串
     */
    private static String messageIdOf(Map<String, Object> flat) {
        for (String key : new String[]{"id", "Id", "slug", "messageId", "message_id"}) {
            Object v = flat.get(key);
            if (v instanceof String && !((String) v).trim().isEmpty()) {
                return ((String) v).trim();
            }
        }
        return "";
    }
}