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
 * TempMail Portal 渠道 — https://api.tempmailportal.com
 *
 * <p>POST /api/v2/inbox 建箱（body {}，响应 address/token/private/expiresAt/retentionMs，
 * token 为 p2 前缀），GET /api/messages 读信（Header Authorization: Bearer &lt;token&gt;），
 * GET /api/messages/{id} 取单封详情（Bearer），GET /api/domains 返回收信域名池（Bearer）。</p>
 */
public final class Tempmailportal {

    private static final String BASE_URL = "https://api.tempmailportal.com";
    private static final String CHANNEL = "tempmailportal";

    private Tempmailportal() {
    }

    /**
     * 构造 tempmailportal 请求的通用请求头（含 Bearer 认证）。
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
     * 创建 tempmailportal 临时邮箱（POST /api/v2/inbox，空 JSON body）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        HttpResult resp = HttpClient.post(BASE_URL + "/api/v2/inbox",
                "{}", "application/json", authHeaders(""));
        if (!resp.isOk()) {
            throw new RuntimeException("tempmailportal: 创建邮箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("tempmailportal: 解析创建响应失败");
        }
        String address = Json.str(data, "address").trim();
        String token = Json.str(data, "token").trim();
        if (address.isEmpty() || token.isEmpty()) {
            throw new RuntimeException("tempmailportal: 创建邮箱响应缺少 address 或 token");
        }
        String expiresAt = Json.str(data, "expiresAt").trim();
        return new EmailInfo(CHANNEL, address, token,
                expiresAt.isEmpty() ? null : parseIsoMillis(expiresAt), null);
    }

    /**
     * 获取邮件列表：GET /api/messages 取列表，对每个元素按 id 逐封
     * GET /api/messages/{id} 合并详情；详情失败时回退为列表摘要归一。
     *
     * @param token 建箱返回的 p2 前缀认证令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("tempmailportal: token 为空");
        }
        HttpResult resp = HttpClient.get(BASE_URL + "/api/messages", authHeaders(tok));
        if (!resp.isOk()) {
            throw new RuntimeException("tempmailportal: 获取邮件列表失败 http " + resp.getStatusCode()
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
     * 获取单封邮件详情（GET /api/messages/{id}），失败返回 null。
     *
     * @param token     认证令牌
     * @param messageId 邮件 ID
     * @return 详情 JSON 对象，失败返回 null
     */
    private static JsonObject fetchDetail(String token, String messageId) {
        try {
            HttpResult resp = HttpClient.get(BASE_URL + "/api/messages/"
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