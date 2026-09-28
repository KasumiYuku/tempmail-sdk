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
 * MailTicking 渠道 — https://www.mailticking.com（旧域名 temporary-mail.net 的更名站）
 *
 * <p>三步协议（均 application/json POST）：
 * 1) 建箱 POST /get-mailbox body {"types":["4"]}（4=独立域名，排除 Gmail 别名），
 *    响应 {"success":true,"email":"...","activate_token":"..."}；
 * 2) 激活 POST /activate-email {"email","source":"api","activate_token"}；
 * 3) 列信 POST /get-emails?lang=en {"email","code"}，空箱 {"emails":[],"success":true}，
 *    needNewEmail:true 时返回错误提示换箱。</p>
 *
 * <p>官网从未提供独立读信端点，本渠道只具备列表能力；列表字段名在无站点文档
 * 佐证前不做猜测，统一交给多候选字段策略提取。基于此实态，读信保持
 * 列表-only 形态参与维护。</p>
 */
public final class Mailticking {

    private static final String BASE_URL = "https://www.mailticking.com";
    private static final String CHANNEL = "mailticking";

    private Mailticking() {
    }

    /**
     * 构造 mailticking 请求头（含 Referer/Origin 站点特征）。
     *
     * @return 请求头
     */
    private static Map<String, String> buildHeaders() {
        Map<String, String> h = new LinkedHashMap<>();
        h.put("Accept", "application/json");
        h.put("Accept-Language", "en-US,en;q=0.9");
        h.put("Content-Type", "application/json");
        h.put("Referer", BASE_URL + "/");
        h.put("Origin", BASE_URL);
        return h;
    }

    /**
     * 执行 POST 并解析响应；非 2xx 时优先使用响应体中的 error/message 文案。
     *
     * @param path    请求路径（含查询串）
     * @param payload 请求体对象（经 gson 序列化）
     * @return 响应 JSON 对象
     */
    private static JsonObject postJson(String path, Map<String, Object> payload) {
        HttpResult resp = HttpClient.post(path,
                Json.serialize(payload), "application/json", buildHeaders());
        JsonObject data = Json.parseObject(resp.getBody());
        if (!resp.isOk()) {
            if (data != null) {
                String msg = Json.str(data, "error");
                if (msg.isEmpty()) {
                    msg = Json.str(data, "message");
                }
                if (msg.isEmpty()) {
                    msg = resp.getBody().trim();
                }
                throw new RuntimeException("mailticking: http " + resp.getStatusCode()
                        + ": " + msg);
            }
            throw new RuntimeException("mailticking: http " + resp.getStatusCode()
                    + ": " + resp.getBody().trim());
        }
        if (data == null) {
            throw new RuntimeException("mailticking: parse response");
        }
        return data;
    }

    /**
     * 从响应对象提取 error/message 文案（用于 success 非真的报错）。
     *
     * @param data 响应对象
     * @return 错误文案
     */
    private static String failMessage(JsonObject data) {
        String msg = Json.str(data, "error");
        if (msg.isEmpty()) {
            msg = Json.str(data, "message");
        }
        return msg.isEmpty() ? "unknown error" : msg;
    }

    /**
     * 创建 mailticking 邮箱账号：GET /get-mailbox（type=4 固定取独立域名）→
     * POST /activate-email 激活。token 必须携带 activate code（列信协议依赖
     * 激活会话），code 缺失时以 email 兜底。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        Map<String, Object> getBody = new LinkedHashMap<>();
        getBody.put("types", java.util.List.of("4"));
        JsonObject box = postJson(BASE_URL + "/get-mailbox", getBody);
        if (!"true".equals(Json.str(box, "success"))) {
            throw new RuntimeException("mailticking: get-mailbox failed: " + failMessage(box));
        }
        String email = Json.str(box, "email").trim();
        String code = Json.str(box, "code").trim();
        if (code.isEmpty()) {
            code = email;
        }
        if (code.isEmpty()) {
            throw new RuntimeException("mailticking: get-mailbox returned empty email/activate_token");
        }
        if (email.isEmpty()) {
            throw new RuntimeException("mailticking: get-mailbox returned empty email");
        }

        // 激活邮箱，使后续列信请求与服务器记录的最新会话一致
        Map<String, Object> actBody = new LinkedHashMap<>();
        actBody.put("email", email);
        actBody.put("source", "api");
        actBody.put("activate_token", code);
        JsonObject act = postJson(BASE_URL + "/activate-email", actBody);
        if (!"true".equals(Json.str(act, "success"))) {
            throw new RuntimeException("mailticking: activate-email failed: " + failMessage(act));
        }
        return new EmailInfo(CHANNEL, email, code, null, null);
    }

    /**
     * 获取邮件列表（POST /get-emails?lang=en，body {"email","code"}）。
     * 空箱返回空列表不报错；needNewEmail:true 返回换箱语义错误。
     * 列表字段名多候选提取（与 Go mailtickingMigratedFields 同构），
     * 未命中的字段留给 Normalizer 的既有候选策略。
     *
     * @param token 建箱下发的 activate code
     * @param email 邮箱地址
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("mailticking: empty email");
        }
        String tok = (token != null ? token : "").trim();
        if (tok.isEmpty()) {
            throw new RuntimeException("mailticking: empty activate code");
        }
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("email", addr);
        body.put("code", tok);
        JsonObject data = postJson(BASE_URL + "/get-emails?lang=en", body);
        if (!"true".equals(Json.str(data, "success"))) {
            if ("true".equals(Json.str(data, "needNewEmail"))) {
                throw new RuntimeException("mailticking: mailbox expired, please renew");
            }
            throw new RuntimeException("mailticking: get-emails failed");
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
            out.add(Normalizer.normalizeEmail(migratedFields(item.getAsJsonObject()), addr));
        }
        return out;
    }

    /**
     * 将列表字段尽量映射到统一候选字段（发件人多候选 → from；
     * from 兜底 sender；mail_id → id；received_at → date），
     * 未命中的字段保留原样交给 Normalizer。
     *
     * @param raw 列表元素
     * @return 迁移后的字段字典
     */
    private static Map<String, Object> migratedFields(JsonObject raw) {
        Map<String, Object> flat = Json.toDict(raw);
        String[] fromCandidates = {"mail_from", "from_mail", "from_email", "sender_address",
                "from_address", "send_addr", "mail_addr", "address_from", "ho_from",
                "fromname", "fromS"};
        if (!flat.containsKey("from")) {
            for (String key : fromCandidates) {
                Object v = flat.get(key);
                if (v instanceof String && !((String) v).trim().isEmpty()) {
                    flat.put("from", v);
                    break;
                }
            }
        }
        if (!flat.containsKey("sender")) {
            Object from = flat.get("from");
            if (from instanceof String && !((String) from).trim().isEmpty()) {
                flat.put("sender", from);
            }
        }
        if (!flat.containsKey("id")) {
            Object mailId = flat.get("mail_id");
            if (mailId != null) {
                flat.put("id", mailId);
            }
        }
        if (!flat.containsKey("date")) {
            Object receivedAt = flat.get("received_at");
            if (receivedAt != null) {
                flat.put("date", receivedAt);
            }
        }
        return flat;
    }
}