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
 * NowTempMail 渠道 — https://nowtempmail.com
 *
 * <p>POST /mailbox 建箱（空 body，响应 token（JWT）/mailbox），
 * GET /messages 读信（Header Authorization: Bearer &lt;token&gt;，
 * 响应 {"messages":[...]}），每封 GET /message/{id} 取单封详情（Bearer）。
 * 列表元素按多候选字段归一；详情仅补缺失字段，失败回退列表摘要。</p>
 */
public final class Nowtempmail {

    private static final String BASE_URL = "https://nowtempmail.com";
    private static final String CHANNEL = "nowtempmail";

    private Nowtempmail() {
    }

    /**
     * 构造 nowtempmail 请求的通用请求头（含 Bearer 认证）。
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
     * 创建 nowtempmail.com 临时邮箱（POST /mailbox，空 body）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Content-Type", "application/json");
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.post(BASE_URL + "/mailbox",
                null, "application/json", headers);
        if (!resp.isOk()) {
            throw new RuntimeException("nowtempmail: 创建邮箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("nowtempmail: 解析创建响应失败");
        }
        String token = Json.str(data, "token").trim();
        String mailbox = Json.str(data, "mailbox").trim();
        if (token.isEmpty() || mailbox.isEmpty() || !mailbox.contains("@")) {
            throw new RuntimeException("nowtempmail: 创建邮箱响应缺少必要字段: "
                    + resp.getBody().trim());
        }
        return new EmailInfo(CHANNEL, mailbox, token, null, null);
    }

    /**
     * 获取邮件列表：GET /messages 取列表（{"messages":[...]}），对每个元素按 id
     * 逐封 GET /message/{id} 合并详情（缺字段才覆盖）；详情失败时回退为列表摘要。
     *
     * @param token 建箱返回的 JWT 认证令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("nowtempmail: token 为空");
        }
        HttpResult resp = HttpClient.get(BASE_URL + "/messages", authHeaders(tok));
        if (!resp.isOk()) {
            throw new RuntimeException("nowtempmail: 获取邮件列表失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("nowtempmail: 解析邮件列表失败");
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
     * 获取单封邮件详情（GET /message/{id}，Bearer token），失败返回 null。
     *
     * @param token     认证令牌
     * @param messageId 邮件 ID
     * @return 详情 JSON 对象，失败返回 null
     */
    private static JsonObject fetchDetail(String token, String messageId) {
        try {
            HttpResult resp = HttpClient.get(BASE_URL + "/message/"
                    + lastSegment(messageId), authHeaders(token));
            if (!resp.isOk()) {
                return null;
            }
            return Json.parseObject(resp.getBody());
        } catch (RuntimeException ignored) {
            return null;
        }
    }

    /**
     * 取路径或 ID 的末段（与 Go path.Base 等价），防止异常输入注入路径。
     *
     * @param id 原始 ID
     * @return 末段字符串
     */
    private static String lastSegment(String id) {
        int idx = Math.max(id.lastIndexOf('/'), id.lastIndexOf('\\'));
        return idx >= 0 ? id.substring(idx + 1) : id;
    }

    /**
     * 从列表元素中提取邮件 ID。
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