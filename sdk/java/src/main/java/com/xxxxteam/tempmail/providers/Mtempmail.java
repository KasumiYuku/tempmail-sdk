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
import java.util.regex.Pattern;

/**
 * MTempMail 渠道 — https://mtempmail.com（公共 key 认证）
 *
 * <p>建箱: POST /api/emails/{apiKey}（body {}）→
 * {"status":true,"data":{"email":"xxx@domain","domain":"..","ip":"..",
 * "fingerprint":"..","expire_at":"..","created_at":"..","id":213000,"email_token":"..."}}
 * 读信: GET /api/messages/{apiKey}/{email} →
 * {"status":true,"mailbox":"..","email_token":"..","messages":[]}
 * 消息列表元素为 mailgun 入站 webhook 风格：
 * {"to":[{..}],"body":[{content_type:"text/html",value:".."}],"created_at":"..",
 * "id":123,"from":[{"full":"Sender &lt;a@b.com&gt;"}],"subject":"..","flags":[..]}。
 * 邮箱 24 小时有效（过期时间为北京时间，以服务端为准）。</p>
 */
public final class Mtempmail {

    private static final String BASE_URL = "https://mtempmail.com";
    private static final String CHANNEL = "mtempmail";

    /** mtempmail.com 官方提供的公共固定 API key。 */
    private static final String PUBLIC_KEY = "pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw";

    /** 后台拼接的 "• " 前缀（或实收分隔符），预编译避免重复编译。 */
    private static final Pattern BULLET_RE = Pattern.compile("^[•·]+\\s*");

    private Mtempmail() {
    }

    /**
     * 清洗主题前导分隔符（后台可能以 "• " 开头拼接微件）。
     *
     * @param subject 原始主题
     * @return 清洗后的主题
     */
    private static String cleanSubject(String subject) {
        return BULLET_RE.matcher(subject.trim()).replaceFirst("").trim();
    }

    /**
     * 拼接正文纯文本（body[].value 按序，每段后补换行）。
     *
     * @param parts body 段落
     * @return 纯文本正文
     */
    private static String bodyText(List<Map<String, Object>> parts) {
        StringBuilder sb = new StringBuilder();
        for (Map<String, Object> p : parts) {
            Object v = p.get("value");
            if (v instanceof String) {
                sb.append((String) v).append("\n");
            }
        }
        return sb.toString().stripTrailing();
    }

    /**
     * 提取首个 text/html 段。
     *
     * @param parts body 段落
     * @return HTML 正文，未命中返回空串
     */
    private static String bodyHtml(List<Map<String, Object>> parts) {
        for (Map<String, Object> p : parts) {
            Object ct = p.get("content_type");
            if (ct instanceof String && "text/html".equals(ct)) {
                Object v = p.get("value");
                if (v instanceof String) {
                    return (String) v;
                }
            }
        }
        return "";
    }

    /**
     * 把 JSON 数组逐元素转为字典列表。
     *
     * @param arr JSON 数组
     * @return 字典列表（非字典元素跳过）
     */
    @SuppressWarnings("unchecked")
    private static List<Map<String, Object>> toMapList(JsonArray arr) {
        List<Map<String, Object>> out = new ArrayList<>();
        for (JsonElement el : arr) {
            Object raw = Json.toRaw(el);
            if (raw instanceof Map) {
                out.add((Map<String, Object>) raw);
            }
        }
        return out;
    }

    /**
     * 创建 mtempmail 临时邮箱（POST /api/emails/{apiKey}，空 JSON body）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.post(BASE_URL + "/api/emails/" + PUBLIC_KEY,
                "{}", "application/json", headers);
        resp.ensureSuccess();

        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null || !"true".equals(Json.str(data, "status"))) {
            throw new RuntimeException("mtempmail create: 响应 status 缺失或为 false");
        }
        JsonElement dataEl = data.get("data");
        if (dataEl == null || !dataEl.isJsonObject()) {
            throw new RuntimeException("mtempmail create: 响应缺少 data 字段");
        }
        JsonObject inner = dataEl.getAsJsonObject();
        String email = Json.str(inner, "email").trim();
        if (email.isEmpty()) {
            throw new RuntimeException("mtempmail create: 响应缺少邮箱");
        }
        return new EmailInfo(CHANNEL, email, Json.str(inner, "email_token").trim(),
                null, Json.str(inner, "created_at").trim());
    }

    /**
     * 读取 mtempmail 收件箱（GET /api/messages/{apiKey}/{email}）。
     *
     * @param token 建箱返回的 email_token（仅校验，不参与请求）
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("mtempmail: 邮箱为空");
        }
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("mtempmail: token 为空");
        }
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.get(BASE_URL + "/api/messages/" + PUBLIC_KEY + "/"
                + ProviderUtil.urlEncode(addr), headers);
        resp.ensureSuccess();

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
            flat.put("to", addr);
            Object subjObj = flat.get("subject");
            if (subjObj instanceof String) {
                flat.put("subject", cleanSubject((String) subjObj));
            }
            Object bodyObj = flat.get("body");
            if (bodyObj instanceof List) {
                List<Map<String, Object>> parts = new ArrayList<>();
                for (Object seg : (List<Object>) bodyObj) {
                    if (seg instanceof Map) {
                        @SuppressWarnings("unchecked")
                        Map<String, Object> pm = (Map<String, Object>) seg;
                        parts.add(pm);
                    }
                }
                String text = bodyText(parts);
                if (!text.isEmpty()) {
                    flat.put("text", text);
                }
                String html = bodyHtml(parts);
                if (!html.isEmpty()) {
                    flat.put("html", html);
                }
            }
            flat.put("date", flat.get("created_at"));
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }
}