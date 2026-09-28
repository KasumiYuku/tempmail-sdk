using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// ShitPost.email 渠道（shitpost.email 公共实例）：无认证 REST。
/// POST /api/create 建箱（username/domain/ttl → email/token/type/expires），
/// GET /api/inbox?email=&token= 读信（messages[] 含 from/subject/text/html/date）。
/// 域名池：shitpost.email / letsfuckingpiss.party。
/// </summary>
public static class ShitpostEmail
{
    private const string BaseUrl = "https://shitpost.email";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private static readonly string[] Domains = { "shitpost.email", "letsfuckingpiss.party" };

    private const string LocalChars = "abcdefghijklmnopqrstuvwxyz0123456789";

    private static string LocalPart()
    {
        var sb = new System.Text.StringBuilder("sdk");
        for (var i = 0; i < 10; i++) sb.Append(LocalChars[Random.Shared.Next(LocalChars.Length)]);
        return sb.ToString();
    }

    private static Dictionary<string, string> ReqHeaders() => new()
    {
        ["Accept"] = "application/json",
        ["User-Agent"] = Ua,
    };

    /// <summary>创建临时邮箱：POST /api/create body {username,domain,ttl}</summary>
    public static EmailInfo Generate()
    {
        var dom = Domains[Random.Shared.Next(Domains.Length)];
        var payload = Json.Serialize(new Dictionary<string, object?>
        {
            ["username"] = LocalPart(),
            ["domain"] = dom,
            ["ttl"] = 3600,
        });
        var headers = ReqHeaders();
        headers["Content-Type"] = "application/json";
        var resp = Http.Post($"{BaseUrl}/api/create", payload, "application/json", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var email = Json.Str(root, "email").Trim();
        var token = Json.Str(root, "token").Trim();
        if (string.IsNullOrEmpty(email) || string.IsNullOrEmpty(token))
            throw new Exception("shitpost-email: 建箱响应缺少 email 或 token");
        return new EmailInfo("shitpost-email", email, token, createdAt: Json.Str(root, "expires"));
    }

    /// <summary>读取收件箱：GET /api/inbox?email=&token=</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        var qs = $"?email={Uri.EscapeDataString(email)}&token={Uri.EscapeDataString((token ?? "").Trim())}";
        var resp = Http.Get($"{BaseUrl}/api/inbox{qs}", ReqHeaders());
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var msgs = root?["messages"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            raw["from"] = Json.Str(mo, "from");
            raw["to"] = email;
            raw["text"] = Json.Str(mo, "text");
            raw["html"] = Json.Str(mo, "html");
            raw["date"] = Json.Str(mo, "date");
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}