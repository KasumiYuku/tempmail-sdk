using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// NowtempMail 渠道（nowtempmail.com）。
/// POST /mailbox 建箱（空 body + Content-Type: application/json，响应 token（JWT）/mailbox），
/// GET /messages 读信列表（Header Authorization: Bearer &lt;token&gt;，响应 {"messages":[...]}），
/// 每封 GET /message/{id}（Bearer）取详情，缺字段才覆盖合并；详情失败以列表摘要兜底。
/// </summary>
public static class Nowtempmail
{
    private const string BaseUrl = "https://nowtempmail.com";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    /// <summary>设置 nowtempmail 请求的通用请求头（token 非空时附 Bearer）</summary>
    private static Dictionary<string, string> AuthHeaders(string token)
    {
        var h = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        if (token.Length > 0) h["Authorization"] = "Bearer " + token;
        return h;
    }

    /// <summary>创建 nowtempmail.com 临时邮箱：POST /mailbox（空 body + Content-Type: application/json）</summary>
    public static EmailInfo Generate()
    {
        var headers = AuthHeaders("");
        headers["Content-Type"] = "application/json";
        var resp = Http.Post($"{BaseUrl}/mailbox", null, null, headers);
        if (!resp.Ok)
            throw new Exception($"nowtempmail: 创建邮箱失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var token = Json.Str(root, "token").Trim();
        var mailbox = Json.Str(root, "mailbox").Trim();
        if (token.Length == 0 || mailbox.Length == 0 || !mailbox.Contains('@'))
            throw new Exception($"nowtempmail: 创建邮箱响应缺少必要字段: {resp.Body.Trim()}");
        return new EmailInfo("nowtempmail", mailbox, token);
    }

    /// <summary>
    /// 获取邮件列表：GET /messages 取列表，对每个元素按 id 逐封 GET /message/{id}
    /// 合并详情（缺字段才覆盖）；详情失败时以列表摘要归一。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        token = (token ?? "").Trim();
        var resp = Http.Get($"{BaseUrl}/messages", AuthHeaders(token));
        if (!resp.Ok)
            throw new Exception($"nowtempmail: 获取邮件列表失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var msgs = root?["messages"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            var id = ProviderPick.MessageID(raw);
            if (id.Length > 0)
            {
                try
                {
                    var dr = Http.Get($"{BaseUrl}/message/{Uri.EscapeDataString(id)}", AuthHeaders(token));
                    if (dr.Ok && Json.Parse(dr.Body) is JsonObject detail)
                        foreach (var kv in Json.ToDict(detail))
                            if (!raw.ContainsKey(kv.Key)) raw[kv.Key] = kv.Value;
                }
                catch { /* 详情失败时回退为列表摘要 */ }
            }
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}