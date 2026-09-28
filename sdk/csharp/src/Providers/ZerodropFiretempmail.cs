using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Zerodrop 渠道（zerodrop.dev）：无认证 REST。
/// 建箱无需请求，本地生成 "sdk"+8 位随机名，地址为 &lt;名&gt;@zerodrop-sandbox.online；
/// 读信 GET /api/inbox/{name}?source=sdk，响应 {"emails":[...],"count":N}。
/// 平台邮件对象只有 id/from/to/subject/receivedAt/raw/otp/magicLink，正文仅存在于
/// raw（完整 MIME 原文，头部与 body 以 \r\n\r\n 空行分隔），须从 raw 中剥离头部
/// 提取纯文本 body 填入 text，text/html 互转交由 Normalize 兜底。
/// </summary>
public static class Zerodrop
{
    private const string BaseUrl = "https://zerodrop.dev";
    private const string Domain = "zerodrop-sandbox.online";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private const string LocalChars = "abcdefghijklmnopqrstuvwxyz0123456789";

    private static string LocalName()
    {
        var sb = new System.Text.StringBuilder("sdk");
        for (var i = 0; i < 8; i++) sb.Append(LocalChars[Random.Shared.Next(LocalChars.Length)]);
        return sb.ToString();
    }

    /// <summary>从 raw（完整 MIME 原文）提取纯文本正文：定位首个空行后的部分即 body</summary>
    private static string RawBody(string raw)
    {
        var idx = raw.IndexOf("\r\n\r\n", StringComparison.Ordinal);
        if (idx >= 0) return raw[(idx + 4)..];
        idx = raw.IndexOf("\n\n", StringComparison.Ordinal);
        if (idx >= 0) return raw[(idx + 2)..];
        return "";
    }

    /// <summary>创建临时邮箱：本地生成随机名，token 复用完整地址以便收件箱回查</summary>
    public static EmailInfo Generate()
    {
        var email = LocalName() + "@" + Domain;
        return new EmailInfo("zerodrop", email, email);
    }

    /// <summary>读取收件箱：GET /api/inbox/{name}?source=sdk</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        var parts = email.Split('@', 2);
        if (parts.Length != 2 || parts[1] != Domain)
            throw new Exception($"zerodrop 读信: 非 {Domain} 域邮箱地址");

        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        var resp = Http.Get($"{BaseUrl}/api/inbox/{Uri.EscapeDataString(parts[0])}?source=sdk", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var msgs = root?["emails"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            // 平台响应无 to 字段，收件人固定为当前邮箱
            raw["to"] = email;
            // 正文仅存在于 raw（完整 MIME 原文），无 text/html 字段：提取纯文本 body 作 text
            var rawMime = ProviderPick.Str(raw, "raw");
            if (rawMime.Length > 0)
            {
                var body = RawBody(rawMime);
                if (body.Length > 0) raw["text"] = body;
            }
            // from/subject 原字段名与 Normalize 候选一致；date 用 receivedAt（Normalize 支持）
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}

/// <summary>
/// FireTempMail 渠道（firetempmail.com）：无认证 REST。
/// 建箱无需请求，本地生成 随机词+0-999@&lt;域&gt;（域池：offrework.click /
/// service-today.click / jobsdeforyou.sa.com）；
/// 读信 GET https://mail.firetempmail.com/mail/get?address=&lt;URL 编码&gt;，
/// 必须携带 Origin: https://firetempmail.com（否则 403）。
/// 响应 {"status":"ok","code":200,"msg":"...","stats":{},"mails":[...]}，
/// 邮件字段以 sender/subject/date/recipient/suffix + content-html/content-text/content-plain 多候选归一。
/// </summary>
public static class Firetempmail
{
    private const string ApiBase = "https://mail.firetempmail.com";
    private const string Origin = "https://firetempmail.com";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private static readonly string[] Domains = { "offrework.click", "service-today.click", "jobsdeforyou.sa.com" };
    private const string WordChars = "abcdefghijklmnopqrstuvwxyz";

    private static string LocalPart()
    {
        // 本地生成 随机小写单词 + 0-999（与官网 faker unique 词 + 1e3 取整一致）
        var n = 3 + Random.Shared.Next(4); // 3-6 位
        var sb = new System.Text.StringBuilder();
        for (var i = 0; i < n; i++) sb.Append(WordChars[Random.Shared.Next(WordChars.Length)]);
        return sb.ToString();
    }

    /// <summary>创建临时邮箱：建箱无需请求，本地生成 随机词+0-999@域名</summary>
    public static EmailInfo Generate()
    {
        var dom = Domains[Random.Shared.Next(Domains.Length)];
        var email = LocalPart() + Random.Shared.Next(1000) + "@" + dom;
        return new EmailInfo("firetempmail", email, email);
    }

    /// <summary>读取收件箱：GET mail.firetempmail.com/mail/get?address=，必带 Origin 头</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        if (email.Length == 0) throw new Exception("firetempmail 读信: 邮箱地址为空");

        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["Origin"] = Origin,
            ["Referer"] = Origin + "/",
            ["User-Agent"] = Ua,
        };
        var resp = Http.Get($"{ApiBase}/mail/get?address={Uri.EscapeDataString(email)}", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var status = Json.Str(root, "status").Trim();
        if (status.Length > 0 && status != "ok")
            throw new Exception($"firetempmail 读信: {Json.Str(root, "msg")}");

        var msgs = root?["mails"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            // 官网 JSON 无统一 to 字段，收件人固定为当前邮箱
            raw["to"] = email;
            // 正文多候选：content-html 优先，其次 content-text / content-plain / text / html
            var html = ProviderPick.Str(raw, "content-html", "html");
            var text = ProviderPick.Str(raw, "content-text", "content-plain", "text");
            if (html.Length > 0) raw["html"] = html;
            if (text.Length > 0) raw["text"] = text;
            var from = ProviderPick.Str(raw, "sender", "from", "from_address");
            var subject = ProviderPick.Str(raw, "subject", "title");
            var date = ProviderPick.Str(raw, "date", "received_at", "created_at");
            if (from.Length > 0) raw["from"] = from;
            if (subject.Length > 0) raw["subject"] = subject;
            if (date.Length > 0) raw["date"] = date;
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}