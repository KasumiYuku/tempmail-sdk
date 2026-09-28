package com.xxxxteam.tempmail.providers;

import com.google.gson.JsonElement;
import com.google.gson.JsonObject;
import com.xxxxteam.tempmail.Email;
import com.xxxxteam.tempmail.EmailInfo;
import com.xxxxteam.tempmail.HttpClient;
import com.xxxxteam.tempmail.HttpResult;
import com.xxxxteam.tempmail.Json;

import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * TempMailGG 渠道 — https://temp-mail.gg（Laravel + Livewire v3）。
 *
 * <p>纯 HTTP 协议（无验证码）：POST /livewire/update 是唯一边界，JSON body
 * 顶层 _token（=data-csrf）+ components[0] {snapshot, updates:{}, calls:[...]}。
 * generateEmail 生成邮箱并回传新 snapshot；calls 为空的 update 即平台轮询
 * 刷信形态；详情 selectEmail(params=[数字id]) 返回模态框 effects.html。</p>
 *
 * <p>Java 的 HttpClient 默认无 Cookie 罐，本类在类内维护静态会话 Cookie 串，
 * 逐响应合并 Set-Cookie（Laravel 每次 livewire/update 都轮换会话 Cookie，
 * 同值重放会 419，必须以响应值覆写后续请求）。</p>
 *
 * <p>Token 格式：{@code temp-mail-gg|{email,csrf,snapshot}}（snapshot 为快照
 * 整段文本）。GetEmails 复用 token 中的快照轮询，并以响应快照 data.email
 * 断言会话仍指向本邮箱（防全局 Cookie 状态被并行会话覆盖后串箱）。</p>
 */
public final class TempMailGg {

    private static final String BASE_URL = "https://temp-mail.gg";
    private static final String CHANNEL = "temp-mail-gg";

    /** Token 前缀，用于识别本渠道会话凭据串。 */
    private static final String TOKEN_PREFIX = "temp-mail-gg|";

    /** 固定浏览器 UA（Chrome 154 / Linux，与各端一致）。 */
    private static final String UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36"
            + " (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /** 首页 data-csrf 令牌属性正则。 */
    private static final Pattern DATA_CSRF_RE = Pattern.compile("data-csrf=\"([^\"]*)\"");

    /** 首页 wire:snapshot 初始快照正则（HTML 属性内为双转义态，须反转义一层）。 */
    private static final Pattern SNAPSHOT_RE = Pattern.compile("wire:snapshot=\"([^\"]*)\"");

    /** 列表条目起点正则（wire:click=selectEmail(数字id) 的 div）。 */
    private static final Pattern ROW_RE =
            Pattern.compile("<div[^>]*wire:click=\"selectEmail\\((\\d+)\\)\"[^>]*>");

    /** 去 script/style 正则（DOTALL 语义）。 */
    private static final Pattern SCRIPT_STYLE_RE =
            Pattern.compile("(?is)<(script|style)[\\s\\S]*?</\\1>");

    /** 去标签正则。 */
    private static final Pattern TAG_RE = Pattern.compile("(?s)<[^>]+>");

    /** 空白压缩正则。 */
    private static final Pattern WS_RE = Pattern.compile("\\s+");

    /** 相对时间正则（"N seconds/minutes/hours/days ago"）。 */
    private static final Pattern RELATIVE_RE =
            Pattern.compile("(?i)^\\s*(\\d+)\\s+(second|minute|hour|day)s?\\s+ago\\s*$");

    /**
     * 类内静态会话 Cookie 状态（键值合并后的 "k=v; k2=v2" 串）。
     * Laravel 每次 livewire/update 轮换会话 Cookie，逐响应覆写；访问与
     * 覆写均持锁，保证并发场景下的可见性与原子更新。
     */
    private static final Object COOKIE_LOCK = new Object();
    private static String sessionCookies = "";

    /** 私有构造，防止实例化。 */
    private TempMailGg() {
    }

    /**
     * 解析响应 Set-Cookie 头（取首分号之前的 name=value），与已有会话串
     * 按键合并（同键覆写），返回新的 Cookie 请求头值。
     *
     * @param existing 已有会话串
     * @param resp     响应
     * @return 合并后的 Cookie 请求头值
     */
    private static String mergeCookies(String existing, HttpResult resp) {
        Map<String, String> map = new LinkedHashMap<>();
        for (String part : existing.split(";")) {
            String kv = part.trim();
            int eq = kv.indexOf('=');
            if (eq > 0) {
                map.put(kv.substring(0, eq).trim(), kv.substring(eq + 1).trim());
            }
        }
        for (String raw : resp.getSetCookies()) {
            String head = raw.split(";", 2)[0].trim();
            int eq = head.indexOf('=');
            if (eq > 0) {
                map.put(head.substring(0, eq).trim(), head.substring(eq + 1).trim());
            }
        }
        StringBuilder sb = new StringBuilder();
        for (Map.Entry<String, String> e : map.entrySet()) {
            if (sb.length() > 0) {
                sb.append("; ");
            }
            sb.append(e.getKey()).append('=').append(e.getValue());
        }
        return sb.toString();
    }

    /**
     * 读取会话 Cookie（线程安全）。
     *
     * @return 会话 Cookie 串，空串表示会话未建立
     */
    private static String currentCookies() {
        synchronized (COOKIE_LOCK) {
            return sessionCookies;
        }
    }

    /**
     * 覆写会话 Cookie（线程安全，响应返回后调用）。
     *
     * @param cookies 最新合并结果
     */
    private static void updateCookies(String cookies) {
        synchronized (COOKIE_LOCK) {
            sessionCookies = cookies;
        }
    }

    /**
     * 设置 livewire/update 同步请求头（同站 fetch 全套；Content-Type 由
     * {@link HttpClient#post} 的 contentType 参数注入，避免重复头）。
     *
     * @param h 请求头
     */
    private static void postHeaders(Map<String, String> h) {
        h.put("User-Agent", UA);
        h.put("Accept", "text/html, application/xhtml+xml");
        h.put("Accept-Language", "en-US,en;q=0.9");
        h.put("X-Livewire", "");
        h.put("X-Requested-With", "XMLHttpRequest");
        h.put("Origin", BASE_URL);
        h.put("Referer", BASE_URL + "/");
        h.put("Cookie", currentCookies());
    }

    /**
     * 调用 livewire/update（按响应 Set-Cookie 即时覆写会话 Cookie）。
     *
     * @param snapshot 当前组件快照 JSON 串
     * @param csrf     data-csrf 令牌（顶层 _token）
     * @param method   组件方法（空串表示纯轮询，calls 为空列表）
     * @param params   方法参数
     * @return 解析后的 update 响应
     */
    private static JsonObject update(String snapshot, String csrf, String method, List<Object> params) {
        List<Map<String, Object>> calls = new ArrayList<>();
        if (method != null && !method.isEmpty()) {
            Map<String, Object> call = new LinkedHashMap<>();
            call.put("path", "");
            call.put("method", method);
            call.put("params", params != null ? params : new ArrayList<>());
            calls.add(call);
        }
        Map<String, Object> component = new LinkedHashMap<>();
        component.put("snapshot", snapshot);
        component.put("updates", new LinkedHashMap<String, Object>());
        component.put("calls", calls);
        Map<String, Object> payload = new LinkedHashMap<>();
        payload.put("_token", csrf);
        payload.put("components", List.of(component));

        Map<String, String> h = new LinkedHashMap<>();
        postHeaders(h);
        HttpResult resp = HttpClient.post(BASE_URL + "/livewire/update",
                Json.serialize(payload), "application/json", h);
        updateCookies(mergeCookies(currentCookies(), resp));
        if (resp.getStatusCode() == 419) {
            throw new RuntimeException("temp-mail-gg: livewire 会话过期（419），请重新 Generate");
        }
        if (!resp.isOk()) {
            throw new RuntimeException("temp-mail-gg: livewire/update http " + resp.getStatusCode());
        }
        JsonObject data = Json.parseObject(resp.getBody());
        if (data == null) {
            throw new RuntimeException("temp-mail-gg: 解析 update 响应失败");
        }
        return data;
    }

    /**
     * 解析快照 JSON 串并按 data 键提取字符串。
     *
     * @param snapshot 快照 JSON 串
     * @return data 子对象，解析失败返回 null
     */
    private static JsonObject snapshotData(String snapshot) {
        JsonObject snap = Json.parseObject(snapshot);
        if (snap == null) {
            return null;
        }
        JsonElement dataEl = snap.get("data");
        return dataEl != null && dataEl.isJsonObject() ? dataEl.getAsJsonObject() : null;
    }

    /**
     * 取 update 响应第一个组件的原始快照文本。
     *
     * @param resp update 响应
     * @return 快照文本，缺失返回空串
     */
    private static String firstSnapshot(JsonObject resp) {
        JsonElement compsEl = resp != null ? resp.get("components") : null;
        if (compsEl == null || !compsEl.isJsonArray() || compsEl.getAsJsonArray().size() == 0) {
            return "";
        }
        JsonElement c0 = compsEl.getAsJsonArray().get(0);
        if (c0 == null || !c0.isJsonObject()) {
            return "";
        }
        return Json.str(c0.getAsJsonObject(), "snapshot");
    }

    /**
     * 取 update 响应第一个组件的 effects.html 文本。
     *
     * @param resp update 响应
     * @return effects.html，缺失返回空串
     */
    private static String firstEffectsHtml(JsonObject resp) {
        JsonElement compsEl = resp != null ? resp.get("components") : null;
        if (compsEl == null || !compsEl.isJsonArray() || compsEl.getAsJsonArray().size() == 0) {
            return "";
        }
        JsonElement c0 = compsEl.getAsJsonArray().get(0);
        if (c0 == null || !c0.isJsonObject()) {
            return "";
        }
        JsonElement effectsEl = c0.getAsJsonObject().get("effects");
        if (effectsEl == null || !effectsEl.isJsonObject()) {
            return "";
        }
        return Json.str(effectsEl.getAsJsonObject(), "html");
    }

    /**
     * 创建 temp-mail.gg 临时邮箱：GET 首页取 data-csrf + 初始快照 →
     * update generateEmail → 响应快照取 data.email，并把
     * {email, csrf, snapshot} 打成凭据串。邮箱约 30 分钟过期。
     *
     * @return 邮箱信息
     */
    public static EmailInfo generate() {
        Map<String, String> h = new LinkedHashMap<>();
        h.put("User-Agent", UA);
        h.put("Accept",
                "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8");
        h.put("Accept-Language", "en-US,en;q=0.9");
        h.put("Cookie", currentCookies());
        HttpResult resp = HttpClient.get(BASE_URL, h);
        updateCookies(mergeCookies(currentCookies(), resp));
        if (!resp.isOk()) {
            throw new RuntimeException("temp-mail-gg: 建立会话失败: http " + resp.getStatusCode());
        }

        String csrf = match1(DATA_CSRF_RE, resp.getBody()).trim();
        String snapshotAttr = match1(SNAPSHOT_RE, resp.getBody());
        if (csrf.isEmpty() || snapshotAttr.isEmpty()) {
            throw new RuntimeException("temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱");
        }
        // HTML 属性内的快照 JSON 是双转义态（&quot;），逐层反转义为原始 JSON
        String snapshot = unescapeHtml(snapshotAttr);

        // generateEmail：平台免费额度为免登录每时段 5 个，耗尽时响应无 email
        JsonObject updateResp = update(snapshot, csrf, "generateEmail", List.of());
        String snap2Raw = firstSnapshot(updateResp);
        if (snap2Raw.isEmpty()) {
            throw new RuntimeException("temp-mail-gg: generateEmail 响应异常（components 缺失）");
        }
        JsonObject data2 = snapshotData(snap2Raw);
        String email = data2 != null ? Json.str(data2, "email").trim() : "";
        if (email.isEmpty()) {
            throw new RuntimeException("temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录配额（每时段 5 个）");
        }

        Map<String, Object> sess = new LinkedHashMap<>();
        sess.put("email", email);
        sess.put("csrf", csrf);
        sess.put("snapshot", snap2Raw);
        String token = TOKEN_PREFIX + Json.serialize(sess);
        return new EmailInfo(CHANNEL, email, token, null, null);
    }

    /**
     * 正则提取第一个捕获组。
     *
     * @param re  正则
     * @param src 源文本
     * @return 捕获组内容，未命中返回空串
     */
    private static String match1(Pattern re, String src) {
        Matcher m = re.matcher(src == null ? "" : src);
        return m.find() ? m.group(1) : "";
    }

    /**
     * 解析凭据串（prefix | {email,csrf,snapshot} JSON）。
     *
     * @param token 凭据串
     * @return 会话凭据对象
     */
    private static JsonObject decodeSession(String token) {
        String tok = token != null ? token : "";
        if (!tok.startsWith(TOKEN_PREFIX)) {
            throw new RuntimeException("temp-mail-gg: 凭据串前缀不符，请重新 Generate");
        }
        JsonObject sess = Json.parseObject(tok.substring(TOKEN_PREFIX.length()));
        if (sess == null) {
            throw new RuntimeException("temp-mail-gg: 解析凭据串失败");
        }
        if (Json.str(sess, "email").isEmpty() || Json.str(sess, "snapshot").isEmpty()) {
            throw new RuntimeException("temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate");
        }
        return sess;
    }

    /**
     * 解析轮询响应 effects.html 的 Inbox 条目：以
     * div[wire:click=selectEmail(数字id)] 为块起点，块内
     * h3(font-semibold)=发件人、p(text-zinc-300)=主题、
     * p(line-clamp-2)=预览、span(text-xs)=相对时间。
     *
     * @param html effects.html 区块
     * @return 收件箱条目列表（id/from/subject/preview/when）
     */
    private static List<Map<String, String>> parseList(String html) {
        List<Map<String, String>> rows = new ArrayList<>();
        Matcher m = ROW_RE.matcher(html);
        int prevStart = -1;
        while (m.find()) {
            if (prevStart >= 0) {
                addRow(rows, html, prevStart, m.start());
            }
            prevStart = m.start();
        }
        if (prevStart >= 0) {
            addRow(rows, html, prevStart, html.length());
        }
        return rows;
    }

    /**
     * 解析单个条目块（从块起点到下一个条目块起点/结尾）的字段并加入列表。
     *
     * @param rows  输出列表
     * @param html  完整 HTML
     * @param start 块起点
     * @param end   块终点（不含）
     */
    private static void addRow(List<Map<String, String>> rows, String html, int start, int end) {
        String chunk = html.substring(start, Math.min(end, html.length()));
        String id = match1(ROW_RE, chunk);
        if (id.isEmpty()) {
            return;
        }
        Map<String, String> row = new LinkedHashMap<>();
        row.put("id", id);
        row.put("from", htmlText(tagOf(chunk, "h3", "font-semibold")));
        row.put("subject", htmlText(tagOf(chunk, "p", "text-zinc-300")));
        row.put("preview", htmlText(tagOf(chunk, "p", "line-clamp-2")));
        row.put("when", htmlText(tagOf(chunk, "span", "text-xs")));
        rows.add(row);
    }

    /**
     * 提取指定标签、class 含指定类名的元素内部 HTML。
     *
     * @param src    源 HTML
     * @param tag    标签名
     * @param cls    类名
     * @return 内部 HTML，未命中返回空串
     */
    private static String tagOf(String src, String tag, String cls) {
        Pattern p = Pattern.compile("(?is)<" + tag + "[^>]*class=\"[^\"]*\\b"
                + Pattern.quote(cls) + "\\b[^\"]*\"[^>]*>([\\s\\S]*?)</" + tag + ">");
        return match1(p, src);
    }

    /**
     * 解析 selectEmail 详情视图（模态框）的正文/发件人/主题。
     * h3(text-xl)=主题；span 文本以 "From:" 前缀取发件人；
     * div[x-show 含 activeTab === 'text']=纯文本正文。
     *
     * @param html effects.html 区块
     * @return [from, subject, text]，无可识别内容返回 null
     */
    private static String[] parseDetail(String html) {
        String subject = htmlText(tagOf(html, "h3", "text-xl"));
        String from = "";
        Matcher spanM = Pattern.compile("(?is)<span[^>]*>([\\s\\S]*?)</span>").matcher(html);
        while (spanM.find()) {
            String txt = htmlText(spanM.group(1)).trim();
            if (txt.startsWith("From:")) {
                from = txt.substring("From:".length()).trim();
                break;
            }
        }
        Pattern textDiv = Pattern.compile(
                "(?is)<div[^>]*x-show=\"[^\"]*activeTab\\s*===\\s*'text'[^\"]*\"[^>]*>([\\s\\S]*?)</div>");
        String text = htmlText(match1(textDiv, html));
        if (subject.isEmpty() && from.isEmpty() && text.isEmpty()) {
            return null;
        }
        return new String[]{from, subject, text};
    }

    /**
     * 将 HTML 片段转为纯文本（去 script/style/标签、反转义、压缩空白）。
     *
     * @param src HTML 文本
     * @return 纯文本
     */
    private static String htmlText(String src) {
        String cleaned = SCRIPT_STYLE_RE.matcher(src).replaceAll(" ");
        cleaned = TAG_RE.matcher(cleaned).replaceAll(" ");
        return WS_RE.matcher(unescapeHtml(cleaned)).replaceAll(" ").trim();
    }

    /**
     * 标准 HTML 实体反转义。
     *
     * @param s 含实体的文本
     * @return 反转义后的文本
     */
    private static String unescapeHtml(String s) {
        return s.replace("&amp;", "&")
                .replace("&lt;", "<")
                .replace("&gt;", ">")
                .replace("&quot;", "\"")
                .replace("&#39;", "'")
                .replace("&apos;", "'")
                .replace("&nbsp;", " ");
    }

    /**
     * HTML 转义，用于 text 兜底合成 HTML。
     *
     * @param s 纯文本
     * @return 转义后的文本
     */
    private static String escapeHtml(String s) {
        return s.replace("&", "&amp;")
                .replace("<", "&lt;")
                .replace(">", "&gt;")
                .replace("\"", "&quot;")
                .replace("'", "&#39;");
    }

    /**
     * 解析相对时间（"N seconds/minutes/hours/days ago"）为 ISO 8601 时间，
     * 解析失败返回当前时间。
     *
     * @param s 相对时间文本
     * @return ISO 8601 时间戳
     */
    private static String parseRelative(String s) {
        Matcher m = RELATIVE_RE.matcher(s == null ? "" : s.trim());
        Instant now = Instant.now();
        if (!m.find()) {
            return now.toString();
        }
        long n;
        try {
            n = Long.parseLong(m.group(1));
        } catch (NumberFormatException e) {
            return now.toString();
        }
        long unit;
        switch (m.group(2).toLowerCase()) {
            case "minute":
                unit = 60L;
                break;
            case "hour":
                unit = 3600L;
                break;
            case "day":
                unit = 86400L;
                break;
            default:
                unit = 1L;
                break;
        }
        return now.minusSeconds(n * unit).toString();
    }

    /**
     * 读取 temp-mail.gg 收件箱：轮询 update（calls 空）取列表 →
     * 逐封 selectEmail 拉详情正文（详情失败回退列表字段，不中断整批）。
     *
     * @param token 凭据串（prefix | {email,csrf,snapshot}）
     * @param email 邮箱地址（与 token 内会话邮箱一致才继续）
     * @return 邮件列表
     */
    public static List<Email> getEmails(String token, String email) {
        String addr = (email != null ? email : "").trim();
        if (addr.isEmpty()) {
            throw new RuntimeException("temp-mail-gg: 邮箱为空，请重新 Generate");
        }
        JsonObject sess = decodeSession(token);
        if (!Json.str(sess, "email").equalsIgnoreCase(addr)) {
            throw new RuntimeException("temp-mail-gg: 邮箱与凭据不匹配");
        }
        String csrf = Json.str(sess, "csrf");
        String snapshot = Json.str(sess, "snapshot");

        // 轮询刷新（calls 空 = 平台 20 秒自动刷新形态）
        JsonObject pollResp = update(snapshot, csrf, "", List.of());
        String pollSnap = firstSnapshot(pollResp);
        if (pollSnap.isEmpty()) {
            throw new RuntimeException("temp-mail-gg: 轮询响应异常（components 缺失）");
        }
        JsonObject pollData = snapshotData(pollSnap);
        if (pollData != null) {
            String current = Json.str(pollData, "email").trim();
            if (!current.isEmpty() && !current.equalsIgnoreCase(addr)) {
                throw new RuntimeException("temp-mail-gg: 会话已被切换");
            }
        }
        String listHtml = firstEffectsHtml(pollResp);
        if (listHtml.trim().isEmpty()) {
            throw new RuntimeException("temp-mail-gg: 轮询响应无 effects.html，会话可能已失效");
        }

        List<Map<String, String>> rows = parseList(listHtml);
        if (rows.isEmpty()) {
            return new ArrayList<>();
        }

        // 逐封拉详情正文；详情失败回退列表字段
        String latestSnap = pollSnap;
        List<Email> out = new ArrayList<>();
        for (Map<String, String> row : rows) {
            String id = row.get("id");
            if (id.isEmpty()) {
                continue;
            }
            Email ne = new Email();
            ne.setId(id);
            ne.setFrom(row.get("from"));
            ne.setTo(addr);
            ne.setSubject(row.get("subject"));
            ne.setDate(parseRelative(row.get("when")));
            ne.setText(row.get("preview"));
            try {
                long idNum = Long.parseLong(id);
                JsonObject detailResp = update(latestSnap, csrf, "selectEmail", List.of(idNum));
                String detailSnap = firstSnapshot(detailResp);
                if (!detailSnap.isEmpty()) {
                    latestSnap = detailSnap;
                }
                String[] parsed = parseDetail(firstEffectsHtml(detailResp));
                if (parsed != null) {
                    if (!parsed[0].isEmpty()) {
                        ne.setFrom(parsed[0]);
                    }
                    if (!parsed[1].isEmpty()) {
                        ne.setSubject(parsed[1]);
                    }
                    if (!parsed[2].isEmpty()) {
                        ne.setText(parsed[2]);
                    }
                }
            } catch (RuntimeException ignored) {
                // 单封详情失败不中断列表其余邮件
            }
            if (ne.getText().isEmpty()) {
                ne.setText(ne.getSubject());
            }
            ne.setHtml("<html><body><pre>" + escapeHtml(ne.getText()) + "</pre></body></html>");
            out.add(ne);
        }
        return out;
    }
}