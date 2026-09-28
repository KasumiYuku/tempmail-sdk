using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Crazymailing 渠道（crazymailing.com，Next.js 全栈站点）。
/// 建箱 POST /api/mailbox（空 JSON body）→ {"mailbox":{"id","address","expiresAt"}}，token=id；
/// 读信 GET /api/messages?mailbox=&lt;urlenc 完整地址&gt; → {"messages":[...]}，
/// 每封 GET /api/message/{id}/body 拉正文 HTML（失败兜底），元素无 to 注入地址。
/// </summary>
public static class Crazymailing
{
    private const string BaseUrl = "https://crazymailing.com";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    /// <summary>设置 crazymailing API 请求通用头</summary>
    private static Dictionary<string, string> ApiHeaders()
    {
        return new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["Origin"] = BaseUrl,
            ["Referer"] = BaseUrl + "/",
            ["User-Agent"] = Ua,
        };
    }

    /// <summary>创建临时邮箱：POST /api/mailbox（空 JSON body，域由服务端统一分配）</summary>
    public static EmailInfo Generate()
    {
        var headers = ApiHeaders();
        headers["Content-Type"] = "application/json";
        var resp = Http.Post($"{BaseUrl}/api/mailbox", "{}", "application/json", headers);
        if (!resp.Ok)
            throw new Exception($"crazymailing: 创建邮箱失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var mailbox = root?["mailbox"] as JsonObject;
        var address = Json.Str(mailbox, "address").Trim();
        if (mailbox is null || address.Length == 0)
            throw new Exception($"crazymailing: 创建响应缺少 mailbox.address: {resp.Body.Trim()}");
        return new EmailInfo("crazymailing", address, Json.Str(mailbox, "id").Trim(),
            createdAt: Json.Str(mailbox, "expiresAt"));
    }

    /// <summary>
    /// 读取收件箱：GET /api/messages?mailbox=&lt;完整地址&gt;；
    /// 对每个元素逐封 GET /api/message/{id}/body 拉取正文（失败以列表摘要兜底，不阻断列表）。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        if (email.Length == 0) throw new Exception("crazymailing: 邮箱地址为空");

        var resp = Http.Get($"{BaseUrl}/api/messages?mailbox={Uri.EscapeDataString(email)}", ApiHeaders());
        if (!resp.Ok)
            throw new Exception($"crazymailing: 读取收件箱失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var msgs = root?["messages"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            // 注入收件人地址（to 字段缺失时保底）
            raw["to"] = email;
            // 列表元素为摘要，正文须逐封二拉
            var id = ProviderPick.MessageID(raw);
            if (id.Length > 0)
            {
                var html = GetBody(id);
                if (html.Length > 0) raw["html"] = html;
            }
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }

    /// <summary>拉取单封正文：GET /api/message/{id}/body，响应为完整 HTML 页面</summary>
    private static string GetBody(string id)
    {
        try
        {
            var headers = new Dictionary<string, string>
            {
                ["Accept"] = "text/html,application/xhtml+xml,*/*;q=0.8",
                ["Origin"] = BaseUrl,
                ["Referer"] = BaseUrl + "/",
                ["User-Agent"] = Ua,
            };
            var resp = Http.Get($"{BaseUrl}/api/message/{Uri.EscapeDataString(id)}/body", headers);
            if (!resp.Ok) return "";
            return resp.Body.Trim();
        }
        catch { return ""; }
    }
}