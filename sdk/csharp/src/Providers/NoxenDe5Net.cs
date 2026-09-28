using System;
using System.Collections.Generic;
using System.Globalization;
using System.Linq;
using System.Text;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// NoxenDe5Net 渠道（UniMail-Bot 公共实例 tempmail.noxen.de5.net，guest/123456）。
/// 登录 POST /api/login {"username":"guest","password":"123456"} → Set-Cookie 提取
/// iding-session=&lt;JWT&gt;；建箱 GET /api/generate 带 Cookie → {"email","expires"(毫秒)}；
/// 读信 GET /api/emails?mailbox=&lt;urlenc&gt;&amp;limit=20 取列表，每封 GET /api/email/{id}
/// 详情（content/html_content/to_addrs/r2_bucket/r2_object_key/download），
/// 正文优先走 download 端点拉原始 EML 本地解析 MIME（multipart 递归 + base64 /
/// quoted-printable 解码），不可得时以验证码置顶 + preview 合成占位正文。
/// token 格式："noxen-de5-net|" + URL编码(cookie) + "|base=https://tempmail.noxen.de5.net"。
/// 会话以显式 Cookie 头逐请求携带（无 Cookie 罐模式），避免与会话罐残留串池。
/// </summary>
public static class NoxenDe5Net
{
    private const string Base = "https://tempmail.noxen.de5.net";
    private const string User = "guest";
    private const string Pass = "123456";
    private const string TokenPrefix = "noxen-de5-net|";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    /// <summary>登录取得会话 Cookie（iding-session=JWT，分号截断、前缀匹配）</summary>
    private static string SessionCookie()
    {
        var body = Json.Serialize(new Dictionary<string, string>
        {
            ["username"] = User,
            ["password"] = Pass,
        });
        var headers = new Dictionary<string, string>
        {
            ["Content-Type"] = "application/json",
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        var resp = Http.RawPost(Base + "/api/login", body, "application/json", headers);
        if (!resp.Ok)
            throw new Exception($"noxen-de5-net login: http {resp.StatusCode}");
        var root = Json.Parse(resp.Body) as JsonObject;
        if (!((root?["success"] as JsonValue)?.GetValue<bool>() ?? false))
            throw new Exception("noxen-de5-net login: 登录失败");
        foreach (var sc in resp.SetCookies)
        {
            var kv = sc[..(sc.IndexOf(';') > 0 ? sc.IndexOf(';') : sc.Length)];
            if (kv.StartsWith("iding-session=", StringComparison.Ordinal))
                return kv;
        }
        throw new Exception("noxen-de5-net login: 未下发会话 Cookie");
    }

    /// <summary>校验会话 Cookie 是否仍有效（GET /api/session → {"authenticated":true}）</summary>
    private static bool CookieStillValid(string cookie)
    {
        try
        {
            var headers = new Dictionary<string, string>
            {
                ["Accept"] = "application/json",
                ["User-Agent"] = Ua,
                ["Cookie"] = cookie,
            };
            var resp = Http.RawGet(Base + "/api/session", headers);
            var root = Json.Parse(resp.Body) as JsonObject;
            return resp.Ok && ((root?["authenticated"] as JsonValue)?.GetValue<bool>() ?? false);
        }
        catch { return false; }
    }

    /// <summary>创建临时邮箱：登录 → GET /api/generate 带 Cookie 建箱，expires（毫秒）转 ISO 串</summary>
    public static EmailInfo Generate()
    {
        var session = SessionCookie();

        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
            ["Cookie"] = session,
        };
        var resp = Http.RawGet(Base + "/api/generate", headers);
        if (!resp.Ok)
            throw new Exception($"noxen-de5-net generate: http {resp.StatusCode}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var email = Json.Str(root, "email").Trim();
        if (email.Length == 0)
            throw new Exception("noxen-de5-net generate: 响应缺少 email");

        // 凭据串：前缀 + URL 编码的会话 Cookie + |base=<基址>
        var token = TokenPrefix + Uri.EscapeDataString(session) + "|base=" + Base;
        object? createdAt = null;
        if (root?["expires"] is JsonValue ev && ev.TryGetValue<long>(out var ep) && ep > 0)
            createdAt = DateTimeOffset.FromUnixTimeMilliseconds(ep).ToString("o", CultureInfo.InvariantCulture);
        return new EmailInfo("noxen-de5-net", email, token, createdAt: createdAt);
    }

    /// <summary>
    /// 读取收件箱：先 GET /api/session 校验，未通过重登录；
    /// 列表→逐封详情→download 原始 EML 本地解析；解析失败或 download 空时正文为
    /// 验证码置顶 + preview 合成占位。401 报「会话失效或非本会话邮箱」。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        token = (token ?? "").Trim();
        if (!token.StartsWith(TokenPrefix, StringComparison.Ordinal))
            throw new Exception("noxen-de5-net: token 格式错误");
        var cookie = Uri.UnescapeDataString(token[TokenPrefix.Length..]);
        cookie = cookie.TrimEndSuffix("|base=" + Base);
        if (cookie.Length == 0)
            throw new Exception("noxen-de5-net: token 解码失败");

        if (!CookieStillValid(cookie))
            cookie = SessionCookie(); // 会话过期自动重登录

        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
            ["Cookie"] = cookie,
        };
        var resp = Http.RawGet(
            Base + "/api/emails?mailbox=" + Uri.EscapeDataString(email) + "&limit=20",
            headers);
        if (resp.StatusCode == 401)
            throw new Exception("noxen-de5-net 读信: http 401（会话失效或非本会话邮箱）");
        if (!resp.Ok)
            throw new Exception($"noxen-de5-net 读信: http {resp.StatusCode}");

        var arr = Json.Parse(resp.Body) as JsonArray;
        var result = new List<Email>();
        if (arr is null) return result;
        foreach (var m in arr)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            var id = FieldStr(raw, "id");
            raw["from"] = raw.TryGetValue("sender", out var s) ? s : null;
            raw["to"] = email;
            raw["date"] = raw.TryGetValue("received_at", out var ra) ? ra : null;
            raw["text"] = raw.TryGetValue("preview", out var pv) ? pv : null;
            raw["isRead"] = raw.TryGetValue("is_read", out var ir) ? ir : null;

            var full = false;
            if (id.Length > 0 && id != "0")
            {
                // 详情：取 download 定位拉原始 EML → 本地拆分 text/html；失败时合成占位
                var detail = FetchDetail(cookie, id);
                if (detail is not null)
                {
                    raw["content"] = Json.Str(detail, "content");
                    raw["html_content"] = Json.Str(detail, "html_content");
                    raw["to_addrs"] = Json.Str(detail, "to_addrs");
                    raw["r2_bucket"] = Json.Str(detail, "r2_bucket");
                    raw["r2_object_key"] = Json.Str(detail, "r2_object_key");
                    var download = Json.Str(detail, "download").Trim();
                    if (download.Length > 0)
                    {
                        var eml = FetchEml(cookie, download);
                        if (eml.Length > 0)
                        {
                            var (text, html) = ParseEml(eml);
                            if (text.Length > 0 || html.Length > 0)
                            {
                                raw["text"] = text;
                                raw["html_content"] = html;
                                full = true;
                            }
                        }
                    }
                }
                if (!full)
                    raw["text"] = ComposePlaceholder(mo);
            }
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }

    /// <summary>详情字段优先从原始 map 取标量字符串（数值转十进制），缺失返回空串</summary>
    private static string FieldStr(IDictionary<string, object?> m, string key)
    {
        if (!m.TryGetValue(key, out var v) || v is null) return "";
        switch (v)
        {
            case string s: return s.Trim();
            case double d: return ((long)d).ToString(CultureInfo.InvariantCulture);
            case long l: return l.ToString(CultureInfo.InvariantCulture);
            default: return "";
        }
    }

    /// <summary>拉取单封邮件详情（GET /api/email/{id}，带会话 Cookie）</summary>
    private static JsonObject? FetchDetail(string cookie, string id)
    {
        try
        {
            var headers = new Dictionary<string, string>
            {
                ["Accept"] = "application/json",
                ["User-Agent"] = Ua,
                ["Cookie"] = cookie,
            };
            var resp = Http.RawGet(Base + "/api/email/" + Uri.EscapeDataString(id), headers);
            if (!resp.Ok) return null;
            return Json.Parse(resp.Body) as JsonObject;
        }
        catch { return null; }
    }

    /// <summary>拉取原始 EML 报文（download 相对路径补基址，Accept: message/rfc822,*/*）</summary>
    private static string FetchEml(string cookie, string dlPath)
    {
        try
        {
            var u = dlPath;
            if (!u.StartsWith("http://", StringComparison.Ordinal) &&
                !u.StartsWith("https://", StringComparison.Ordinal))
                u = Base + u;
            var headers = new Dictionary<string, string>
            {
                ["Accept"] = "message/rfc822, */*",
                ["User-Agent"] = Ua,
                ["Cookie"] = cookie,
            };
            var resp = Http.RawGet(u, headers);
            if (!resp.Ok) return "";
            return resp.Body;
        }
        catch { return ""; }
    }

    /// <summary>全文不可得时合成占位正文：验证码置顶（非空）+ "\n\n" + preview</summary>
    private static string ComposePlaceholder(JsonObject m)
    {
        var code = Json.Str(m, "verification_code").Trim();
        var preview = Json.Str(m, "preview").Trim();
        var parts = new List<string>(2);
        if (code.Length > 0) parts.Add("验证码: " + code);
        if (preview.Length > 0) parts.Add(preview);
        return string.Join("\n\n", parts);
    }

    private static readonly Regex BoundaryRe =
        new("(?i)boundary=\"?([^\";\\s]+)\"?", RegexOptions.Compiled);

    /// <summary>从原始 Content-Type 头值提取 boundary（区分大小写，与上游一致）</summary>
    private static string BoundaryOf(string ctRaw)
    {
        var m = BoundaryRe.Match(ctRaw);
        return m.Success ? m.Groups[1].Value : "";
    }

    /// <summary>
    /// 将原始报文切分为首部 map 与正文块（已做 CRLF→LF 归一化）。
    /// 折行续行（空白开头）拼接到上一条；跳过 mbox "From " 首行。
    /// </summary>
    private static (Dictionary<string, string> Headers, string Body) SplitEml(string payload, int offset)
    {
        var lines = payload.Split('\n');
        var headers = new Dictionary<string, string>(StringComparer.OrdinalIgnoreCase);
        var i = offset;
        if (i < lines.Length && lines[i].StartsWith("From ", StringComparison.Ordinal)) i++;
        var curKey = "";
        for (; i < lines.Length; i++)
        {
            var line = lines[i];
            if (line.Length == 0)
            {
                i++;
                break;
            }
            if ((line[0] == ' ' || line[0] == '\t') && curKey.Length > 0)
            {
                headers[curKey] += " " + line.Trim();
                continue;
            }
            var k = line.IndexOf(':');
            if (k > 0)
            {
                curKey = line[..k].Trim().ToLowerInvariant();
                headers[curKey] = line[(k + 1)..].Trim();
            }
        }
        if (i > lines.Length) i = lines.Length;
        return (headers, string.Join("\n", lines.Skip(i)));
    }

    /// <summary>按 boundary 切出各 part（TrimPrefix "\n"、TrimSuffix "--\n"/"--"，空白段丢弃）</summary>
    private static List<string> SplitMultipart(string body, string boundary)
    {
        var parts = new List<string>();
        foreach (var seg in body.Split("--" + boundary))
        {
            var s = seg;
            if (s.StartsWith("\n", StringComparison.Ordinal)) s = s[1..];
            if (s.EndsWith("--\n", StringComparison.Ordinal)) s = s[..^3];
            else if (s.EndsWith("--", StringComparison.Ordinal)) s = s[..^2];
            if (s.Trim().Length > 0) parts.Add(s);
        }
        return parts;
    }

    /// <summary>解析 EML 原始报文 → (纯文本正文, HTML 正文)</summary>
    private static (string Text, string Html) ParseEml(string raw)
    {
        var payload = raw.Replace("\r\n", "\n").Replace("\r", "");
        var (topHeaders, topBody) = SplitEml(payload, 0);
        return ParseEntity(topHeaders, topBody);
    }

    /// <summary>
    /// 递归解析单个 MIME 实体：multipart/* 按 boundary 递归拆分，
    /// message/rfc822 整段递归，rfc822-headers 跳过；text 槽与 html 槽各取首个非空命中。
    /// </summary>
    private static (string Text, string Html) ParseEntity(
        IDictionary<string, string> headers, string body)
    {
        var ctRaw = headers.TryGetValue("content-type", out var ct0) ? ct0 : "";
        var ct = ctRaw.ToLowerInvariant();
        var cte = headers.TryGetValue("content-transfer-encoding", out var cte0)
            ? cte0.ToLowerInvariant() : "";

        // 单体：text/html 或 text/plain（含无 Content-Type 时按纯文本处理）
        if (!ct.StartsWith("multipart/", StringComparison.Ordinal))
        {
            var decoded = DecodePart(body, cte);
            return ct.Contains("text/html") ? ("", decoded) : (decoded, "");
        }

        // 复合：递归拆分，text 槽与 html 槽各自取第一个非空命中
        var text = "";
        var html = "";
        var boundary = BoundaryOf(ctRaw);
        if (boundary.Length > 0)
        {
            foreach (var part in SplitMultipart(body, boundary))
            {
                var (ph, pb) = SplitEml("#participant\n" + part, 1);
                var pct = ph.TryGetValue("content-type", out var pct0)
                    ? pct0.ToLowerInvariant() : "";
                if (pct.StartsWith("multipart/", StringComparison.Ordinal))
                {
                    var (t, h) = ParseEntity(ph, pb);
                    if (text.Length == 0) text = t;
                    if (html.Length == 0) html = h;
                }
                else if (pct.StartsWith("message/rfc822", StringComparison.Ordinal))
                {
                    // 转发的原始邮件整体作为 part：递归整封解析
                    var (nh, nb) = SplitEml(pb, 0);
                    var (t, h) = ParseEntity(nh, nb);
                    if (text.Length == 0) text = t;
                    if (html.Length == 0) html = h;
                }
                else if (pct.Contains("rfc822-headers"))
                {
                    // 纯头部 part 跳过，正文在后续 part 中抓取
                    continue;
                }
                else
                {
                    var (t, h) = ParseEntity(ph, pb);
                    if (text.Length == 0) text = t;
                    if (html.Length == 0) html = h;
                }
                if (text.Length > 0 && html.Length > 0) break;
            }
        }
        // 无 HTML 命中时从整体原文兜底抓取 HTML 片段（与上游 guessHtmlFromRaw 同构）
        if (html.Length == 0) html = GuessHtml(body);
        return (text, html);
    }

    /// <summary>
    /// 按 Content-Transfer-Encoding 解码 part 内容：base64（去空白后解码）、
    /// quoted-printable（软换行 + =XX hex）、其余原样。不做 GBK 转码（与 Go 一致）。
    /// </summary>
    private static string DecodePart(string data, string cte)
    {
        switch (cte.Trim().ToLowerInvariant())
        {
            case "base64":
                var joined = new string(data.Where(c =>
                    c != '\n' && c != '\r' && c != '\t' && c != ' ').ToArray());
                try
                {
                    return Encoding.UTF8.GetString(Convert.FromBase64String(joined)).Trim();
                }
                catch { return data; }
            case "quoted-printable":
                try { return DecodeQuotedPrintable(data).Trim(); }
                catch { return data; }
            default:
                // 7bit/8bit/binary：原样返回
                return data.Trim();
        }
    }

    /// <summary>quoted-printable 解码：行尾 '=' 软换行拼接、=XX 还原为字节，按 UTF-8 输出</summary>
    private static string DecodeQuotedPrintable(string data)
    {
        var soft = Regex.Replace(data, "=\r?\n", "");
        var bytes = Encoding.Latin1.GetBytes(soft);
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
        return Encoding.UTF8.GetString(outBytes.ToArray());
    }

    /// <summary>从整体原文中抓取 <html>…</html> 片段（大小写不敏感，无则 <!doctype html）</summary>
    private static string GuessHtml(string body)
    {
        if (body.Length == 0) return "";
        var lower = body.ToLowerInvariant();
        var hs = lower.IndexOf("<html", StringComparison.Ordinal);
        if (hs == -1) hs = lower.IndexOf("<!doctype html", StringComparison.Ordinal);
        if (hs == -1) return "";
        var he = lower.LastIndexOf("</html>", StringComparison.Ordinal);
        if (he == -1 || he < hs) return "";
        return body[hs..(he + 7)];
    }
}

/// <summary>字符串扩展：移除末尾后缀（存在时）</summary>
internal static class NoxenDe5NetStringExt
{
    public static string TrimEndSuffix(this string s, string suffix)
        => s.EndsWith(suffix, StringComparison.Ordinal) ? s[..^suffix.Length] : s;
}