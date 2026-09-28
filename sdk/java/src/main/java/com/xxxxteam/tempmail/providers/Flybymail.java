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
 * FlyByMail 渠道 — https://flybymail.com
 *
 * <p>POST /api/recipients 建箱（空 JSON body，响应
 * id/email/createdAt/expiresAt 毫秒时间戳，email 形如 temp_xxx@flybymail.com，约 4 小时）；
 * GET /api/recipients/&lt;URL 编码完整地址&gt;/emails 读信（按邮箱地址、非 id 查询，
 * 响应 {"emails":[...]}）。</p>
 *
 * <p>信件字段 id/from/to/subject/body（纯文本）/htmlBody（HTML 正文）/preview/time/read/
 * attachments：text=body、html=htmlBody、id 数字→字符串、timestamp=time。</p>
 */
public final class Flybymail {

    private static final String BASE_URL = "https://flybymail.com";
    private static final String CHANNEL = "flybymail";

    private Flybymail() {
    }

    /**
     * 构造 flybymail 请求的通用 JSON 请求头。
     *
     * @return 请求头
     */
    private static Map<String, String> jsonHeaders() {
        Map<String, String> h = new LinkedHashMap<>();
        h.put("Content-Type", "application/json");
        h.put("Accept", "application/json");
        return h;
    }

    /**
     * 创建 flybymail.com 临时邮箱（POST /api/recipients，空 JSON body）。
     * expiresAt 为毫秒时间戳，转换为秒供 EmailInfo 统一展示。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        HttpResult resp = HttpClient.post(BASE_URL + "/api/recipients",
                "{}", "application/json", jsonHeaders());
        if (!resp.isOk()) {
            throw new RuntimeException("flybymail: 创建邮箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("flybymail: 解析创建响应失败");
        }
        String id = Json.str(data, "id").trim();
        String email = Json.str(data, "email").trim();
        if (id.isEmpty() || email.isEmpty() || !email.contains("@")) {
            throw new RuntimeException("flybymail: 创建邮箱响应缺少必要字段: "
                    + resp.getBody().trim());
        }
        long expiresAt = 0;
        try {
            expiresAt = Long.parseLong(Json.str(data, "expiresAt").trim());
        } catch (NumberFormatException ignored) {
            // expiresAt 缺失/非法时保留 0
        }
        Long expiresSec = expiresAt > 0 ? expiresAt / 1000 : null;
        return new EmailInfo(CHANNEL, email, id, expiresSec, null);
    }

    /**
     * 获取邮件列表（GET /api/recipients/&lt;完整地址 URL 编码&gt;/emails）。
     * id 为数字时转十进制字符串，time 为毫秒时间戳时归一进 timestamp。
     *
     * @param token 建箱返回的收件人 ID（读信按邮箱地址查询，令牌作为保留参数）
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty() || !addr.contains("@")) {
            throw new RuntimeException("flybymail: 邮箱地址为空或格式错误");
        }
        HttpResult resp = HttpClient.get(BASE_URL + "/api/recipients/"
                + ProviderUtil.urlEncode(addr) + "/emails", jsonHeaders());
        if (!resp.isOk()) {
            throw new RuntimeException("flybymail: 获取邮件列表失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("flybymail: 解析邮件列表失败");
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
            JsonObject raw = item.getAsJsonObject();
            Map<String, Object> flat = new LinkedHashMap<>();
            flat.put("from", toRaw(raw, "from"));
            flat.put("to", toRaw(raw, "to"));
            flat.put("subject", toRaw(raw, "subject"));
            flat.put("text", toRaw(raw, "body"));
            flat.put("html", toRaw(raw, "htmlBody"));
            flat.put("time", toRaw(raw, "time"));
            flat.put("read", toRaw(raw, "read"));
            flat.put("attachments", toRaw(raw, "attachments"));
            // id 为数字时转十进制字符串
            flat.put("id", anyString(raw, "id"));
            flat.put("timestamp", timestampOf(raw));
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }

    /**
     * 从 JSON 对象提取候选键的首个非空字符串（数字转十进制字符串）。
     *
     * @param obj  JSON 对象
     * @param keys 候选键
     * @return 命中字符串，未命中返回空串
     */
    private static String anyString(JsonObject obj, String... keys) {
        for (String key : keys) {
            if (!obj.has(key) || obj.get(key).isJsonNull()) {
                continue;
            }
            String v = Json.nodeToString(obj.get(key)).trim();
            if (!v.isEmpty()) {
                return v;
            }
        }
        return "";
    }

    /**
     * 归一邮件时间，候选 time/date 字段（time 为毫秒时间戳）。
     *
     * @param obj JSON 对象
     * @return 时间原生值，均缺失返回 null
     */
    private static Object timestampOf(JsonObject obj) {
        for (String key : new String[]{"time", "date"}) {
            if (obj.has(key) && !obj.get(key).isJsonNull()) {
                return Json.toRaw(obj.get(key));
            }
        }
        return null;
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