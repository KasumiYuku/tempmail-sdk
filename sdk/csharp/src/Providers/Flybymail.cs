using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Flybymail 渠道（flybymail.com）。
/// POST /api/recipients 建箱（空 JSON body，响应 id/email/createdAt/expiresAt，expiresAt 为毫秒时间戳，约 4 小时）；
/// GET /api/recipients/{email}/emails 读信（按邮箱地址、非 id 查询，响应 {"emails":[...]}）。
/// 信件字段：id/from/to/subject/body（纯文本）/htmlBody（HTML 正文）/preview/time/read。
/// </summary>
public static class Flybymail
{
    private const string BaseUrl = "https://flybymail.com";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    /// <summary>设置 flybymail 请求的通用 JSON 请求头</summary>
    private static Dictionary<string, string> JsonHeaders()
    {
        return new Dictionary<string, string>
        {
            ["Content-Type"] = "application/json",
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
    }

    /// <summary>创建 flybymail.com 临时邮箱：POST /api/recipients（空 JSON body）</summary>
    public static EmailInfo Generate()
    {
        var resp = Http.Post($"{BaseUrl}/api/recipients", "{}", "application/json", JsonHeaders());
        if (!resp.Ok)
            throw new Exception($"flybymail: 创建邮箱失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var id = Json.Str(root, "id").Trim();
        var email = Json.Str(root, "email").Trim();
        if (id.Length == 0 || email.Length == 0 || !email.Contains('@'))
            throw new Exception($"flybymail: 创建邮箱响应缺少必要字段: {resp.Body.Trim()}");

        // expiresAt 为毫秒时间戳：折为秒对齐其余端口径后按 C# EmailInfo 毫秒单位存储
        long? expiresAt = null;
        if (root?["expiresAt"] is JsonValue ev && ev.TryGetValue<long>(out var ep) && ep > 0)
            expiresAt = ep / 1000 * 1000;
        return new EmailInfo("flybymail", email, id, expiresAt);
    }

    /// <summary>获取邮件列表：GET /api/recipients/{email}/emails（按地址查询，直接多候选字段归一）</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        var address = (email ?? "").Trim();
        if (address.Length == 0 || !address.Contains('@'))
            throw new Exception("flybymail: 邮箱地址为空或格式错误");

        var resp = Http.Get($"{BaseUrl}/api/recipients/{Uri.EscapeDataString(address)}/emails", JsonHeaders());
        if (!resp.Ok)
            throw new Exception($"flybymail: 获取邮件列表失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var msgs = root?["emails"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var entries = new Dictionary<string, object?>
            {
                ["from"] = Json.Str(mo, "from"),
                ["to"] = Json.Str(mo, "to"),
                ["subject"] = Json.Str(mo, "subject"),
                ["text"] = Json.Str(mo, "body"),
                ["html"] = Json.Str(mo, "htmlBody"),
                ["time"] = mo["time"],
                ["read"] = mo["read"],
                ["attachments"] = mo["attachments"],
            };
            // id 为数字时转换为字符串（与发件页字段一致）
            entries["id"] = AnyString(mo, "id");
            // time 为毫秒时间戳：按 timestamp 候选归一
            entries["timestamp"] = mo["time"] ?? mo["date"];
            result.Add(Normalize.NormalizeEmail(entries, address));
        }
        return result;
    }

    /// <summary>从对象提取候选键的首个非空字符串（数值转十进制字符串）</summary>
    private static string AnyString(JsonObject m, string key)
    {
        if (m[key] is not JsonValue v) return "";
        if (v.TryGetValue<string>(out var s)) return s.Trim();
        if (v.TryGetValue<long>(out var l)) return l.ToString(System.Globalization.CultureInfo.InvariantCulture);
        if (v.TryGetValue<double>(out var d))
            return ((long)d).ToString(System.Globalization.CultureInfo.InvariantCulture);
        return "";
    }
}