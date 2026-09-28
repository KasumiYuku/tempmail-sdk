using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// TempMail100 渠道（tempmail100.com）。
/// POST /init 建箱（空 body，响应 code/data.token（JWT），无 Cookie），
/// POST /web/generate 建随机地址（Header Authorization: &lt;裸 token 无 Bearer&gt;，响应 code/data.address），
/// GET /web/emails 读信列表（Authorization 裸 token，响应 code/data.list[]/data.total）。
/// 列表元素 {uuid, subject, fromAddress, toAddress, fromName, content, timestamp, read}，
/// 其中 content 平台恒为空，如实留空（详情端点不可得，为平台限制）。
/// </summary>
public static class Tempmail100
{
    private const string BaseUrl = "https://tempmail100.com";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    /// <summary>设置 tempmail100 请求的通用请求头（前端使用 Authorization: <token> 不带 Bearer）</summary>
    private static Dictionary<string, string> AuthHeaders(string token)
    {
        var h = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        if (token.Length > 0) h["Authorization"] = token;
        return h;
    }

    /// <summary>创建 tempmail100.com 临时邮箱：POST /init 取 JWT，再 POST /web/generate 创建地址</summary>
    public static EmailInfo Generate()
    {
        // 第一步：初始化取得 token
        var initResp = Http.Post($"{BaseUrl}/init", null, null, AuthHeaders(""));
        if (!initResp.Ok)
            throw new Exception($"tempmail100: 初始化失败 http {initResp.StatusCode}: {initResp.Body.Trim()}");
        var init = Json.Parse(initResp.Body) as JsonObject;
        var token = Json.Str(init?["data"] as JsonObject, "token").Trim();
        if (init is null || (Json.Str(init, "code") != "0" && Json.NodeToString(init["code"]) != "0") ||
            token.Length == 0)
            throw new Exception($"tempmail100: 初始化响应异常: {initResp.Body.Trim()}");

        // 第二步：创建随机地址
        var genResp = Http.Post($"{BaseUrl}/web/generate", null, null, AuthHeaders(token));
        if (!genResp.Ok)
            throw new Exception($"tempmail100: 创建地址失败 http {genResp.StatusCode}: {genResp.Body.Trim()}");
        var gen = Json.Parse(genResp.Body) as JsonObject;
        var address = Json.Str(gen?["data"] as JsonObject, "address").Trim();
        if (gen is null || (Json.Str(gen, "code") != "0" && Json.NodeToString(gen["code"]) != "0") ||
            address.Length == 0 || !address.Contains('@'))
            throw new Exception($"tempmail100: 创建地址响应异常: {genResp.Body.Trim()}");
        return new EmailInfo("tempmail100", address, token);
    }

    /// <summary>
    /// 获取邮件列表：GET /web/emails（Authorization 头为裸 token）返回 code/data.list/data.total；
    /// code!=0 报错，list null 返回空。列表元素 content 恒为空（平台限制），如实留空。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        token = (token ?? "").Trim();
        var resp = Http.Get($"{BaseUrl}/web/emails", AuthHeaders(token));
        if (!resp.Ok)
            throw new Exception($"tempmail100: 获取邮件列表失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        if (root is null) throw new Exception("tempmail100: 解析邮件列表失败");
        if (Json.Str(root, "code") != "0" && Json.NodeToString(root["code"]) != "0")
            throw new Exception($"tempmail100: 获取邮件列表响应异常: {Json.Str(root, "message")}");

        var result = new List<Email>();
        var data = root["data"] as JsonObject;
        var list = data?["list"] as JsonArray;
        if (list is null) return result; // list null 返回空
        foreach (var m in list)
        {
            if (m is not JsonObject mo) continue;
            result.Add(NormalizeItem(mo, email));
        }
        return result;
    }

    /// <summary>
    /// 将 /web/emails 列表元素归一为统一邮件结构。
    /// fromName 非空且与 fromAddress 不同且含 @ 时组合为 "Name <address>"；
    /// read 兼容 bool / 数字 / 字符串 "true"|"1"。
    /// </summary>
    private static Email NormalizeItem(JsonObject item, string email)
    {
        var fromName = Json.Str(item, "fromName").Trim();
        var fromAddress = Json.Str(item, "fromAddress").Trim();
        if (fromName.Length > 0 &&
            !string.Equals(fromName, fromAddress, StringComparison.OrdinalIgnoreCase) &&
            fromAddress.Contains('@'))
            fromAddress = $"{fromName} <{fromAddress}>";

        var flat = new Dictionary<string, object?>
        {
            ["id"] = Json.Str(item, "uuid"),
            ["from"] = fromAddress,
            ["to"] = Json.Str(item, "toAddress"),
            ["subject"] = Json.Str(item, "subject"),
            ["content"] = Json.Str(item, "content"),
            ["timestamp"] = item["timestamp"],
            ["isRead"] = ReadValue(item),
        };
        return Normalize.NormalizeEmail(flat, email);
    }

    /// <summary>将 read 字段归一为布尔已读标记，兼容 bool / 数字(0|1) / 字符串("true"|"1")</summary>
    private static bool ReadValue(JsonObject item)
    {
        if (item["read"] is not JsonValue v) return false;
        if (v.TryGetValue<bool>(out var b)) return b;
        if (v.TryGetValue<long>(out var l)) return l != 0;
        if (v.TryGetValue<double>(out var d)) return d != 0;
        if (v.TryGetValue<string>(out var s))
        {
            var t = s.Trim();
            return string.Equals(t, "true", StringComparison.OrdinalIgnoreCase) || t == "1";
        }
        return false;
    }
}