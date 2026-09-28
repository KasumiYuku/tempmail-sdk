package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.xxxxteam.tempmail.Email;
import com.xxxxteam.tempmail.EmailInfo;
import com.xxxxteam.tempmail.HttpResult;
import com.xxxxteam.tempmail.HttpClient;
import com.xxxxteam.tempmail.Json;
import com.xxxxteam.tempmail.Normalizer;

import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Tenmin.app 渠道 — https://tenmin.app（真实 API 域 api.tenmin.app）
 *
 * <p>建箱+读信共用 GET /api/inbox/{localpart}（Accept: application/json）。
 * localpart 为随机 6 位小写十六进制串（首访即建箱，无显式创建接口）；
 * 响应：{"inboxId":..,"address":"..@tenmin.app","ttl":600,"count":0,"messages":[]}，
 * messages[] 元素字段：id/from/subject/text/html/receivedAt（from 为 {name,address} 对象）。</p>
 */
public final class TenminApp {

    private static final String BASE_URL = "https://api.tenmin.app";
    private static final String CHANNEL = "tenmin-app";
    private static final String HEX = "0123456789abcdef";

    private TenminApp() {
    }

    /**
     * 生成 6 位小写十六进制随机 localpart。
     *
     * @return localpart
     */
    private static String localPart() {
        StringBuilder sb = new StringBuilder(6);
        for (int i = 0; i < 6; i++) {
            sb.append(HEX.charAt(java.util.concurrent.ThreadLocalRandom.current().nextInt(16)));
        }
        return sb.toString();
    }

    /**
     * 请求 GET /api/inbox/{localpart}。
     *
     * @param localpart 邮箱 localpart
     * @return 解析后的收件箱响应
     */
    private static JsonObject fetchInbox(String localpart) {
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.get(BASE_URL + "/api/inbox/" + localpart, headers);
        resp.ensureSuccess();
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("tenmin-app: 收件箱响应解析失败");
        }
        return data;
    }

    /**
     * 创建 tenmin.app 临时邮箱（首次 GET 随机 localpart 即自动建箱，10 分钟 TTL）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        String local = localPart();
        JsonObject data = fetchInbox(local);
        String address = Json.str(data, "address").trim();
        if (address.isEmpty()) {
            address = local + "@tenmin.app";
        }
        String ttl = Json.str(data, "ttl");
        long ttlSec = 0;
        try {
            ttlSec = Long.parseLong(ttl);
        } catch (NumberFormatException ignored) {
            // ttl 缺失时 expiresAt 置 null
        }
        Long expiresMs = null;
        if (ttlSec > 0) {
            expiresMs = Instant.now().plusSeconds(ttlSec).toEpochMilli();
        }
        return new EmailInfo(CHANNEL, address, local, expiresMs, null);
    }

    /**
     * 读取收件箱（复用建箱同一 localpart 轮询；from 为 {name,address} 对象时拆出地址字段）。
     *
     * @param token 建箱时下发的 localpart
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("tenmin-app: token 为空");
        }
        JsonObject data = fetchInbox(tok);
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
            // from 为对象（{name,address}）时拆出地址字段，展开后交归一化处理
            JsonElement fromEl = msg.get("from");
            if (fromEl != null && fromEl.isJsonObject()) {
                JsonObject fromObj = fromEl.getAsJsonObject();
                String addr = Json.str(fromObj, "address").trim();
                String name = Json.str(fromObj, "name").trim();
                if (!addr.isEmpty() && !name.isEmpty()) {
                    flat.put("from", name + " <" + addr + ">");
                } else if (!addr.isEmpty()) {
                    flat.put("from", addr);
                } else {
                    flat.put("from", name);
                }
            }
            flat.put("to", email);
            flat.put("text", Json.str(msg, "text"));
            flat.put("html", Json.str(msg, "html"));
            flat.put("date", Json.str(msg, "receivedAt"));
            out.add(Normalizer.normalizeEmail(flat, email));
        }
        return out;
    }
}