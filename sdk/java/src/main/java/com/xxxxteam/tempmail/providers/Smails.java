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
 * Smails.dev 渠道 — https://smails.dev
 *
 * <p>POST /api/mailbox 建箱（空 body，响应 address/token），
 * GET /api/mailbox/messages 读信（Header Authorization: Bearer &lt;token&gt;，数组响应），
 * GET /api/mailbox/messages/{id} 取单封详情（Bearer）。</p>
 */
public final class Smails {

    private static final String BASE_URL = "https://smails.dev";
    private static final String CHANNEL = "smails";

    private Smails() {
    }

    /**
     * 构造 smails.dev 请求的通用请求头（含 Bearer 认证）。
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
     * 创建 smails.dev 临时邮箱（POST /api/mailbox，空 JSON body）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        HttpResult resp = HttpClient.post(BASE_URL + "/api/mailbox",
                "{}", "application/json", authHeaders(""));
        if (!resp.isOk()) {
            throw new RuntimeException("smails: 创建邮箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("smails: 解析创建响应失败");
        }
        String address = Json.str(data, "address").trim();
        String token = Json.str(data, "token").trim();
        if (address.isEmpty() || token.isEmpty()) {
            throw new RuntimeException("smails: 创建邮箱响应缺少 address 或 token");
        }
        return new EmailInfo(CHANNEL, address, token, null, null);
    }

    /**
     * 获取邮件列表：GET /api/mailbox/messages 取列表，对每个元素按 id 逐封
     * GET /api/mailbox/messages/{id} 合并详情；详情失败时回退为列表摘要归一。
     *
     * @param token 建箱返回的认证令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("smails: token 为空");
        }
        HttpResult resp = HttpClient.get(BASE_URL + "/api/mailbox/messages", authHeaders(tok));
        if (!resp.isOk()) {
            throw new RuntimeException("smails: 获取邮件列表失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonElement parsed = Json.parse(resp.getBody());
        if (parsed == null || !parsed.isJsonArray()) {
            return new ArrayList<>();
        }
        List<Email> out = new ArrayList<>();
        for (JsonElement item : parsed.getAsJsonArray()) {
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
     * 获取单封邮件详情（GET /api/mailbox/messages/{id}），失败返回 null。
     *
     * @param token     认证令牌
     * @param messageId 邮件 ID
     * @return 详情 JSON 对象，失败返回 null
     */
    private static JsonObject fetchDetail(String token, String messageId) {
        try {
            HttpResult resp = HttpClient.get(BASE_URL + "/api/mailbox/messages/"
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