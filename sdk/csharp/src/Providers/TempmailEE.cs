using System;
using System.Collections.Generic;
using System.Globalization;
using System.Linq;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// TemporálEE 渠道辅助（tempmail.ee Cookies 会话 + MIME 解析），仅供 TempmailEE 复用。
/// </summary>
internal static class TempmailEEUtil
{
    /// <summary>与 sec-ch-ua 品牌版本一致的固定 UA（Chrome 154 / Linux）</summary>
    public const string UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /// <summary>Token 前缀，用于识别本渠道会话凭据串</summary>
    public const string TokenPrefix = "tempmail-ee|";

    /// <summary>按候选键顺序提取字符串值（数值转十进制字符串，与 Go getStrFromMap 一致）</summary>
    public static string GetStr(IDictionary<string, object?> m, params string[] keys)
    {
        foreach (var key in keys)
        {
            if (!m.TryGetValue(key, out var v) || v is null) continue;
            switch (v)
            {
                case string s:
                    var t = s.Trim();
                    if (t.Length > 0) return t;
                    break;
                case double d:
                    return ((long)d).ToString(CultureInfo.InvariantCulture);
                case long l:
                    return l.ToString(CultureInfo.InvariantCulture);
                case bool b:
                    return b ? "true" : "false";
            }
        }
        return "";
    }

    /// <summary>从 map 提取布尔值（兼容 bool / int / long 数值）</summary>
    public static bool GetBool(IDictionary<string, object?> m, params string[] keys)
    {
        foreach (var key in keys)
        {
            if (!m.TryGetValue(key, out var v) || v is null) continue;
            switch (v)
            {
                case bool b: return b;
                case double d: return d != 0;
                case long l: return l != 0;
            }
        }
        return false;
    }

    /// <summary>从响应 Set-Cookie 列表中提取 temp_email/temp_mail_session 键值对</summary>
    public static (string Email, string Session) CookieFromResponse(HttpResult resp)
    {
        var email = "";
        var session = "";
        foreach (var raw in resp.SetCookies)
        {
            var semi = raw.IndexOf(';');
            var kv = (semi < 0 ? raw : raw[..semi]).Trim();
            if (kv.StartsWith("temp_email=", StringComparison.Ordinal))
                email = kv["temp_email=".Length..];
            else if (kv.StartsWith("temp_mail_session=", StringComparison.Ordinal))
                session = kv["temp_mail_session=".Length..];
        }
        return (email, session);
    }

    /// <summary>由邮箱与 temp_mail_session 组装渠道内部凭据串</summary>
    public static string TokenBuild(string email, string session) =>
        TokenPrefix + "temp_email=" + email + "; temp_mail_session=" + session;

    /// <summary>
    /// 解析读信凭据，返回 (cookie, ok)。会话绑定邮箱：以请求邮箱为准重拼 cookie，
    /// 防止凭据与邮箱错配。
    /// </summary>
    public static (string Cookie, bool Ok) ParseToken(string token, string email)
    {
        if (!token.StartsWith(TokenPrefix, StringComparison.Ordinal)) return ("", false);
        var cred = token[TokenPrefix.Length..];
        var session = "";
        foreach (var partRaw in cred.Split(';'))
        {
            var kv = partRaw.Trim();
            if (kv.StartsWith("temp_mail_session=", StringComparison.Ordinal))
                session = kv["temp_mail_session=".Length..];
        }
        if (session.Length == 0) return ("", false);
        return ("temp_email=" + email + "; temp_mail_session=" + session, true);
    }

    /// <summary>返回 HTML 转纯文本（去标签、反转义、压缩空白）</summary>
    public static string HtmlToText(string html) => Normalize.HtmlToText(html);

    public static string TextToHtml(string text) =>
        $"<html><body><pre>{System.Net.WebUtility.HtmlEncode(text)}</pre></body></html>";

    /// <summary>按 Content-Transfer-Encoding 解码 part 内容</summary>
    public static string DecodePart(string data, string cte)
    {
        data = data.Trim();
        switch (cte.ToLowerInvariant())
        {
            case "base64":
                var joined = new string(data.Where(c => c != '\n' && c != '\r' && c != '\t' && c != ' ').ToArray());
                try
                {
                    return System.Text.Encoding.UTF8.GetString(Convert.FromBase64String(joined)).Trim();
                }
                catch { /* 解码失败回退原文 */ }
                break;
            case "quoted-printable":
                try { return DecodeQuotedPrintable(data).Trim(); }
                catch { /* 解码失败回退原文 */ }
                break;
        }
        return data;
    }

    /// <summary>quoted-printable 解码：去软换行、=XX 转义还原，按 UTF-8 输出</summary>
    private static string DecodeQuotedPrintable(string data)
    {
        var soft = System.Text.RegularExpressions.Regex.Replace(data, "=\r?\n", "");
        var bytes = System.Text.Encoding.Latin1.GetBytes(soft);
        var outBytes = new List<byte>(bytes.Length);
        for (var i = 0; i < bytes.Length; i++)
        {
            if (bytes[i] == (byte)'=' && i + 2 < bytes.Length)
            {
                var hex = new string(new[] { (char)bytes[i + 1], (char)bytes[i + 2] });
                if (byte.TryParse(hex, NumberStyles.HexNumber, CultureInfo.InvariantCulture, out var b))
                {
                    outBytes.Add(b);
                    i += 2;
                    continue;
                }
            }
            outBytes.Add(bytes[i]);
        }
        return System.Text.Encoding.UTF8.GetString(outBytes.ToArray());
    }

    /// <summary>
    /// 解析详情 content：平台已做 HTML 实体转义（= 写成 &amp;#61; 等），先反转义；
    /// content 是 MIME multipart 原文（含顶部边界装饰头），按首个边界行切块，
    /// 各 part 依据 Content-Transfer-Encoding 做 base64 / quoted-printable 解码，
    /// text part 入纯文本、html part 入 HTML。两者之一为空时互为兜底合成。
    /// </summary>
    public static (string Text, string Html) ParseContent(string raw)
    {
        var payload = System.Net.WebUtility.HtmlDecode(raw);
        payload = payload.Replace("&#61;", "=").Replace("&#43;", "+").Replace("&#47;", "/");
        payload = payload.Replace("\r\n", "\n");

        var lines = payload.Split('\n');
        var boundary = BoundaryOf(lines);
        var text = "";
        var html = "";
        if (boundary.Length > 0) (text, html) = Parts(lines, boundary);
        if (boundary.Length == 0 || (text.Length == 0 && html.Length == 0))
        {
            // 无有效 multipart 结构（如邮件 body 仅单个 part）：整个 content 去壳后作为正文
            (text, html) = Singleton(payload);
        }
        if (text.Length == 0 && html.Length > 0) text = HtmlToText(html);
        if (html.Length == 0 && text.Length > 0) html = TextToHtml(text);
        return (text, html);
    }

    /// <summary>扫描首块寻找边界行（默认 multipart 边界行处于块首）</summary>
    private static string BoundaryOf(string[] lines)
    {
        for (var i = 0; i < lines.Length && i < 120; i++)
        {
            if (lines[i].StartsWith("--", StringComparison.Ordinal) && lines[i].Length > 2)
                return lines[i].TrimEnd('\r').Remove(0, 2);
        }
        return "";
    }

    /// <summary>按 boundary 拆分 multipart 并解码归并 text/html 两个 part</summary>
    private static (string Text, string Html) Parts(string[] lines, string boundary)
    {
        var text = "";
        var html = "";
        for (var i = 0; i < lines.Length; i++)
        {
            if (!lines[i].StartsWith("--" + boundary, StringComparison.Ordinal)) continue;
            if (lines[i].StartsWith("--" + boundary + "--", StringComparison.Ordinal)) break;
            var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
            var j = i + 1;
            // part 头部：直到首个空行（RFC 空行在 Content-* 头之后）
            while (j < lines.Length && lines[j].Length > 0 && !lines[j].StartsWith("--" + boundary, StringComparison.Ordinal))
            {
                var idx = lines[j].IndexOf(':');
                if (idx > 0)
                    headers[lines[j][..idx].Trim().ToLowerInvariant()] = lines[j][(idx + 1)..].Trim();
                j++;
            }
            if (j < lines.Length && lines[j].Length == 0) j++;
            // part 正文：到下一个边界行为止，内部空行属于正文内容
            var body = new List<string>();
            while (j < lines.Length && !lines[j].StartsWith("--" + boundary, StringComparison.Ordinal))
            {
                body.Add(lines[j]);
                j++;
            }
            (text, html) = MergePart(body, headers, text, html);
            i = j - 1;
        }
        return (text, html);
    }

    /// <summary>单个 part 的解码归并：按 content-type 归类，缺省回退 text 槽</summary>
    private static (string Text, string Html) MergePart(List<string> body,
        Dictionary<string, string> headers, string text, string html)
    {
        var ct = headers.GetValueOrDefault("content-type", "").ToLowerInvariant();
        var semi = ct.IndexOf(';');
        if (semi > 0) ct = ct[..semi].Trim();
        var cte = headers.GetValueOrDefault("content-transfer-encoding", "").ToLowerInvariant();
        var content = string.Join("\n", body);
        if (ct.Contains("text/plain"))
        {
            if (text.Length == 0) text = DecodePart(content, cte);
            return (text, html);
        }
        if (ct.Contains("text/html"))
        {
            if (html.Length == 0) html = DecodePart(content, cte);
            return (text, html);
        }
        if (text.Length == 0) text = DecodePart(content, cte);
        return (text, html);
    }

    /// <summary>
    /// 单 part（无边界或拆不出内容）降级解析：
    /// 依次按「头部区隔（首个空行之后）→ 整体」取内容，
    /// 并通过关键字识别 Content-Transfer-Encoding 做解码。
    /// </summary>
    private static (string Text, string Html) Singleton(string payload)
    {
        var body = payload.Trim();
        if (!body.Contains('\n')) return (body, "");
        var lines = payload.Split('\n');
        // 头部以 RFC822 形式出现（首行含冒号键值）时，正文从首个空行后开始
        var start = 0;
        var found = false;
        for (var i = 0; i < lines.Length; i++)
        {
            var ln = lines[i];
            if (ln.Trim().Length == 0)
            {
                start = i + 1;
                found = true;
                break;
            }
            if (i > 40 || (i >= 3 && !ln.Contains(':'))) break;
        }
        if (found) body = string.Join("\n", lines.Skip(start));
        var lower = payload.ToLowerInvariant();
        var cte = lower.Contains("base64") ? "base64"
            : lower.Contains("quoted-printable") ? "quoted-printable" : "";
        return (DecodePart(body, cte), "");
    }
}

/// <summary>
/// tempmail.ee 渠道。
/// 出口风控实证：/api/mails 的 403 是「会话 Cookie 绑定校验」而非 TLS 指纹——
/// change（换箱）返回的 Set-Cookie 中 temp_mail_session 与 temp_email 共同构成
/// 读信凭据，同一会话内带齐两者即 200。因此 change → 提取 Set-Cookie → 读信
/// 须在同一事务内完成。会话凭据由本渠道以显式 Cookie 请求头逐请求携带
/// （无 Cookie 罐模式），杜绝与会话罐残留串池。
/// 已通行配方：POST /api/mailbox/change 必须带 sec-ch-ua 三件套
/// （否则 403 Browser request required）；UA 使用固定 Chrome 154。
/// 读信列表只含元数据（id/fromAddress/toAddress/subject/createdAt/isRead），
/// 正文须逐封 GET /api/mails/{id}，其 content 字段为 multipart（MIME 原文，
/// HTML 实体已转义），由本渠道解码拆出 text / html。
/// </summary>
public static class TempmailEE
{
    private const string Base = "https://tempmail.ee";

    /// <summary>建箱提交的浏览器指纹（与官方前端一致）</summary>
    private static Dictionary<string, object?> BrowserIntegrity() => new()
    {
        ["webdriver"] = false, ["languagesMissing"] = false, ["languageMissing"] = false,
        ["pluginsMissing"] = false, ["pluginsUndefined"] = false, ["outerSizeMissing"] = false,
        ["innerSizeMissing"] = false, ["screenMissing"] = false, ["screenDepthMissing"] = false,
        ["timezoneMissing"] = false, ["timezoneOffsetMissing"] = false,
        ["userAgentDataPresent"] = true, ["userAgentMissing"] = false, ["platformClass"] = "Linux",
        ["mobile"] = false, ["collectionFailed"] = false,
    };

    /// <summary>设置浏览器特征安全头（同站 fetch 全套）。withSecCH 控制是否携带 sec-ch-ua 三件套（change 必须）</summary>
    private static Dictionary<string, string> BrowserHeaders(bool withSecCH)
    {
        var h = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["Content-Type"] = "application/json",
            ["X-Requested-With"] = "XMLHttpRequest",
            ["Origin"] = Base,
            ["Referer"] = Base + "/",
            ["Sec-Fetch-Site"] = "same-origin",
            ["Sec-Fetch-Mode"] = "cors",
            ["Sec-Fetch-Dest"] = "empty",
            ["Accept-Language"] = "en-US,en;q=0.9",
            ["User-Agent"] = TempmailEEUtil.UA,
        };
        if (withSecCH)
        {
            h["Sec-Ch-Ua"] = "\"Chromium\";v=\"154\", \"Google Chrome\";v=\"154\", \"Not.A/Brand\";v=\"99\"";
            h["Sec-Ch-Ua-Mobile"] = "?0";
            h["Sec-Ch-Ua-Platform"] = "\"Linux\"";
        }
        return h;
    }

    /// <summary>
    /// 创建 tempmail.ee 临时邮箱。
    /// 事务会话：GET / 面熟 → POST /api/mailbox/change（sec-ch-ua 全套头）换新邮箱
    /// → 提取 Set-Cookie 中的 temp_mail_session，随 EmailInfo token 透传给读信。
    /// </summary>
    public static EmailInfo Generate()
    {
        // 步骤 1：GET / 建立 Cookie 会话（面熟首访，无 Cookie 罐模式下显式丢弃响应）
        var bootHeaders = new Dictionary<string, string>
        {
            ["User-Agent"] = TempmailEEUtil.UA,
            ["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
        };
        Http.RawGet(Base, bootHeaders);

        // 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua）
        var body = Json.Serialize(new Dictionary<string, object?>
        {
            ["turnstileToken"] = null,
            ["browserIntegrity"] = BrowserIntegrity(),
        });
        var chResp = Http.RawPost(Base + "/api/mailbox/change", body, "application/json", BrowserHeaders(true));
        if (chResp.StatusCode < 200 || chResp.StatusCode >= 300)
            throw new Exception($"tempmail-ee: 建箱失败 HTTP {chResp.StatusCode}: {chResp.Body.Trim()}");

        var chg = Json.Parse(chResp.Body) as JsonObject;
        var success = (chg?["success"] as JsonValue)?.GetValue<bool>() ?? false;
        var newEmail = Json.Str(chg, "newEmail").Trim();
        if (!success || newEmail.Length == 0)
            throw new Exception($"tempmail-ee: 建箱失败: {chResp.Body.Trim()}（status {chResp.StatusCode}）");

        // 步骤 3：从 Set-Cookie 接管会话凭据，供读信校验使用
        var (cookieEmail, session) = TempmailEEUtil.CookieFromResponse(chResp);
        var email = newEmail;
        if (cookieEmail.Length > 0 && cookieEmail != email) email = cookieEmail;
        if (session.Length == 0)
            throw new Exception("tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信");

        var expires = Json.Str(chg, "expiresAt").Trim();
        if (expires.Length == 0)
            expires = DateTimeOffset.UtcNow.AddMinutes(60).ToString("o", CultureInfo.InvariantCulture);

        return new EmailInfo("tempmail-ee", email, TempmailEEUtil.TokenBuild(email, session), createdAt: expires);
    }

    /// <summary>
    /// 读取 tempmail.ee 收件箱。
    /// 凭据来自 Generate 时从 change 响应提取的 temp_mail_session，
    /// 逐请求以显式 Cookie 头携带（temp_email + temp_mail_session 缺一不可）。
    /// 列表只含元数据，正文逐封 GET /api/mails/{id} 解码 multipart content。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        if (email.Length == 0) throw new Exception("tempmail-ee: 邮箱为空");
        token = (token ?? "").Trim();
        if (token.Length == 0) throw new Exception("tempmail-ee: token 为空");

        // 解析会话凭据；无有效 temp_mail_session 无法通过平台会话校验
        var (cookie, ok) = TempmailEEUtil.ParseToken(token, email);
        if (!ok) throw new Exception("tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱");

        var body = Json.Serialize(new Dictionary<string, object?> { ["email"] = email });
        var postHeaders = BrowserHeaders(true);
        postHeaders["Cookie"] = cookie;
        var resp = Http.RawPost(Base + "/api/mails", body, "application/json", postHeaders);
        if (resp.StatusCode == 403)
            throw new Exception("tempmail-ee inbox: http 403（会话 Cookie 校验失败，邮箱可能已过期，请重新 Generate）");
        if (resp.StatusCode < 200 || resp.StatusCode >= 300)
            throw new Exception($"tempmail-ee inbox: http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var mails = root?["mails"] as JsonArray;
        var result = new List<Email>();
        if (mails is null) return result;

        var getHeaders = BrowserHeaders(false);
        getHeaders.Remove("Content-Type");
        getHeaders["Cookie"] = cookie;

        foreach (var m in mails)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            var id = TempmailEEUtil.GetStr(raw, "id");
            if (id.Length == 0) continue; // id 缺失视为无效行

            var to = TempmailEEUtil.GetStr(raw, "toAddress", "to");
            if (to.Length == 0) to = email;
            var created = TempmailEEUtil.GetStr(raw, "createdAt", "date", "receivedAt");
            if (created.Length == 0) created = DateTimeOffset.UtcNow.ToString("o", CultureInfo.InvariantCulture);

            var item = new Email
            {
                Id = id,
                From = TempmailEEUtil.GetStr(raw, "fromAddress", "from", "sender"),
                To = to,
                Subject = TempmailEEUtil.GetStr(raw, "subject"),
                Date = created,
                IsRead = TempmailEEUtil.GetBool(raw, "isRead"),
            };

            try
            {
                // 单封详情拉取失败不阻塞列表其余邮件
                var dr = Http.RawGet(Base + "/api/mails/" + Uri.EscapeDataString(id), getHeaders);
                if (dr.StatusCode >= 200 && dr.StatusCode < 300 && Json.Parse(dr.Body) is JsonObject detail)
                {
                    var content = TempmailEEUtil.GetStr(Json.ToDict(detail), "content");
                    if (content.Length == 0)
                        content = TempmailEEUtil.GetStr(Json.ToDict(detail), "text", "body", "html");
                    if (content.Length > 0)
                    {
                        var (text, html) = TempmailEEUtil.ParseContent(content);
                        if (item.Text.Length == 0) item.Text = text;
                        if (item.Html.Length == 0) item.Html = html;
                    }
                    var dRaw = Json.ToDict(detail);
                    if (item.From.Length == 0) item.From = TempmailEEUtil.GetStr(dRaw, "fromAddress", "from");
                    if (item.Subject.Length == 0) item.Subject = TempmailEEUtil.GetStr(dRaw, "subject");
                }
            }
            catch { /* 详情偶发 4xx/网络抖动不阻塞列表 */ }

            result.Add(item);
        }
        return result;
    }
}