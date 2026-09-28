using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// 30minemail 渠道（30minemail.com）。
/// 建箱 GET /?generate 返回完整 HTML 页面（含 &lt;地址&gt;@30minemail.com），
/// 从页面回溯解析本地名（空白/&gt;/引号前），本地名 &lt;8 报错；token 复用完整地址。
/// 读信 GET /messages.php?email=&lt;完整地址&gt;&amp;_=&lt;unix毫秒&gt;，
/// 响应 {"ok":true,"expired":false,"count","emails":[{id,from,to,subject,date,html}]}。
/// </summary>
public static class Email30min
{
    private const string BaseUrl = "https://30minemail.com";
    private const string Domain = "30minemail.com";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    /// <summary>创建 30minemail.com 临时邮箱：GET /?generate 解析 HTML 中的地址，token 复用完整地址</summary>
    public static EmailInfo Generate()
    {
        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
            ["User-Agent"] = Ua,
        };
        var resp = Http.Get(BaseUrl + "/?generate", headers);
        resp.EnsureSuccess();

        var page = resp.Body;
        var idx = page.IndexOf("@" + Domain, StringComparison.Ordinal);
        if (idx < 0) throw new Exception("30minemail: 创建页面未找到邮箱地址");

        // 向前查找本地名起点：空白或 > 之后
        var start = idx;
        while (start > 0)
        {
            var c = page[start - 1];
            if (c == ' ' || c == '\n' || c == '\t' || c == '>' || c == '"') break;
            start--;
        }
        var local = page[start..idx].Trim();
        if (local.Length < 8)
            throw new Exception($"30minemail: 创建页面解析地址异常: {page.Substring(start, idx - start + Domain.Length + 1)}");
        var addr = local + "@" + Domain;
        return new EmailInfo("30minemail", addr, addr);
    }

    /// <summary>读取收件箱：GET /messages.php?email=&amp;_=&lt;unix毫秒&gt;（模拟官方轮询参数）</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        if (email.Length == 0) throw new Exception("30minemail: 邮箱地址为空");

        var nowMillis = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        var resp = Http.Get(
            $"{BaseUrl}/messages.php?email={Uri.EscapeDataString(email)}&_={nowMillis}",
            headers);
        if (!resp.Ok)
            throw new Exception($"30minemail: 读取收件箱失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        if (root is null) throw new Exception("30minemail: 解析收件箱响应失败");
        var ok = (root["ok"] as JsonValue)?.GetValue<bool>() ?? false;
        var expired = (root["expired"] as JsonValue)?.GetValue<bool>() ?? false;
        if (!ok || expired)
            throw new Exception($"30minemail: 收件箱不可用或已过期: {resp.Body.Trim()}");

        var result = new List<Email>();
        var mails = root["emails"] as JsonArray;
        if (mails is null) return result;
        foreach (var m in mails)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            // 列表元素无 to 字段时注入收件人地址
            raw["to"] = email;
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}