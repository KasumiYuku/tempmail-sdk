using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Internxt 渠道实现（internxt.com/temporary-email，Next.js + OpenNext）。
/// 建箱 GET /api/temp-mail/create-email；读信 GET /api/temp-mail/get-inbox
/// （顶层数组）；详情 GET /api/temp-mail/get-message（messageId 参数）。
/// CSRF：首次 GET /temporary-email 夺取 XSRF-TOKEN（Set-Cookie），数据接口
/// 校验 csrf-token 头（值为罐中 XSRF-TOKEN，每个 API 响应都会刷新该 Cookie，
/// 故每次请求前重取）。建箱响应 {"address","token"}；token 存 JSON 快照。
/// </summary>
public static class Internxt
{
    private const string Site = "https://internxt.com";
    private const string Ref = Site + "/temporary-email";
    private const string ApiBase = Site + "/api/temp-mail";
    private const string UA =
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /// <summary>模块级自管 Cookie 罐（XSRF-TOKEN 每个 API 响应刷新）</summary>
    private static string _xsrf = "";
    private static string _secret = "";

    /// <summary>收下响应 Set-Cookie：XSRF-TOKEN/csrfSecret 同名覆写</summary>
    private static void MergeCookies(HttpResult resp)
    {
        foreach (var line in resp.SetCookies)
        {
            var kv = line.Split(';')[0].Trim();
            var eq = kv.IndexOf('=');
            if (eq <= 0) continue;
            var name = kv[..eq];
            var value = kv[(eq + 1)..];
            if (name == "XSRF-TOKEN") _xsrf = value;
            else if (name == "csrfSecret") _secret = value;
        }
    }

    /// <summary>组装 Cookie 请求头（有则携带）</summary>
    private static string CookieHeader()
    {
        var parts = new List<string>();
        if (_secret.Length > 0) parts.Add("csrfSecret=" + _secret);
        if (_xsrf.Length > 0) parts.Add("XSRF-TOKEN=" + _xsrf);
        return string.Join("; ", parts);
    }

    /// <summary>确保罐中持有 XSRF-TOKEN：无则 GET /temporary-email 夺取</summary>
    private static void PrepareXsrf()
    {
        if (_xsrf.Length > 0) return;
        var resp = Http.Get(Ref, new Dictionary<string, string>
        {
            ["User-Agent"] = UA,
            ["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            ["Accept-Language"] = "en-US,en;q=0.9",
        });
        resp.EnsureSuccess();
        MergeCookies(resp);
        if (_xsrf.Length == 0) throw new Exception("internxt: 未取得 XSRF-TOKEN Cookie");
    }

    /// <summary>带 CSRF 头请求 internxt 数据接口（GET），返回解析后的 JSON 节点</summary>
    private static JsonNode? ApiGet(string path, IEnumerable<KeyValuePair<string, string>>? query)
    {
        PrepareXsrf();
        var url = ApiBase + path;
        if (query is not null)
        {
            var qs = new List<string>();
            foreach (var kv in query)
                qs.Add(Uri.EscapeDataString(kv.Key) + "=" + Uri.EscapeDataString(kv.Value));
            if (qs.Count > 0) url += "?" + string.Join("&", qs);
        }
        var headers = new Dictionary<string, string>
        {
            ["User-Agent"] = UA,
            ["Accept"] = "application/json, text/plain, */*",
            ["Origin"] = Site,
            ["Referer"] = Ref,
            ["csrf-token"] = _xsrf,
        };
        var cookie = CookieHeader();
        if (cookie.Length > 0) headers["Cookie"] = cookie;

        var resp = Http.Get(url, headers);
        MergeCookies(resp);
        resp.EnsureSuccess();
        return Json.Parse(resp.Body);
    }

    /// <summary>
    /// 创建 internxt.com 临时邮箱
    /// 先确保罐中 XSRF-TOKEN，再 GET /api/temp-mail/create-email
    /// （带 csrf-token 头），响应 {"address","token"}。
    /// </summary>
    public static EmailInfo Generate()
    {
        var data = ApiGet("/create-email", null) as JsonObject
            ?? throw new Exception("internxt: 解析建箱响应失败");
        var address = Json.Str(data, "address").Trim();
        var apiToken = Json.Str(data, "token").Trim();
        if (address.Length == 0 || apiToken.Length == 0 || !address.Contains('@'))
            throw new Exception("internxt: 创建邮箱响应缺少必要字段（address/token）");
        var tokenJson = new JsonObject { ["address"] = address, ["token"] = apiToken }.ToJsonString();
        return new EmailInfo("internxt", address, tokenJson);
    }

    /// <summary>从列表/详情元素提取邮件 ID，候选字段 id/messageId/message_id</summary>
    private static string MessageIdOf(JsonObject m)
    {
        foreach (var key in new[] { "id", "messageId", "message_id" })
        {
            if (m.TryGetPropertyValue(key, out var v) && v is JsonValue jv
                && jv.TryGetValue<string>(out var s) && s.Trim().Length > 0)
                return s;
        }
        return "";
    }

    /// <summary>
    /// 获取 internxt.com 收件箱
    /// get-inbox 返回顶层数组（列表元素含 id/from/subject/date/seen 等）；
    /// 逐条 get-message 拉单封全文（响应为单封对象，含 html 渲染全文），
    /// 详情失败回退列表摘要。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        JsonObject sess;
        try
        {
            sess = Json.Parse(token ?? "") as JsonObject
                ?? throw new Exception("非对象");
        }
        catch (Exception e)
        {
            throw new Exception($"internxt: 会话凭据解析失败: {e.Message}");
        }
        var address = Json.Str(sess, "address").Trim();
        var apiToken = Json.Str(sess, "token").Trim();
        if (address.Length == 0 || apiToken.Length == 0)
            throw new Exception("internxt: 会话凭据缺少必要字段");
        if (address != email)
            throw new Exception("internxt: 会话邮箱与查询邮箱不匹配");

        var inbox = ApiGet("/get-inbox", new[]
        {
            new KeyValuePair<string, string>("email", address),
            new KeyValuePair<string, string>("token", apiToken),
        }) as JsonArray
            ?? throw new Exception("internxt: 解析收件箱响应失败");

        var result = new List<Email>();
        foreach (var item in inbox)
        {
            if (item is not JsonObject m)
                continue;
            var flat = Json.ToDict(m);
            var mid = MessageIdOf(m);
            if (mid.Length > 0)
            {
                try
                {
                    var detail = ApiGet("/get-message", new[]
                    {
                        new KeyValuePair<string, string>("email", address),
                        new KeyValuePair<string, string>("token", apiToken),
                        new KeyValuePair<string, string>("messageId", mid),
                    }) as JsonObject;
                    if (detail is not null)
                    {
                        /* 列表字段优先，详情仅补齐缺失字段 */
                        foreach (var kv in detail)
                        {
                            if (!flat.ContainsKey(kv.Key))
                                flat[kv.Key] = Json.ToRaw(kv.Value) ?? "";
                        }
                    }
                }
                catch
                {
                    /* 详情失败回退列表摘要 */
                }
            }
            result.Add(Normalize.NormalizeEmail(flat, email));
        }
        return result;
    }
}