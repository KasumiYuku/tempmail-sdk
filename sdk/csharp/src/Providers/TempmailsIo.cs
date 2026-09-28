using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// tempmails.io 渠道：无认证 REST。
/// POST /api/temp-mail/generate 建箱（响应 success/data{email,token,expires_at}），
/// GET /api/temp-mail/inbox/{token} 读信（data.messages[] 含 from_email/text_body/html_body/attachments）。
/// 邮箱借用 uberip.com 等公共域（10 分钟自动过期）。
/// </summary>
public static class TempmailsIo
{
    private const string BaseUrl = "https://tempmails.io";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private static Dictionary<string, string> ReqHeaders()
    {
        return new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
    }

    /// <summary>创建 10 分钟临时邮箱：POST /api/temp-mail/generate（空 JSON body）</summary>
    public static EmailInfo Generate()
    {
        var headers = ReqHeaders();
        headers["Content-Type"] = "application/json";
        var resp = Http.Post($"{BaseUrl}/api/temp-mail/generate", "{}", "application/json", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var data = root?["data"] as JsonObject;
        if ((root?["success"] as JsonValue)?.GetValue<bool>() != true || data is null)
            throw new Exception("tempmails-io: 建箱响应格式无效");
        var email = Json.Str(data, "email").Trim();
        var token = Json.Str(data, "token").Trim();
        if (string.IsNullOrEmpty(email) || string.IsNullOrEmpty(token))
            throw new Exception("tempmails-io: 建箱响应缺少 email 或 token");
        return new EmailInfo("tempmails-io", email, token, createdAt: Json.Str(data, "expires_at"));
    }

    /// <summary>
    /// 读取收件箱。
    /// 必须先 POST /api/temp-mail/fetch-emails/{token} 触发平台对上游信箱
    /// （uberip.com 域，mail.tm 别名）的主动同步，其后 GET /api/temp-mail/inbox/{token}
    /// 才能读到新邮件。只轮询 inbox 会永远为空。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        token = (token ?? "").Trim();
        if (string.IsNullOrEmpty(token)) throw new Exception("tempmails-io: token 为空");

        // 1) 触发主动同步（失败不致命，仍尝试静态读）
        try
        {
            Http.Post($"{BaseUrl}/api/temp-mail/fetch-emails/{Uri.EscapeDataString(token)}", null, null, ReqHeaders());
        }
        catch { /* 同步失败不阻断静态读 */ }

        // 2) 读静态收件箱
        var resp = Http.Get($"{BaseUrl}/api/temp-mail/inbox/{Uri.EscapeDataString(token)}", ReqHeaders());
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var msgs = root?["data"]?["messages"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            // 直接映射字段；attachments 若存在则随原字典透传
            raw["from"] = Json.Str(mo, "from_email");
            raw["to"] = email;
            raw["text"] = Json.Str(mo, "text_body");
            raw["html"] = Json.Str(mo, "html_body");
            raw["date"] = Json.Str(mo, "received_at");
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}