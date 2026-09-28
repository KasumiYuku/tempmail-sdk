using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Smails.dev 渠道。
/// POST /api/mailbox 建箱（空 JSON body，响应 address/token），
/// GET /api/mailbox/messages 读信（Authorization: Bearer，数组响应），
/// GET /api/mailbox/messages/{id} 取单封详情（Bearer）。
/// </summary>
public static class Smails
{
    private const string BaseUrl = "https://smails.dev";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private static Dictionary<string, string> ReqHeaders(string token = "")
    {
        var h = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        if (!string.IsNullOrEmpty(token)) h["Authorization"] = "Bearer " + token;
        return h;
    }

    /// <summary>创建 smails.dev 临时邮箱：POST /api/mailbox（空 JSON body）</summary>
    public static EmailInfo Generate()
    {
        var headers = ReqHeaders();
        headers["Content-Type"] = "application/json";
        var resp = Http.Post($"{BaseUrl}/api/mailbox", "{}", "application/json", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var address = Json.Str(root, "address").Trim();
        var token = Json.Str(root, "token").Trim();
        if (string.IsNullOrEmpty(address) || string.IsNullOrEmpty(token))
            throw new Exception("smails: 创建邮箱响应缺少必要字段");
        return new EmailInfo("smails", address, token);
    }

    /// <summary>
    /// 获取邮件列表：GET /api/mailbox/messages 取列表，对每个元素按 id 逐封
    /// GET /api/mailbox/messages/{id} 合并详情；详情失败时以列表摘要归一。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        token = (token ?? "").Trim();
        var resp = Http.Get($"{BaseUrl}/api/mailbox/messages", ReqHeaders(token));
        resp.EnsureSuccess();
        var rows = Json.Parse(resp.Body) as JsonArray;
        var result = new List<Email>();
        if (rows is null) return result;
        foreach (var m in rows)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            var id = ProviderPick.MessageID(raw);
            if (id.Length > 0)
            {
                try
                {
                    var dr = Http.Get($"{BaseUrl}/api/mailbox/messages/{Uri.EscapeDataString(id)}", ReqHeaders(token));
                    if (dr.Ok && Json.Parse(dr.Body) is JsonObject det)
                        foreach (var kv in Json.ToDict(det))
                            if (!raw.ContainsKey(kv.Key)) raw[kv.Key] = kv.Value;
                }
                catch { /* 详情失败时回退为列表摘要 */ }
            }
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}

/// <summary>
/// TempMail Portal 渠道（api.tempmailportal.com）。
/// POST /api/v2/inbox 建箱（空 JSON body，响应 address/token/private/expiresAt，token 为 p2 前缀），
/// GET /api/messages 读信（Authorization: Bearer），GET /api/messages/{id} 取单封详情（Bearer）。
/// </summary>
public static class Tempmailportal
{
    private const string BaseUrl = "https://api.tempmailportal.com";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private static Dictionary<string, string> ReqHeaders(string token = "")
    {
        var h = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        if (!string.IsNullOrEmpty(token)) h["Authorization"] = "Bearer " + token;
        return h;
    }

    /// <summary>创建 tempmailportal 临时邮箱：POST /api/v2/inbox（空 JSON body）</summary>
    public static EmailInfo Generate()
    {
        var headers = ReqHeaders();
        headers["Content-Type"] = "application/json";
        var resp = Http.Post($"{BaseUrl}/api/v2/inbox", "{}", "application/json", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var address = Json.Str(root, "address").Trim();
        var token = Json.Str(root, "token").Trim();
        if (string.IsNullOrEmpty(address) || string.IsNullOrEmpty(token))
            throw new Exception("tempmailportal: 创建邮箱响应缺少必要字段");
        return new EmailInfo("tempmailportal", address, token, createdAt: Json.Str(root, "expiresAt"));
    }

    /// <summary>
    /// 获取邮件列表：GET /api/messages 取列表，对每个元素按 id 逐封 GET /api/messages/{id}
    /// 合并详情；详情失败时以列表摘要归一。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        token = (token ?? "").Trim();
        var resp = Http.Get($"{BaseUrl}/api/messages", ReqHeaders(token));
        resp.EnsureSuccess();
        var rows = Json.Parse(resp.Body) as JsonArray;
        var result = new List<Email>();
        if (rows is null) return result;
        foreach (var m in rows)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            var id = ProviderPick.MessageID(raw);
            if (id.Length > 0)
            {
                try
                {
                    var dr = Http.Get($"{BaseUrl}/api/messages/{Uri.EscapeDataString(id)}", ReqHeaders(token));
                    if (dr.Ok && Json.Parse(dr.Body) is JsonObject det)
                        foreach (var kv in Json.ToDict(det))
                            if (!raw.ContainsKey(kv.Key)) raw[kv.Key] = kv.Value;
                }
                catch { /* 详情失败时回退为列表摘要 */ }
            }
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}

/// <summary>
/// HuskMail 渠道（huskmail.xyz，真实 API 域 api.huskmail.space）。
/// POST /api/v1/accounts 建箱（空 JSON body，响应 id/address/password/token/expiresAt，token 为 JWT），
/// GET /v1/messages 读信（Authorization: Bearer，响应 {"messages":[...]}），
/// GET /v1/messages/{id} 取单封详情（Bearer）。收信域固定为 @huskmail.xyz。
/// </summary>
public static class Huskmail
{
    private const string BaseUrl = "https://api.huskmail.space";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private static Dictionary<string, string> ReqHeaders(string token = "")
    {
        var h = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        if (!string.IsNullOrEmpty(token)) h["Authorization"] = "Bearer " + token;
        return h;
    }

    /// <summary>创建 huskmail（@huskmail.xyz）临时邮箱：POST /v1/accounts（空 JSON body）</summary>
    public static EmailInfo Generate()
    {
        var headers = ReqHeaders();
        headers["Content-Type"] = "application/json";
        var resp = Http.Post($"{BaseUrl}/v1/accounts", "{}", "application/json", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var address = Json.Str(root, "address").Trim();
        var token = Json.Str(root, "token").Trim();
        if (string.IsNullOrEmpty(address) || string.IsNullOrEmpty(token))
            throw new Exception("huskmail: 创建邮箱响应缺少必要字段");
        return new EmailInfo("huskmail", address, token, createdAt: Json.Str(root, "expiresAt"));
    }

    /// <summary>
    /// 获取邮件列表：GET /v1/messages 取列表（{"messages":[...]}），对每个元素按 id 逐封
    /// GET /v1/messages/{id} 合并详情；详情失败时以列表摘要归一。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        token = (token ?? "").Trim();
        var resp = Http.Get($"{BaseUrl}/v1/messages", ReqHeaders(token));
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var rows = root?["messages"] as JsonArray;
        var result = new List<Email>();
        if (rows is null) return result;
        foreach (var m in rows)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            var id = ProviderPick.MessageID(raw);
            if (id.Length > 0)
            {
                try
                {
                    var dr = Http.Get($"{BaseUrl}/v1/messages/{Uri.EscapeDataString(id)}", ReqHeaders(token));
                    if (dr.Ok && Json.Parse(dr.Body) is JsonObject det)
                        foreach (var kv in Json.ToDict(det))
                            if (!raw.ContainsKey(kv.Key)) raw[kv.Key] = kv.Value;
                }
                catch { /* 详情失败时回退为列表摘要 */ }
            }
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}