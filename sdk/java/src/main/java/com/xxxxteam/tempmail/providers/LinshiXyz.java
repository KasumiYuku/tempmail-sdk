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
import java.util.concurrent.ThreadLocalRandom;

/**
 * LinshiXYZ 渠道 — https://linshi.xyz
 *
 * <p>无建箱请求：本地随机 6 位 hex 前缀（官网 client 即用短 id）+ @linshi.xyz，
 * token 复用完整地址。读信 GET /api/mails/{前缀}，无邮件返回空数组 []，
 * 有邮件为对象数组（元素 {headers:{from,to,subject,date},html}），
 * 需把 headers 平铺到顶层后归一；响应非数组骨架整体报错，交给上层 fallback。</p>
 */
public final class LinshiXyz {

    private static final String BASE_URL = "https://linshi.xyz";
    private static final String DOMAIN = "linshi.xyz";
    private static final String CHANNEL = "linshi-xyz";

    private LinshiXyz() {
    }

    /**
     * 生成本地随机 6 位 hex 前缀（与官网 client 相同格式）。
     *
     * @return 6 位 hex 本地名
     */
    private static String localName() {
        char[] hex = "0123456789abcdef".toCharArray();
        StringBuilder sb = new StringBuilder(6);
        ThreadLocalRandom r = ThreadLocalRandom.current();
        for (int i = 0; i < 6; i++) {
            sb.append(hex[r.nextInt(hex.length)]);
        }
        return sb.toString();
    }

    /**
     * 创建 linshi.xyz 临时邮箱（无需建箱请求，本地生成前缀，token 复用完整地址）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        String email = localName() + "@" + DOMAIN;
        return new EmailInfo(CHANNEL, email, email, null, null);
    }

    /**
     * 读取收件箱（GET /api/mails/{前缀}）。headers 对象平铺为顶层字段，
     * 无 to 字段时注入收件人地址；响应非数组骨架整体报错。
     *
     * @param token 复用完整地址的令牌
     * @param email 邮箱地址（前缀@linshi.xyz）
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty() || !addr.contains("@")) {
            throw new RuntimeException("linshi-xyz: 邮箱地址无效: \"" + addr + "\"");
        }
        String local = addr.split("@", 2)[0];

        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.get(BASE_URL + "/api/mails/" + local, headers);
        if (!resp.isOk()) {
            throw new RuntimeException("linshi-xyz: 读取收件箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonElement parsed = Json.parse(resp.getBody());
        if (parsed == null || !parsed.isJsonArray()) {
            throw new RuntimeException("linshi-xyz: 解析收件箱响应失败");
        }

        List<Email> out = new ArrayList<>();
        for (JsonElement item : parsed.getAsJsonArray()) {
            if (!item.isJsonObject()) {
                continue;
            }
            JsonObject msg = item.getAsJsonObject();
            Map<String, Object> flat = Json.toDict(msg);
            // 展开嵌套 headers（headers.{from,to,subject,date} 平铺到顶层，覆盖同名键）
            JsonElement headersEl = msg.get("headers");
            if (headersEl != null && headersEl.isJsonObject()) {
                for (Map.Entry<String, Object> kv : Json.toDict(headersEl).entrySet()) {
                    flat.put(kv.getKey(), kv.getValue());
                }
            }
            // 注入收件人地址
            flat.put("to", addr);
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }
}