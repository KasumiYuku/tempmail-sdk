using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Linshi.xyz 渠道。
/// 无建箱请求：本地随机 6 位 hex 前缀 + @linshi.xyz，token 复用完整地址。
/// 读信 GET /api/mails/{前缀}，元素 {headers:{from,to,subject,date},html}，
/// 归一化时把 headers 平铺顶层、无 to 注入地址；响应非数组整体报错。
/// </summary>
public static class LinshiXyz
{
    private const string BaseUrl = "https://linshi.xyz";
    private const string Domain = "linshi.xyz";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private const string HexChars = "0123456789abcdef";

    /// <summary>生成本地随机 6 位 hex 前缀（与官网 client 相同格式）</summary>
    private static string LocalName()
    {
        var sb = new System.Text.StringBuilder(6);
        for (var i = 0; i < 6; i++) sb.Append(HexChars[Random.Shared.Next(HexChars.Length)]);
        return sb.ToString();
    }

    /// <summary>创建 linshi.xyz 临时邮箱：无需建箱请求，token 复用完整地址</summary>
    public static EmailInfo Generate()
    {
        var addr = LocalName() + "@" + Domain;
        return new EmailInfo("linshi-xyz", addr, addr);
    }

    /// <summary>读取收件箱：GET /api/mails/{前缀}，响应为邮件对象数组（非数组整体报错）</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        if (email.Length == 0 || !email.Contains('@'))
            throw new Exception($"linshi-xyz: 邮箱地址无效: {email}");
        var local = email.Split('@', 2)[0];

        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        var resp = Http.Get($"{BaseUrl}/api/mails/{Uri.EscapeDataString(local)}", headers);
        if (!resp.Ok)
            throw new Exception($"linshi-xyz: 读取收件箱失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body);
        if (root is null || root is not JsonArray arr)
            throw new Exception("linshi-xyz: 解析收件箱响应失败（响应非数组骨架）");

        var result = new List<Email>();
        foreach (var m in arr)
        {
            if (m is not JsonObject mo) continue;
            // headers 对象平铺为顶层字段；嵌套对象无法被候选提取，故显式展开
            var raw = Json.ToDict(mo);
            if (mo["headers"] is JsonObject hd)
                foreach (var kv in Json.ToDict(hd))
                    raw[kv.Key] = kv.Value;
            // 注入收件人地址（headers.to 存在时 normalizeTo 会优先使用）
            raw["to"] = email;
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}