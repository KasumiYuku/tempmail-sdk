using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// ClawdEmail 渠道（clawdemail.com，API 域 api.clawdemail.com）。
/// POST /register 建箱（JSON body {"name":""}，响应 success/email/token），
/// GET /inbox?limit=50 读信列表（Header Authorization: Bearer &lt;token&gt;，
/// 响应 success/email/count/unread/emails[]，success 非真报 error），
/// 每封 GET /email/{id} 取单封详情（Bearer）；详情含 email 嵌套对象时提升嵌套对象
/// （from_addr/subject/body_text/received_at/read），缺字段才合并；详情失败以列表摘要兜底。
/// </summary>
public static class Clawdemail
{
    private const string BaseUrl = "https://api.clawdemail.com";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    /// <summary>设置 clawdemail 请求的通用请求头（token 非空时附 Bearer）</summary>
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

    /// <summary>创建 clawdemail.com 临时邮箱：POST /register（body {"name":""}，空名服务端随机生成）</summary>
    public static EmailInfo Generate()
    {
        var headers = AuthHeaders("");
        headers["Content-Type"] = "application/json";
        var payload = Json.Serialize(new Dictionary<string, string> { ["name"] = "" });
        var resp = Http.Post($"{BaseUrl}/register", payload, "application/json", headers);
        if (!resp.Ok)
            throw new Exception($"clawdemail: 创建邮箱失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var email = Json.Str(root, "email").Trim();
        var token = Json.Str(root, "token").Trim();
        if (email.Length == 0 || token.Length == 0 || !email.Contains('@'))
            throw new Exception($"clawdemail: 创建邮箱响应缺少必要字段: {resp.Body.Trim()}");
        return new EmailInfo("clawdemail", email, token);
    }

    /// <summary>
    /// 获取邮件列表：GET /inbox?limit=50 取列表，对每个元素按 id 逐封 GET /email/{id}
    /// 合并详情（缺字段才覆盖）；详情失败时以列表摘要归一。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        token = (token ?? "").Trim();
        var resp = Http.Get($"{BaseUrl}/inbox?limit=50", AuthHeaders(token));
        if (!resp.Ok)
            throw new Exception($"clawdemail: 获取邮件列表失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        if (root is null || !((root["success"] as JsonValue)?.GetValue<bool>() ?? false))
            throw new Exception($"clawdemail: 读取收件箱失败: {Json.Str(root, "error")}");

        var result = new List<Email>();
        var msgs = root["emails"] as JsonArray;
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
                    var dr = Http.Get($"{BaseUrl}/email/{Uri.EscapeDataString(id)}", AuthHeaders(token));
                    if (dr.Ok && Json.Parse(dr.Body) is JsonObject detail)
                    {
                        // 详情为 {success,email:{...},code,links} 时提升嵌套 email 对象
                        var merged = detail["email"] is JsonObject nested
                            ? Json.ToDict(nested)
                            : Json.ToDict(detail);
                        foreach (var kv in merged)
                            if (!raw.ContainsKey(kv.Key)) raw[kv.Key] = kv.Value;
                    }
                }
                catch { /* 详情失败时回退为列表摘要 */ }
            }
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}