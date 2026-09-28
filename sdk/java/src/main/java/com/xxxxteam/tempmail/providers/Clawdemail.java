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
 * ClawdEmail 渠道 — https://api.clawdemail.com
 *
 * <p>POST /register 建箱（body {"name":""}，响应 success/email/token），
 * GET /inbox?limit=50 读信（Header Authorization: Bearer &lt;token&gt;，
 * 响应 success/email/count/unread/emails[]，success 非真时携带 error 报错），
 * 每封 GET /email/{id} 取详情（响应含 email 嵌套对象时提升嵌套对象）。</p>
 */
public final class Clawdemail {

    private static final String BASE_URL = "https://api.clawdemail.com";
    private static final String CHANNEL = "clawdemail";

    private Clawdemail() {
    }

    /**
     * 构造 clawdemail 请求的通用请求头（含 Bearer 认证）。
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
     * 创建 clawdemail 临时邮箱（POST /register，空名称 JSON body）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        HttpResult resp = HttpClient.post(BASE_URL + "/register",
                "{\"name\":\"\"}", "application/json", authHeaders(""));
        if (!resp.isOk()) {
            throw new RuntimeException("clawdemail: 创建邮箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("clawdemail: 解析创建响应失败");
        }
        String email = Json.str(data, "email").trim();
        String token = Json.str(data, "token").trim();
        if (email.isEmpty() || token.isEmpty() || !email.contains("@")) {
            throw new RuntimeException("clawdemail: 创建邮箱响应缺少必要字段: "
                    + resp.getBody().trim());
        }
        return new EmailInfo(CHANNEL, email, token, null, null);
    }

    /**
     * 获取邮件列表：GET /inbox?limit=50 取列表，对每个元素按 id 逐封
     * GET /email/{id} 合并详情；详情失败时回退为列表摘要归一。
     *
     * @param token 注册返回的认证令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("clawdemail: token 为空");
        }
        HttpResult resp = HttpClient.get(BASE_URL + "/inbox?limit=50", authHeaders(tok));
        if (!resp.isOk()) {
            throw new RuntimeException("clawdemail: 获取邮件列表失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("clawdemail: 解析邮件列表响应失败");
        }
        if (!"true".equals(Json.str(data, "success"))) {
            throw new RuntimeException("clawdemail: 读取收件箱失败: " + Json.str(data, "error"));
        }
        JsonElement emailsEl = data.get("emails");
        if (emailsEl == null || !emailsEl.isJsonArray()) {
            return new ArrayList<>();
        }
        List<Email> out = new ArrayList<>();
        for (JsonElement item : emailsEl.getAsJsonArray()) {
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
     * 获取单封邮件详情（GET /email/{id}），响应含 email 嵌套对象
     * （from_addr/subject/body_text/received_at）时提升嵌套对象，失败返回 null。
     *
     * @param token     认证令牌
     * @param messageId 邮件 ID
     * @return 详情 JSON 对象，失败返回 null
     */
    private static JsonObject fetchDetail(String token, String messageId) {
        try {
            HttpResult resp = HttpClient.get(BASE_URL + "/email/"
                    + ProviderUtil.urlEncode(lastSegment(messageId)), authHeaders(token));
            if (!resp.isOk()) {
                return null;
            }
            JsonObject detail = Json.parseObject(resp.getBody());
            if (detail == null) {
                return null;
            }
            JsonElement nested = detail.get("email");
            if (nested != null && nested.isJsonObject()) {
                return nested.getAsJsonObject();
            }
            return detail;
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