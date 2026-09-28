package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.google.gson.JsonPrimitive;
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
 * TempMail100 渠道 — https://tempmail100.com
 *
 * <p>两步建箱：POST /init（空 body）取得 JWT token（响应 code/data.token），
 * 再 POST /web/generate（Authorization: 裸 token 无 Bearer）建随机地址
 * （响应 code/data.address）。读信 GET /web/emails（Authorization 裸 token，
 * 响应 code/data.list[]/data.total），code!=0 报错，list 为 null 返回空列表。</p>
 *
 * <p>列表元素 {uuid, subject, fromAddress, toAddress, fromName, content, timestamp,
 * read} 逐项归一：id=uuid；fromName 非空且 !=fromAddress 且 fromAddress 含 @ 时
 * 合成 "Name &lt;address&gt;"；timestamp 毫秒；read 兼容 bool/数字/字符串
 * "true"|"1"。平台正文端点不存在（content 恒空），如实输出空正文。</p>
 */
public final class Tempmail100 {

    private static final String BASE_URL = "https://tempmail100.com";
    private static final String CHANNEL = "tempmail100";

    private Tempmail100() {
    }

    /**
     * 构造 tempmail100 请求的通用请求头
     * （前端使用 Authorization: &lt;token&gt; 不带 Bearer）。
     *
     * @param token 认证令牌，可为空
     * @return 请求头
     */
    private static Map<String, String> authHeaders(String token) {
        Map<String, String> h = new LinkedHashMap<>();
        h.put("Accept", "application/json");
        if (token != null && !token.isEmpty()) {
            h.put("Authorization", token);
        }
        return h;
    }

    /**
     * 创建临时邮箱：POST /init 取得 JWT token，再 POST /web/generate 创建地址。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        // 第一步：初始化取得 token
        HttpResult initResp = HttpClient.post(BASE_URL + "/init",
                null, "application/json", authHeaders(""));
        if (!initResp.isOk()) {
            throw new RuntimeException("tempmail100: 初始化失败 http " + initResp.getStatusCode()
                    + ": " + initResp.getBody().trim());
        }
        JsonObject init = Json.parseObject(initResp.getBody());
        if (init == null) {
            throw new RuntimeException("tempmail100: 解析初始化响应失败");
        }
        JsonElement initDataEl = init.get("data");
        String token = initDataEl != null && initDataEl.isJsonObject()
                ? Json.str(initDataEl.getAsJsonObject(), "token").trim() : "";
        if (token.isEmpty() || !"0".equals(Json.str(init, "code"))) {
            throw new RuntimeException("tempmail100: 初始化响应异常: " + initResp.getBody().trim());
        }

        // 第二步：创建随机地址
        HttpResult genResp = HttpClient.post(BASE_URL + "/web/generate",
                null, "application/json", authHeaders(token));
        if (!genResp.isOk()) {
            throw new RuntimeException("tempmail100: 创建地址失败 http " + genResp.getStatusCode()
                    + ": " + genResp.getBody().trim());
        }
        JsonObject gen = Json.parseObject(genResp.getBody());
        JsonElement genDataEl = gen != null ? gen.get("data") : null;
        String address = genDataEl != null && genDataEl.isJsonObject()
                ? Json.str(genDataEl.getAsJsonObject(), "address").trim() : "";
        if (gen == null || !"0".equals(Json.str(gen, "code")) || address.isEmpty()
                || !address.contains("@")) {
            throw new RuntimeException("tempmail100: 创建地址响应异常: " + genResp.getBody().trim());
        }
        return new EmailInfo(CHANNEL, address, token, null, null);
    }

    /**
     * 获取邮件列表（GET /web/emails，Authorization 裸 token）。
     * code!=0 报错；list 为 null 返回空列表；content 平台恒空，如实留空。
     *
     * @param token 初始化返回的 JWT 认证令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        HttpResult resp = HttpClient.get(BASE_URL + "/web/emails", authHeaders(token));
        if (!resp.isOk()) {
            throw new RuntimeException("tempmail100: 获取邮件列表失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("tempmail100: 解析邮件列表失败");
        }
        if (!"0".equals(Json.str(data, "code"))) {
            throw new RuntimeException("tempmail100: 获取邮件列表响应异常: "
                    + Json.str(data, "message"));
        }
        JsonElement dataEl = data.get("data");
        JsonElement listEl = dataEl != null && dataEl.isJsonObject()
                ? dataEl.getAsJsonObject().get("list") : null;
        if (listEl == null || listEl.isJsonNull()) {
            return new ArrayList<>();
        }
        if (!listEl.isJsonArray()) {
            return new ArrayList<>();
        }
        List<Email> out = new ArrayList<>();
        for (JsonElement item : listEl.getAsJsonArray()) {
            if (!item.isJsonObject()) {
                continue;
            }
            out.add(normalizeItem(item.getAsJsonObject(), email));
        }
        return out;
    }

    /**
     * 将 /web/emails 列表元素归一为统一邮件结构。
     * content 平台恒为空如实留空；fromName+fromAddress 组合为
     * "Name &lt;address&gt;" 填入 from；timestamp 为毫秒值；
     * read 兼容 bool/数字/字符串。
     *
     * @param item  列表元素
     * @param email 邮箱地址
     * @return 标准化邮件
     */
    private static Email normalizeItem(JsonObject item, String email) {
        String fromName = safeStr(item, "fromName");
        String fromAddress = safeStr(item, "fromAddress");
        if (!fromName.isEmpty() && !fromName.equalsIgnoreCase(fromAddress)
                && fromAddress.contains("@")) {
            fromAddress = fromName + " <" + fromAddress + ">";
        }
        Map<String, Object> flat = new LinkedHashMap<>();
        flat.put("id", safeStr(item, "uuid"));
        flat.put("from", fromAddress);
        flat.put("to", safeStr(item, "toAddress"));
        flat.put("subject", safeStr(item, "subject"));
        flat.put("content", safeStr(item, "content"));
        if (item.has("timestamp") && !item.get("timestamp").isJsonNull()) {
            flat.put("timestamp", Json.toRaw(item.get("timestamp")));
        }
        flat.put("isRead", readBool(item, "read"));
        return Normalizer.normalizeEmail(flat, email);
    }

    /**
     * 将接口字段值安全转换为字符串（字符串原样、数值转十进制、其余空串）。
     *
     * @param obj JSON 对象
     * @param key 字段名
     * @return 字符串值，缺失返回空串
     */
    private static String safeStr(JsonObject obj, String key) {
        if (obj == null || !obj.has(key) || obj.get(key).isJsonNull()) {
            return "";
        }
        JsonPrimitive p = obj.get(key).isJsonPrimitive() ? obj.get(key).getAsJsonPrimitive() : null;
        if (p == null) {
            return "";
        }
        if (p.isString()) {
            return p.getAsString();
        }
        if (p.isNumber()) {
            double d = p.getAsDouble();
            if (d == Math.floor(d) && !Double.isInfinite(d)) {
                return Long.toString((long) d);
            }
            return Double.toString(d);
        }
        return "";
    }

    /**
     * 将 read 字段归一为布尔已读标记，兼容 bool / 0|1 数字 / "true"|"1" 字符串。
     *
     * @param obj JSON 对象
     * @param key 字段名
     * @return 布尔已读值，未命中返回 false
     */
    private static boolean readBool(JsonObject obj, String key) {
        if (obj == null || !obj.has(key) || obj.get(key).isJsonNull()) {
            return false;
        }
        JsonElement v = obj.get(key);
        if (!v.isJsonPrimitive()) {
            return false;
        }
        JsonPrimitive p = v.getAsJsonPrimitive();
        if (p.isBoolean()) {
            return p.getAsBoolean();
        }
        if (p.isNumber()) {
            return p.getAsLong() != 0;
        }
        if (p.isString()) {
            String s = p.getAsString().trim();
            return "true".equalsIgnoreCase(s) || "1".equals(s);
        }
        return false;
    }
}