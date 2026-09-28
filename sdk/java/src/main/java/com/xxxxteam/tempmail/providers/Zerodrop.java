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
 * Zerodrop 渠道 — https://zerodrop.dev
 *
 * <p>无认证 REST：建箱无需请求，本地生成 "sdk"+8 位随机名，
 * 地址为 &lt;名&gt;@zerodrop-sandbox.online；读信 GET /api/inbox/{name}?source=sdk，
 * 响应形如 {"emails":[...],"count":N}。</p>
 *
 * <p>平台邮件对象只有 id/from/to/subject/receivedAt/raw/otp/magicLink：正文仅存在于
 * raw（完整 MIME 原文，头部与 body 以 \r\n\r\n 空行分隔），无 text/html 字段，
 * 故须从 raw 中剥离头部提取纯文本 body 填入 text，html 留空由归一化互转。</p>
 */
public final class Zerodrop {

    private static final String BASE_URL = "https://zerodrop.dev";
    private static final String DOMAIN = "zerodrop-sandbox.online";
    private static final String CHANNEL = "zerodrop";

    private Zerodrop() {
    }

    /**
     * 生成 "sdk"+8 位随机本地名。
     *
     * @return 本地名
     */
    private static String localName() {
        return "sdk" + ProviderUtil.randomString(8);
    }

    /**
     * 从 raw（完整 MIME 原文）提取纯文本正文：定位首个空行
     * （RFC 5322 头部/正文分隔，\r\n\r\n 或 \n\n），其后部分即 body。
     *
     * @param raw 完整 MIME 原文
     * @return 纯文本正文，未找到分隔返回空串
     */
    private static String rawBody(String raw) {
        int idx = raw.indexOf("\r\n\r\n");
        if (idx >= 0) {
            return raw.substring(idx + 4);
        }
        idx = raw.indexOf("\n\n");
        if (idx >= 0) {
            return raw.substring(idx + 2);
        }
        return "";
    }

    /**
     * 创建 zerodrop 临时邮箱（建箱无需请求，本地生成随机名，token 复用完整地址）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        String email = localName() + "@" + DOMAIN;
        return new EmailInfo(CHANNEL, email, email, null, null);
    }

    /**
     * 读取收件箱（GET /api/inbox/{name}?source=sdk），从 raw 中提取纯文本正文。
     *
     * @param token 复用完整地址的令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        String[] parts = addr.split("@", 2);
        if (parts.length != 2 || !DOMAIN.equals(parts[1])) {
            throw new RuntimeException("zerodrop 读信: 非 " + DOMAIN + " 域邮箱地址");
        }
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        HttpResult resp = HttpClient.get(BASE_URL + "/api/inbox/"
                + ProviderUtil.urlEncode(parts[0]) + "?source=sdk", headers);
        resp.ensureSuccess();

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
            // 平台响应无 to 字段，收件人固定为当前邮箱
            flat.put("to", addr);
            // 正文仅存在于 raw（完整 MIME 原文），无 text/html 字段：提取纯文本 body 作 text
            String raw = Json.str(msg, "raw");
            String text = rawBody(raw);
            if (!text.isEmpty()) {
                flat.put("text", text);
            }
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }
}