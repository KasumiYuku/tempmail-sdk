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
 * FireTempMail 渠道 — https://firetempmail.com（API 域 mail.firetempmail.com）
 *
 * <p>无认证 REST：建箱无需请求，本地生成 随机词+0-999@&lt;域&gt;
 * （域池与官网一致：offrework.click / service-today.click / jobsdeforyou.sa.com）；
 * 读信 GET https://mail.firetempmail.com/mail/get?address=&lt;邮箱 URL 编码&gt;，
 * 必须携带 Header Origin: https://firetempmail.com（否则 403 'Origin not allowed'）。
 * 响应形如 {"status":"ok","code":200,"msg":"...","stats":{},"mails":[...]}，
 * 邮件字段以 sender/subject/date/recipient/suffix + content-html/content-text/content-plain
 * 多候选归一化。</p>
 */
public final class Firetempmail {

    private static final String API_BASE = "https://mail.firetempmail.com";
    private static final String ORIGIN = "https://firetempmail.com";
    private static final String CHANNEL = "firetempmail";

    /** 官网 JS chunk 中的完整平台域池，顺序与官网一致。 */
    private static final String[] DOMAINS =
            {"offrework.click", "service-today.click", "jobsdeforyou.sa.com"};

    private Firetempmail() {
    }

    /**
     * 随机小写单词（3-6 位）+ 0-999（与官网 faker unique 词 + 1e3 取整一致）。
     *
     * @return 本地名
     */
    private static String localPart() {
        int n = 3 + ThreadLocalRandom.current().nextInt(4);
        StringBuilder sb = new StringBuilder(n);
        for (int i = 0; i < n; i++) {
            sb.append((char) ('a' + ThreadLocalRandom.current().nextInt(26)));
        }
        return sb.append(ThreadLocalRandom.current().nextInt(1000)).toString();
    }

    /**
     * 创建 firetempmail 临时邮箱（建箱无需请求，本地生成随机词+0-999@域名）。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        String domain = DOMAINS[ThreadLocalRandom.current().nextInt(DOMAINS.length)];
        String email = localPart() + "@" + domain;
        // token 复用完整地址：注册层对空 token 有统一兜底
        return new EmailInfo(CHANNEL, email, email, null, null);
    }

    /**
     * 读取收件箱（GET /mail/get?address=&lt;URL 编码完整邮箱&gt;，必带 Origin 头）。
     *
     * @param token 复用完整地址的令牌
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("firetempmail 读信: 邮箱地址为空");
        }
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Accept", "application/json");
        headers.put("Origin", ORIGIN);
        headers.put("Referer", ORIGIN + "/");
        HttpResult resp = HttpClient.get(API_BASE + "/mail/get?address="
                + ProviderUtil.urlEncode(addr), headers);
        if (!resp.isOk()) {
            throw new RuntimeException("firetempmail 读信: http " + resp.getStatusCode()
                    + " " + resp.getBody().trim());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            return new ArrayList<>();
        }
        String status = Json.str(data, "status");
        if (!status.isEmpty() && !"ok".equals(status)) {
            throw new RuntimeException("firetempmail 读信: " + Json.str(data, "msg"));
        }
        JsonElement mailsEl = data.get("mails");
        if (mailsEl == null || !mailsEl.isJsonArray()) {
            return new ArrayList<>();
        }
        List<Email> out = new ArrayList<>();
        for (JsonElement item : mailsEl.getAsJsonArray()) {
            if (!item.isJsonObject()) {
                continue;
            }
            JsonObject msg = item.getAsJsonObject();
            Map<String, Object> flat = new LinkedHashMap<>();
            // 官网 JSON 无统一 to 字段，收件人固定为当前邮箱
            flat.put("to", addr);
            // 正文多候选：content-html 优先，其次 content-text / content-plain / text / html
            String html = pickStr(msg, "content-html", "html");
            if (!html.isEmpty()) {
                flat.put("html", html);
            }
            String text = pickStr(msg, "content-text", "content-plain", "text");
            if (!text.isEmpty()) {
                flat.put("text", text);
            }
            String from = pickStr(msg, "sender", "from", "from_address");
            if (!from.isEmpty()) {
                flat.put("from", from);
            }
            String subject = pickStr(msg, "subject", "title");
            if (!subject.isEmpty()) {
                flat.put("subject", subject);
            }
            String date = pickStr(msg, "date", "received_at", "created_at");
            if (!date.isEmpty()) {
                flat.put("date", date);
            }
            out.add(Normalizer.normalizeEmail(flat, addr));
        }
        return out;
    }

    /**
     * 多候选字段取值：返回第一个存在且非空的字符串值。
     *
     * @param obj  原始 JSON 对象
     * @param keys 候选键
     * @return 命中字符串，未命中返回空串
     */
    private static String pickStr(JsonObject obj, String... keys) {
        for (String key : keys) {
            if (obj.has(key) && !obj.get(key).isJsonNull()) {
                String v = Json.nodeToString(obj.get(key)).trim();
                if (!v.isEmpty()) {
                    return v;
                }
            }
        }
        return "";
    }
}