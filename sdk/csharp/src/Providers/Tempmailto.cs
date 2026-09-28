using System;
using System.Collections.Generic;
using System.Linq;
using System.Net;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// tempmailto.com 渠道（Laravel 会话）。
/// 与 Go 端 tempmailto.go 同构：GET / 首页由服务端渲染好一个当前邮箱
/// （#mainEmail value，同一会话内不变），建箱只需 GET + 提取即可；
/// 读信 POST /get_messages（表单 _token=&lt;CSRF&gt;&amp;captcha=），
/// 邮箱由会话 Cookie 承载，会话当前邮箱与请求邮箱不一致时 POST /change 拉回。
/// 会话凭据由本渠道以静态 Cookie 字典 + 显式 Cookie 头维护（无 Cookie 罐
/// 裸客户端，逐响应覆写），杜绝与其他会话罐渠道串池。
/// 邮箱约 10 分钟无活动过期。
/// </summary>
public static class Tempmailto
{
    private const string Base = "https://tempmailto.com";

    private static readonly Regex CsrfRe = new("<meta\\s+name=\"csrf-token\"\\s+content=\"([^\"]+)\"", RegexOptions.Compiled);
    private static readonly Regex MainEmailRe = new("(?is)id=\"mainEmail\"[^>]*\\bvalue=\"([^\"]+)\"", RegexOptions.Compiled);
    private static readonly Regex LocalPartRe = new("^[^@]+", RegexOptions.Compiled);

    private static readonly string[] DetailClasses =
        { "mail-body", "mail_content", "email-body", "content-body", "message-content", "mail-content" };

    // 渠道类内部静态 Cookie 字典：MainEmail 会话凭据（逐响应覆写），仅在生成后进入，避免残留
    private static readonly Dictionary<string, string> TempCookies = new(StringComparer.Ordinal);
    private static readonly object CookiesLock = new();

    /// <summary>取当前会话 Cookie 头（"k=v; k=v"）；无会话则为空串</summary>
    private static string CookieHeader()
    {
        lock (CookiesLock)
        {
            return string.Join("; ", TempCookies.Select(kv => $"{kv.Key}={kv.Value}"));
        }
    }

    /// <summary>用响应 Set-Cookie 覆写会话 Cookie 字典（逐响应覆写，仅保留带回的键）</summary>
    private static void ApplyCookies(HttpResult resp)
    {
        lock (CookiesLock)
        {
            foreach (var raw in resp.SetCookies)
            {
                var semi = raw.IndexOf(';');
                var kv = (semi < 0 ? raw : raw[..semi]).Trim();
                var eq = kv.IndexOf('=');
                if (eq <= 0) continue;
                var k = kv[..eq].Trim();
                var v = kv[(eq + 1)..].Trim();
                if (k.Length > 0) TempCookies[k] = v;
            }
        }
    }

    /// <summary>设置同站浏览器特征头（与前端同源调用一致；带 Cookie 头时追加）</summary>
    private static Dictionary<string, string> BrowserHeaders(string accept, bool withCookie = true)
    {
        var h = new Dictionary<string, string>
        {
            ["User-Agent"] = TempmailEEUtil.UA,
            ["Accept"] = accept,
            ["Accept-Language"] = "en-US,en;q=0.9",
            ["Origin"] = Base,
            ["Referer"] = Base + "/",
        };
        var cookie = CookieHeader();
        if (withCookie && cookie.Length > 0) h["Cookie"] = cookie;
        return h;
    }

    /// <summary>截断正文用于错误消息（保留状态码 + 简短回应）</summary>
    private static string Truncate(string s, int max = 200)
    {
        s = (s ?? "").Trim();
        return s.Length <= max ? s : s[..max];
    }

    /// <summary>创建 tempmailto.com 临时邮箱：GET 首页建立会话并提取服务端渲染的当前邮箱</summary>
    public static EmailInfo Generate()
    {
        var resp = Http.RawGet(Base, BrowserHeaders("text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8", withCookie: false));
        ApplyCookies(resp);
        if (resp.StatusCode < 200 || resp.StatusCode >= 300)
            throw new Exception($"tempmailto: 首页 http {resp.StatusCode}: {Truncate(resp.Body)}");

        // 首页由服务端渲染好一个当前邮箱（#mainEmail value），同一会话内不变
        var m = MainEmailRe.Match(resp.Body);
        if (!m.Success)
            throw new Exception("tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱");
        var email = m.Groups[1].Value.Trim();
        if (email.Length == 0)
            throw new Exception("tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱");

        // 邮箱约 10 分钟无活动过期；凭据本身无法驱动会话（会话由类内 Cookie 字典承载），
        // 注册表要求 token 非空，故约定 Token = 邮箱作防御性占位
        var createdAt = DateTimeOffset.UtcNow.AddMinutes(10).ToString("o");
        return new EmailInfo("tempmailto", email, email, createdAt: createdAt);
    }

    /// <summary>读取 tempmailto.com 当前邮箱的收件箱</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        if (email.Length == 0)
        throw new Exception("tempmailto: 邮箱为空，请重新 Generate");

        var data = FetchMessages();
        if (data.Mailbox.Length > 0 &&
            !string.Equals(data.Mailbox, email, StringComparison.OrdinalIgnoreCase))
        {
            // 会话当前邮箱与请求目标不一致：change 拉回（change 内部重新取 CSRF）
            var changed = Change(email);
            if (!string.Equals(changed, email, StringComparison.OrdinalIgnoreCase))
                throw new Exception($"tempmailto: 会话邮箱无法拉回请求邮箱（{changed} != {email}）");
            data = FetchMessages();
        }

        var result = new List<Email>();
        foreach (var msg in data.Messages)
        {
            var item = BuildItem(msg, email);
            if (item.Id.Length > 0) result.Add(item);
        }
        return result;
    }

    /// <summary>
    /// GET 首页提取 CSRF _token（读信/换箱共用）。
    /// 每次刷新都使用（由 Laravel 按会话轮换，总是取新的）。
    /// </summary>
    private static string GetCsrf()
    {
        var resp = Http.RawGet(Base, BrowserHeaders("text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"));
        ApplyCookies(resp);
        var m = CsrfRe.Match(resp.Body);
        if (!m.Success)
            throw new Exception("tempmailto: 首页未找到 csrf-token");
        return m.Groups[1].Value;
    }

    /// <summary>POST /change 换箱到目标邮箱；返回变更后的当前邮箱</summary>
    private static string Change(string email)
    {
        var csrf = GetCsrf();
        var name = LocalPartRe.Match(email).Value;
        if (name.Length == 0) name = "TmSdk";
        var domain = "tempmailto.com";
        var at = email.LastIndexOf('@');
        if (at >= 0 && at + 1 < email.Length) domain = email[(at + 1)..];

        var form = $"_token={WebUtility.UrlEncode(csrf)}&name={WebUtility.UrlEncode(name)}&domain={WebUtility.UrlEncode(domain)}";
        var headers = BrowserHeaders("application/json, text/plain, */*");
        headers["Content-Type"] = "application/x-www-form-urlencoded; charset=UTF-8";
        headers["X-Requested-With"] = "XMLHttpRequest";
        var resp = Http.RawPost(Base + "/change", form, "application/x-www-form-urlencoded; charset=UTF-8", headers);
        ApplyCookies(resp);

        var root = Json.Parse(resp.Body) as JsonObject;
        var mailbox = Json.Str(root, "mailbox").Trim();
        if (mailbox.Length == 0)
            throw new Exception($"tempmailto: change 响应异常: {Truncate(resp.Body)}");
        return mailbox;
    }

    /// <summary>POST /get_messages（表单 _token + captcha 留空）；返回 messages 列表</summary>
    private static (List<JsonObject> Messages, string Mailbox) FetchMessages()
    {
        var csrf = GetCsrf();
        var form = $"_token={WebUtility.UrlEncode(csrf)}&captcha=";
        var headers = BrowserHeaders("application/json, text/plain, */*");
        headers["Content-Type"] = "application/x-www-form-urlencoded; charset=UTF-8";
        headers["X-Requested-With"] = "XMLHttpRequest";
        var resp = Http.RawPost(Base + "/get_messages", form, "application/x-www-form-urlencoded; charset=UTF-8", headers);
        ApplyCookies(resp);
        if (resp.StatusCode < 200 || resp.StatusCode >= 300)
            throw new Exception($"tempmailto 读信: http {resp.StatusCode}: {Truncate(resp.Body)}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var mailbox = Json.Str(root, "mailbox").Trim();
        var messages = new List<JsonObject>();
        if (root?["messages"] is JsonArray arr)
            foreach (var m in arr)
                if (m is JsonObject mo) messages.Add(mo);
        return (messages, mailbox);
    }

    /// <summary>将 get_messages 列表元素归一为统一邮件结构（正文来自详情页 /view/{id}）</summary>
    private static Email BuildItem(JsonObject row, string email)
    {
        string GetStr(params string[] keys)
        {
            foreach (var k in keys)
            {
                var v = row[k];
                if (v is null) continue;
                var s = Json.NodeToString(v).Trim();
                if (s.Length > 0) return s;
            }
            return "";
        }

        var id = GetStr("id");
        if (id.Length == 0) return new Email(); // id 缺失视为无效行

        var date = GetStr("receivedAt", "received_at", "createdAt");
        if (date.Length == 0) date = DateTimeOffset.UtcNow.ToString("o");

        // 纯文本与 HTML：详情页优先，失败回退列表字段；text 空回退 body/text/snippet/preview，
        // 仍空再回退 subject；html 空时再用 text 合成 pre 包裹
        var text = "";
        var html = "";
        if (id.Length > 0)
        {
            html = ViewDetail(id);
            if (html.Length > 0) text = HtmlToText(html);
        }
        if (text.Length == 0)
        {
            text = GetStr("body", "text", "snippet", "preview");
            if (text.Length == 0) text = GetStr("subject");
        }
        if (html.Length == 0)
            html = $"<html><body><pre>{System.Net.WebUtility.HtmlEncode(text)}</pre></body></html>";

        var flat = new Dictionary<string, object?>
        {
            ["id"] = id,
            ["from"] = GetStr("from_email", "from", "from_name"),
            ["to"] = email,
            ["subject"] = GetStr("subject"),
            ["content"] = text,
            ["html"] = html,
            ["date"] = date,
            ["is_seen"] = IsSeen(row),
        };
        return Normalize.NormalizeEmail(flat, email);
    }

    /// <summary>is_seen 三态归一为布尔已读标记（数值非 0 / 字符串 "1"|"true" 忽略大小写）</summary>
    private static bool IsSeen(JsonObject row)
    {
        var v = row["is_seen"];
        if (v is null) return false;
        var s = Json.NodeToString(v).Trim();
        if (s.Length == 0) return false;
        if (double.TryParse(s, System.Globalization.NumberStyles.Any,
                System.Globalization.CultureInfo.InvariantCulture, out var n))
            return n != 0;
        return s == "1" || string.Equals(s, "true", StringComparison.OrdinalIgnoreCase);
    }

    /// <summary>GET /view/{id} 提取正文 HTML（同 Cookie 会话）；失败返回空串，不中断列表</summary>
    private static string ViewDetail(string id)
    {
        try
        {
            var resp = Http.RawGet(Base + "/view/" + Uri.EscapeDataString(id),
                BrowserHeaders("text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"));
            ApplyCookies(resp);
            if (resp.StatusCode < 200 || resp.StatusCode >= 300) return "";
            var page = resp.Body;

            // class 候选依次尝试，全失败回退 <main>/<article> 区块
            foreach (var cls in DetailClasses)
            {
                var re = new Regex("(?is)<[^>]+class=\"[^\"]*\\b" + Regex.Escape(cls) + "\\b[^\"]*\"[^>]*>([\\s\\S]*?)</(?:div|section|article)>");
                var m = re.Match(page);
                if (m.Success && m.Groups[1].Value.Trim().Length > 0)
                    return m.Groups[1].Value.Trim();
            }
            var m2 = Regex.Match(page, "(?is)<(main|article)[^>]*>([\\s\\S]*?)</\\1>");
            if (m2.Success && m2.Groups[2].Value.Trim().Length > 0)
                return m2.Groups[2].Value.Trim();
        }
        catch { /* 详情偶发网络/解析异常不阻塞列表 */ }
        return "";
    }

    /// <summary>HTML 转纯文本：去 script/style/标签 + 实体反转义 + 空白压缩</summary>
    private static string HtmlToText(string src)
    {
        var s = Regex.Replace(src, "(?is)<(script|style)[\\s\\S]*?</\\1>", " ");
        s = Regex.Replace(s, "(?s)<[^>]+>", " ");
        s = System.Net.WebUtility.HtmlDecode(s);
        return string.Join(" ", s.Split((char[]?)null, StringSplitOptions.RemoveEmptyEntries));
    }
}