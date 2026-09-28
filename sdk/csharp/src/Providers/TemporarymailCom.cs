using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Temporarymail.com 渠道：无认证 REST（key 为空即随机建箱）。
/// 建箱 GET /api/?action=requestEmailAccess&amp;key=&amp;value=random，
/// 响应 {"address","secretKey"}，secretKey 用于后续 checkInbox。
/// 读信 GET /api/?action=checkInbox&amp;value=&lt;secretKey&gt;，响应双形态：
/// 空箱为 []，有信时为 map[id]→对象；逐封 POST /api/?action=getEmail&amp;value=&lt;id&gt;
/// 覆盖真实 subject/from（失败兜底）；再 GET /view/?i=&lt;id&gt;&amp;width=800 取全文剥标签。
/// 403/404 换备用 UA 重试一次；429 报错带 Retry-After。
/// </summary>
public static class TemporarymailCom
{
    private const string BaseUrl = "https://temporarymail.com";
    private const string AltUa = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36";

    private static string Ua()
    {
        var cfg = Config.Get();
        if (cfg.Headers is not null && cfg.Headers.TryGetValue("User-Agent", out var ua) && !string.IsNullOrEmpty(ua))
            return ua;
        return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";
    }

    /// <summary>铺设 /api/ 请求的浏览器形态头部集（规避平台对脚本请求判别尺度收紧）</summary>
    private static Dictionary<string, string> ApiHeaders(string ua, Dictionary<string, string>? extra = null)
    {
        var h = new Dictionary<string, string>
        {
            ["Accept"] = "application/json, text/plain, */*",
            ["Accept-Language"] = "en-US,en;q=0.9",
            ["Sec-Fetch-Site"] = "same-origin",
            ["Sec-Fetch-Mode"] = "cors",
            ["Sec-Fetch-Dest"] = "empty",
            ["Referer"] = BaseUrl + "/",
            ["Origin"] = BaseUrl,
            ["User-Agent"] = ua,
        };
        if (extra is not null)
            foreach (var kv in extra) h[kv.Key] = kv.Value;
        return h;
    }

    /// <summary>创建 temporarymail.com 临时邮箱：GET /api/?action=requestEmailAccess（key 空 → 随机分配）</summary>
    public static EmailInfo Generate()
    {
        var resp = Http.Get($"{BaseUrl}/api/?action=requestEmailAccess&key=&value=random", ApiHeaders(Ua()));
        if (resp.StatusCode == 429)
            throw new Exception($"temporarymail: 创建邮箱平台限流(429 Retry-After={RetryAfter(resp)})，请稍后重试: {resp.Body.Trim()}");
        if (!resp.Ok)
            throw new Exception($"temporarymail: 创建邮箱失败 http {resp.StatusCode}: {resp.Body.Trim()}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var address = Json.Str(root, "address").Trim();
        var secretKey = Json.Str(root, "secretKey").Trim();
        if (string.IsNullOrEmpty(address) || string.IsNullOrEmpty(secretKey))
            throw new Exception($"temporarymail: 创建响应缺少 address 或 secretKey: {resp.Body.Trim()}");
        return new EmailInfo("temporarymail-com", address, secretKey);
    }

    /// <summary>读取临时邮箱（key 无效时返回 {"error","code":500} 会解析失败提示 secretKey 可能失效）</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        token = (token ?? "").Trim();
        if (token.Length == 0) throw new Exception("temporarymail: secretKey 为空");

        var body = CheckInbox(token);
        var result = new List<Email>();

        // 平台响应两种合法形态：空箱 []，有信为 map[id]→对象
        var root = Json.Parse(body);
        if (root is JsonObject obj)
        {
            foreach (var kv in obj)
                if (kv.Value is JsonObject item) result.Add(One(item, email));
            return result;
        }
        if (root is JsonArray arr)
        {
            foreach (var item in arr)
                if (item is JsonObject mo) result.Add(One(mo, email));
            return result;
        }
        throw new Exception("temporarymail: 解析收件箱响应失败（secretKey 可能已失效）");
    }

    /// <summary>归一单个邮件元素：详情覆盖真实 from/subject，view 端点取全文剥标签</summary>
    private static Email One(JsonObject mo, string email)
    {
        var raw = Json.ToDict(mo);
        // 列表元素无 to 字段，注入收件人地址以归一化
        raw["to"] = email;
        var id = Json.Str(mo, "id").Trim();
        if (id.Length > 0)
        {
            // 详情覆盖真实主题/发件人（失败不致命：列表元数据兜底）
            var detail = FetchDetail(id);
            if (detail is not null)
            {
                var subject = Json.Str(detail, "subject").Trim();
                var from = Json.Str(detail, "from").Trim();
                if (subject.Length > 0) raw["subject"] = subject;
                if (from.Length > 0) raw["from"] = from;
            }
            // /view/ 渲染端点全文（失败不致命：列表元数据兜底）
            var text = FetchView(id);
            if (text.Length > 0) raw["text"] = text;
        }
        return Normalize.NormalizeEmail(raw, email);
    }

    /// <summary>拉取 checkInbox 响应体；403/404 换备用 UA 重试一次，429 报错带 Retry-After</summary>
    private static string CheckInbox(string token)
    {
        foreach (var ua in new[] { Ua(), AltUa })
        {
            var resp = Http.Get(
                $"{BaseUrl}/api/?action=checkInbox&value={Uri.EscapeDataString(token)}",
                ApiHeaders(ua));
            if (resp.StatusCode == 429)
            {
                var ra = RetryAfter(resp);
                throw new Exception($"temporarymail: 读取收件箱平台限流(429 Retry-After={ra})，请拉大轮询间隔");
            }
            if (resp.Ok) return resp.Body;
            // 403/404 疑似 UA 键控风控：换备用 UA 重试一次
            if (resp.StatusCode != 403 && resp.StatusCode != 404)
                throw new Exception($"temporarymail: 读取收件箱失败 http {resp.StatusCode}: {resp.Body.Trim()}");
        }
        throw new Exception("temporarymail: 读取收件箱失败 http 403（两次尝试均被拒）");
    }

    /// <summary>提取 Retry-After 响应头（缺失返回空串）</summary>
    private static string RetryAfter(HttpResult resp)
        => resp.Headers.TryGetValue("Retry-After", out var v) ? v : "";

    /// <summary>详情端点（POST /api/?action=getEmail）：响应 {id:{...}} 单元素对象，取首个元素</summary>
    private static JsonObject? FetchDetail(string id)
    {
        try
        {
            var resp = Http.Post(
                $"{BaseUrl}/api/?action=getEmail&value={Uri.EscapeDataString(id)}",
                null, null, ApiHeaders(Ua()));
            if (resp.StatusCode == 429) return null; // 平台限流：降级为列表元数据
            if (!resp.Ok) return null;
            var root = Json.Parse(resp.Body) as JsonObject;
            if (root is null) return null;
            foreach (var kv in root)
                if (kv.Value is JsonObject det) return det;
            return null;
        }
        catch { /* 详情失败不阻断列表 */ }
        return null;
    }

    /// <summary>/view/ 渲染端点全文：剥 <br>/<p> 换行、实体反转义、正则删其余标签、逐行 trim</summary>
    private static string FetchView(string id)
    {
        try
        {
            var headers = new Dictionary<string, string>
            {
                ["Accept"] = "text/html, */*",
                ["User-Agent"] = Ua(),
                ["Referer"] = BaseUrl + "/",
            };
            var resp = Http.Get($"{BaseUrl}/view/?i={Uri.EscapeDataString(id)}&width=800", headers);
            if (!resp.Ok) return "";
            return ViewToText(resp.Body);
        }
        catch { return ""; }
    }

    private static readonly Regex TagRe =
        new("<script[\\s\\S]*?</script>|<style[\\s\\S]*?</style>|<[^>]+>", RegexOptions.Compiled);

    /// <summary>将 /view/ 响应剥标签还原为纯文本（&lt;br&gt;/&lt;p&gt; 换行保留）</summary>
    private static string ViewToText(string src)
    {
        src = src
            .Replace("<br />", "\n").Replace("<br/>", "\n").Replace("<br>", "\n")
            .Replace("<p>", "\n").Replace("</p>", "\n")
            .Replace("&nbsp;", " ").Replace("&gt;", ">").Replace("&lt;", "<")
            .Replace("&amp;", "&").Replace("&quot;", "\"");
        src = TagRe.Replace(src, " ");
        // 实体反转义（含 &quot;/&apos; 二次回转）
        src = System.Net.WebUtility.HtmlDecode(src)
            .Replace("&quot;", "\"").Replace("&apos;", "'");
        var lines = src.Split('\n');
        for (var i = 0; i < lines.Length; i++) lines[i] = lines[i].Trim();
        return string.Join("\n", lines).Trim();
    }
}