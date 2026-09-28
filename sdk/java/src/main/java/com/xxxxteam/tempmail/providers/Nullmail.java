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
 * NullMail 渠道 — https://www.nullmail.cc（收信域 maildock.store）
 *
 * <p>无认证 REST：POST /api/emails（空 JSON body）建箱，响应
 * {"address":"...@maildock.store","expiry":"2026-09-27T02:25:28.778Z"}；
 * 读信 GET /api/emails/{address}（URL 编码），响应 {"expiry":"...","emails":[...]}，
 * 列表项只有 id/sender/subject/delivered，正文须逐封二拉
 * GET /api/emails/{addr}/body/{id}（响应 {"body":...}）；
 * 续期 PUT /api/emails/{addr}/extend/1h 暂不接入。</p>
 */
public final class Nullmail {

    private static final String BASE_URL = "https://www.nullmail.cc";
    private static final String CHANNEL = "nullmail";

    private Nullmail() {
    }

    /**
     * 构造 nullmail 请求的通用请求头（含 Origin/Referer 站点特征）。
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
     * 创建 nullmail 临时邮箱（POST /api/emails，空 JSON body），token 复用完整地址。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        HttpResult resp = HttpClient.post(BASE_URL + "/api/emails",
                "{}", "application/json", baseHeaders());
        if (!resp.isOk()) {
            throw new RuntimeException("nullmail 建箱: http " + resp.getStatusCode()
                    + " " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("nullmail 建箱: 解析响应失败");
        }
        String addr = Json.str(data, "address").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("nullmail 建箱: 响应缺少 address 字段");
        }
        String expiry = Json.str(data, "expiry").trim();
        return new EmailInfo(CHANNEL, addr, addr,
                expiry.isEmpty() ? null : parseIsoMillis(expiry), null);
    }

    /**
     * 单封正文二拉（GET /api/emails/{addr}/body/{id}），失败返回空串。
     *
     * @param addr 邮箱地址
     * @param id   邮件 ID
     * @return 纯文本正文，失败返回空串
     */
    private static String fetchBody(String addr, String id) {
        try {
            HttpResult resp = HttpClient.get(BASE_URL + "/api/emails/"
                    + ProviderUtil.urlEncode(addr) + "/body/" + ProviderUtil.urlEncode(id),
                    baseHeaders());
            if (!resp.isOk()) {
                return "";
            }
            JsonObject data = Json.parseObject(resp.getBody());
            if (data == null) {
                return "";
            }
            return Json.str(data, "body").trim();
        } catch (RuntimeException ignored) {
            return "";
        }
    }

    /**
     * 读取收件箱（GET /api/emails/{address}）。列表项无正文，
     * 逐封二拉 body 端点取纯文本正文，失败降级留空不阻断列表。
     *
     * @param token 复用完整地址的令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("nullmail 读信: 邮箱地址为空");
        }
        HttpResult resp = HttpClient.get(BASE_URL + "/api/emails/"
                + ProviderUtil.urlEncode(addr), baseHeaders());
        if (!resp.isOk()) {
            throw new RuntimeException("nullmail 读信: http " + resp.getStatusCode()
                    + " " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            return new ArrayList<>();
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
            JsonObject msg = item.getAsJsonObject();
            Map<String, Object> flat = Json.toDict(msg);
            flat.put("to", addr);
            // normalizeDate 候选键不含 delivered，显式映射为 date 后归一化
            String delivered = Json.str(msg, "delivered");
            if (!delivered.isEmpty()) {
                flat.put("date", delivered);
            }
            // 列表只有 id/sender/subject/delivered，正文逐封二拉 body 端点
            String id = Json.str(msg, "id");
            if (!id.isEmpty()) {
                String body = fetchBody(addr, id);
                if (!body.isEmpty()) {
                    flat.put("text", body);
                }
            }
            out.add(Normalizer.normalizeEmail(flat, addr));
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