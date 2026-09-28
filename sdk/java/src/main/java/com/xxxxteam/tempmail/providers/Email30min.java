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
 * 30minemail 渠道 — https://30minemail.com
 *
 * <p>建箱 GET /?generate 返回完整 HTML 页面，从中解析 "@30minemail.com" 的本地名
 * （向前回溯到空白/&gt;/引号处）；读信 GET /messages.php?email=&lt;URL 编码完整地址&gt;
 * &amp;_=&lt;unix 毫秒&gt;，响应 {"ok":true,"expired":false,"count","emails":[...]}。
 * ok 非真或 expired 均为错误。token 复用完整地址。</p>
 */
public final class Email30min {

    private static final String BASE_URL = "https://30minemail.com";
    private static final String DOMAIN = "30minemail.com";
    private static final String CHANNEL = "30minemail";

    private Email30min() {
    }

    /**
     * 从建箱页面 HTML 中定位 "@30minemail.com"，向前回溯本地名起点
     * （空白、&gt;、引号之后），提取完整的本地名。本地名过短视为解析异常。
     *
     * @param page 建箱页面 HTML
     * @return 完整邮箱地址
     */
    private static String extractAddress(String page) {
        int idx = page.indexOf("@" + DOMAIN);
        if (idx < 0) {
            throw new RuntimeException("30minemail: 创建页面未找到邮箱地址");
        }
        int start = idx;
        while (start > 0) {
            char c = page.charAt(start - 1);
            if (c == ' ' || c == '\n' || c == '\t' || c == '>' || c == '"') {
                break;
            }
            start--;
        }
        String local = page.substring(start, idx).trim();
        if (local.length() < 8) {
            throw new RuntimeException("30minemail: 创建页面解析地址异常: "
                    + page.substring(start, idx + DOMAIN.length() + 1));
        }
        return local + "@" + DOMAIN;
    }

    /**
     * 创建 30minemail.com 临时邮箱（GET /?generate 服务端建箱，token 复用完整地址）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8");
        HttpResult resp = HttpClient.get(BASE_URL + "/?generate", headers);
        if (!resp.isOk()) {
            throw new RuntimeException("30minemail: 创建邮箱失败 http " + resp.getStatusCode());
        }
        String address = extractAddress(resp.getBody());
        return new EmailInfo(CHANNEL, address, address, null, null);
    }

    /**
     * 读取收件箱（GET /messages.php?email=&lt;完整地址&gt;&amp;_=&lt;unix 毫秒&gt;，
     * 模拟官方轮询参数）。ok 非真或 expired 即为收件箱不可用。
     *
     * @param token 复用完整地址的令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("30minemail: 邮箱地址为空");
        }
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.get(BASE_URL + "/messages.php?email="
                + ProviderUtil.urlEncode(addr) + "&_=" + System.currentTimeMillis(), headers);
        if (!resp.isOk()) {
            throw new RuntimeException("30minemail: 读取收件箱失败 http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("30minemail: 解析收件箱响应失败");
        }
        boolean ok = "true".equals(Json.str(data, "ok"));
        boolean expired = "true".equals(Json.str(data, "expired"));
        if (!ok || expired) {
            throw new RuntimeException("30minemail: 收件箱不可用或已过期: "
                    + resp.getBody().trim());
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
            // 列表元素无 to 字段，注入收件人地址
            flat.put("to", addr);
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }
}