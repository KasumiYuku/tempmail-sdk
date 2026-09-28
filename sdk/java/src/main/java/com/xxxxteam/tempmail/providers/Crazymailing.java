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
 * CrazyMailing 渠道 — https://crazymailing.com
 *
 * <p>Next.js 全栈站点：建箱 POST /api/mailbox（空 JSON body），响应
 * {"mailbox":{"id","address","expiresAt"}}，token=id；
 * 读信 GET /api/messages?mailbox=&lt;URL 编码完整地址&gt;，响应 {"messages":[...]}；
 * 每封 GET /api/message/{id}/body 拉正文（响应为完整 HTML 页面），
 * 失败时以列表摘要归一。</p>
 */
public final class Crazymailing {

    private static final String BASE_URL = "https://crazymailing.com";
    private static final String CHANNEL = "crazymailing";

    private Crazymailing() {
    }

    /**
     * 构造 crazymailing API 请求通用头（含 Origin/Referer 站点特征）。
     *
     * @return 请求头
     */
    private static Map<String, String> baseHeaders() {
        Map<String, String> h = new LinkedHashMap<>();
        h.put("Accept", "application/json");
        h.put("Origin", BASE_URL);
        h.put("Referer", BASE_URL + "/");
        return h;
    }

    /**
     * 创建临时邮箱（POST /api/mailbox，空 JSON body），域名由服务端统一分配。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        HttpResult resp = HttpClient.post(BASE_URL + "/api/mailbox",
                "{}", "application/json", baseHeaders());
        if (!resp.isOk()) {
            throw new RuntimeException("crazymailing: 创建邮箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        JsonElement mailboxEl = data != null ? data.get("mailbox") : null;
        if (mailboxEl == null || !mailboxEl.isJsonObject()) {
            throw new RuntimeException("crazymailing: 创建响应缺少 mailbox.address: "
                    + resp.getBody().trim());
        }
        JsonObject mailbox = mailboxEl.getAsJsonObject();
        String address = Json.str(mailbox, "address").trim();
        if (address.isEmpty()) {
            throw new RuntimeException("crazymailing: 创建响应缺少 mailbox.address: "
                    + resp.getBody().trim());
        }
        String id = Json.str(mailbox, "id").trim();
        String expiresAt = Json.str(mailbox, "expiresAt").trim();
        Long expiresMs = parseIsoMillis(expiresAt);
        return new EmailInfo(CHANNEL, address, id, expiresMs, null);
    }

    /**
     * 读取收件箱（GET /api/messages?mailbox=&lt;完整地址 URL 编码&gt;）。
     * 列表元素为摘要，正文逐封 GET /api/message/{id}/body 二拉，
     * 失败时以列表摘要归一，不阻断列表。
     *
     * @param token 建箱返回的 mailbox id
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("crazymailing: 邮箱地址为空");
        }
        HttpResult resp = HttpClient.get(BASE_URL + "/api/messages?mailbox="
                + ProviderUtil.urlEncode(addr), baseHeaders());
        if (!resp.isOk()) {
            throw new RuntimeException("crazymailing: 读取收件箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("crazymailing: 解析收件箱响应失败");
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
            JsonObject msg = item.getAsJsonObject();
            Map<String, Object> flat = Json.toDict(msg);
            // 注入收件人地址（to 字段缺失时保底）
            flat.put("to", addr);
            // 列表元素为摘要，正文须逐封二拉
            String id = Json.str(msg, "id");
            if (!id.isEmpty()) {
                String html = fetchBody(id);
                if (!html.trim().isEmpty()) {
                    flat.put("html", html);
                }
            }
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }

    /**
     * 拉取单封正文（GET /api/message/{id}/body，响应为完整 HTML 页面），
     * 失败返回空串（列表摘要兜底）。
     *
     * @param id 邮件 ID
     * @return HTML 正文，失败返回空串
     */
    private static String fetchBody(String id) {
        try {
            Map<String, String> headers = new LinkedHashMap<>();
            headers.put("Accept", "text/html,application/xhtml+xml,*/*;q=0.8");
            headers.put("Origin", BASE_URL);
            headers.put("Referer", BASE_URL + "/");
            HttpResult resp = HttpClient.get(BASE_URL + "/api/message/"
                    + ProviderUtil.urlEncode(id) + "/body", headers);
            if (!resp.isOk()) {
                return "";
            }
            return resp.getBody();
        } catch (RuntimeException ignored) {
            return "";
        }
    }

    /**
     * 将 ISO 8601 时间串转为毫秒时间戳。
     *
     * @param iso ISO 时间串
     * @return 毫秒时间戳，解析失败返回 null
     */
    private static Long parseIsoMillis(String iso) {
        if (iso == null || iso.isEmpty()) {
            return null;
        }
        try {
            return java.time.OffsetDateTime.parse(iso.replace("Z", "+00:00"))
                    .toInstant().toEpochMilli();
        } catch (RuntimeException ignored) {
            return null;
        }
    }
}