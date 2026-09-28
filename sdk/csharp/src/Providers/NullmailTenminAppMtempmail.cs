using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// NullMail 渠道（nullmail.cc / maildock.store）：无认证 REST。
/// POST /api/emails（空 JSON body）建箱，响应 {"address":"...@maildock.store","expiry":"..."}；
/// 读信 GET /api/emails/{address}（URL 编码），响应 {"expiry":"...","emails":[...]}，
/// 列表项只有 id/sender/subject/delivered，正文须逐封二拉 GET /api/emails/{addr}/body/{id}
/// （响应 {"body":...}）。
/// </summary>
public static class Nullmail
{
    private const string BaseUrl = "https://www.nullmail.cc";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private static Dictionary<string, string> ReqHeaders()
    {
        return new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["Origin"] = BaseUrl,
            ["Referer"] = BaseUrl + "/",
            ["User-Agent"] = Ua,
        };
    }

    /// <summary>创建临时邮箱：POST /api/emails（空 JSON body），token 复用完整地址</summary>
    public static EmailInfo Generate()
    {
        var headers = ReqHeaders();
        headers["Content-Type"] = "application/json";
        var resp = Http.Post($"{BaseUrl}/api/emails", "{}", "application/json", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var address = Json.Str(root, "address").Trim();
        if (address.Length == 0) throw new Exception("nullmail 建箱: 响应缺少 address 字段");
        return new EmailInfo("nullmail", address, address, createdAt: Json.Str(root, "expiry"));
    }

    /// <summary>
    /// 读取收件箱：GET /api/emails/{address}（完整地址 URL 编码）。
    /// 列表项无正文，逐封二拉 body 端点取纯文本正文；正文拉取失败降级留空不阻断列表。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        if (email.Length == 0) throw new Exception("nullmail 读信: 邮箱地址为空");

        var resp = Http.Get($"{BaseUrl}/api/emails/{Uri.EscapeDataString(email)}", ReqHeaders());
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var msgs = root?["emails"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            raw["to"] = email;
            // normalizeDate 候选键不含 delivered，显式映射为 date 后归一化
            var delivered = ProviderPick.Str(raw, "delivered");
            if (delivered.Length > 0) raw["date"] = delivered;
            // 列表只有 id/sender/subject/delivered，正文逐封二拉 body 端点
            var id = ProviderPick.Str(raw, "id");
            if (id.Length > 0)
            {
                try
                {
                    var br = Http.Get(
                        $"{BaseUrl}/api/emails/{Uri.EscapeDataString(email)}/body/{Uri.EscapeDataString(id)}",
                        ReqHeaders());
                    if (br.Ok && Json.Parse(br.Body) is JsonObject bo)
                    {
                        var body = Json.Str(bo, "body");
                        if (body.Length > 0) raw["text"] = body;
                    }
                }
                catch { /* 正文二拉失败降级留空 */ }
            }
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}

/// <summary>
/// Tenmin.app 渠道（真实 API 域 api.tenmin.app）：建箱+读信共用 GET /api/inbox/{localpart}。
/// localpart 为随机 6 位小写十六进制串（首访即建箱，无显式创建接口）；
/// 响应 {"inboxId":..,"address":"..@tenmin.app","ttl":600,"count":0,"messages":[]}，
/// messages[] 元素字段：id/from/subject/text/html/receivedAt（from 为 {name,address} 对象）。
/// </summary>
public static class TenminApp
{
    private const string BaseUrl = "https://api.tenmin.app";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private const string HexChars = "0123456789abcdef";

    private static string LocalPart()
    {
        var sb = new System.Text.StringBuilder();
        for (var i = 0; i < 6; i++) sb.Append(HexChars[Random.Shared.Next(HexChars.Length)]);
        return sb.ToString();
    }

    /// <summary>请求 /api/inbox/{localpart}，返回解析后的收件箱响应</summary>
    private static JsonObject FetchInbox(string localpart)
    {
        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        var resp = Http.Get($"{BaseUrl}/api/inbox/{localpart}", headers);
        resp.EnsureSuccess();
        return Json.Parse(resp.Body) as JsonObject ?? throw new Exception("tenmin-app: 解析收件箱响应失败");
    }

    /// <summary>创建临时邮箱：首次 GET 随机 localpart 即自动建箱（10 分钟 TTL），token 存储 localpart</summary>
    public static EmailInfo Generate()
    {
        var local = LocalPart();
        var data = FetchInbox(local);
        var address = Json.Str(data, "address").Trim();
        if (address.Length == 0) address = local + "@tenmin.app";
        // ttl 为秒：按响应 TTL 折算过期时间
        var ttl = 0L;
        if (data["ttl"] is JsonValue tv && tv.TryGetValue<long>(out var tl)) ttl = tl;
        var expires = DateTimeOffset.UtcNow.AddSeconds(ttl > 0 ? ttl : 600).ToString("o", System.Globalization.CultureInfo.InvariantCulture);
        return new EmailInfo("tenmin-app", address, local, createdAt: expires);
    }

    /// <summary>读取收件箱：复用建箱同一 localpart 轮询；from 为 {name,address} 对象时拆出地址</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        token = (token ?? "").Trim();
        if (token.Length == 0) throw new Exception("tenmin-app: token 为空");

        var data = FetchInbox(token);
        var msgs = data["messages"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            // from 为对象（{name,address}）时拆出地址字段
            if (mo["from"] is JsonObject fromObj)
            {
                var addr = Json.Str(fromObj, "address").Trim();
                var name = Json.Str(fromObj, "name").Trim();
                raw["from"] = addr.Length > 0 && name.Length > 0
                    ? $"{name} <{addr}>"
                    : addr.Length > 0 ? addr : name;
            }
            raw["to"] = email;
            raw["text"] = Json.Str(mo, "text");
            raw["html"] = Json.Str(mo, "html");
            raw["date"] = Json.Str(mo, "receivedAt");
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}

/// <summary>
/// MTempMail 渠道（mtempmail.com，公共 key 认证）。
/// 建箱: POST /api/emails/{apiKey}（body {}）→ {"status":true,"data":{email,domain,ip,fingerprint,
///       expire_at,created_at,id,email_token}}；
/// 读信: GET /api/messages/{apiKey}/{email} → {"status":true,"mailbox":..,"email_token":..,"messages":[]}，
///       消息列表元素 body 为 [{content_type,value}] 数组，from 为 [{full}] 数组（mailgun 入站 webhook 风格）。
/// 邮箱 24 小时有效。
/// </summary>
public static class Mtempmail
{
    private const string BaseUrl = "https://mtempmail.com";
    private const string PublicKey = "pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private const char Bullet = '•';

    /// <summary>清洗主题前导分隔符（后台可能以 "• " 开头拼接微件）</summary>
    private static string CleanSubject(string s)
    {
        var subj = s.Trim();
        while (subj.Length > 0 && (subj[0] == Bullet || subj[0] == '·')) subj = subj[1..].TrimStart(' ');
        return subj;
    }

    /// <summary>拼接正文纯文本（body[].value 按序，以换行分隔）</summary>
    private static string BodyText(JsonArray parts)
    {
        var sb = new System.Text.StringBuilder();
        foreach (var p in parts)
        {
            if (p is not JsonObject po) continue;
            var v = Json.Str(po, "value");
            if (v.Length > 0)
            {
                if (sb.Length > 0) sb.Append('\n');
                sb.Append(v);
            }
        }
        return sb.ToString();
    }

    /// <summary>提取首个 text/html 段</summary>
    private static string BodyHtml(JsonArray parts)
    {
        foreach (var p in parts)
        {
            if (p is not JsonObject po) continue;
            if (Json.Str(po, "content_type") == "text/html")
            {
                var v = Json.Str(po, "value");
                if (v.Length > 0) return v;
            }
        }
        return "";
    }

    private static Dictionary<string, string> ReqHeaders() => new()
    {
        ["Accept"] = "application/json",
        ["User-Agent"] = Ua,
    };

    /// <summary>创建 mtempmail 临时邮箱：POST /api/emails/{apiKey}（空 JSON body）</summary>
    public static EmailInfo Generate()
    {
        var headers = ReqHeaders();
        headers["Content-Type"] = "application/json";
        var resp = Http.Post($"{BaseUrl}/api/emails/{PublicKey}", "{}", "application/json", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var data = root?["data"] as JsonObject;
        if ((root?["status"] as JsonValue)?.GetValue<bool>() != true || data is null)
            throw new Exception("mtempmail: 建箱响应格式无效");
        var email = Json.Str(data, "email").Trim();
        if (email.Length == 0) throw new Exception("mtempmail: 建箱响应缺少邮箱");
        return new EmailInfo("mtempmail", email, Json.Str(data, "email_token").Trim(),
            createdAt: Json.Str(data, "expire_at"));
    }

    /// <summary>读取收件箱：GET /api/messages/{apiKey}/{email}；token 元数据仅为校验，不参与请求</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        if (email.Length == 0) throw new Exception("mtempmail: 邮箱为空");
        if (string.IsNullOrEmpty((token ?? "").Trim())) throw new Exception("mtempmail: token 为空");

        var resp = Http.Get($"{BaseUrl}/api/messages/{PublicKey}/{Uri.EscapeDataString(email)}", ReqHeaders());
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var msgs = root?["messages"] as JsonArray;
        var result = new List<Email>();
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            raw["to"] = email;
            var subj = Json.Str(mo, "subject");
            if (subj.Length > 0) raw["subject"] = CleanSubject(subj);
            if (mo["body"] is JsonArray bodyArr)
            {
                var text = BodyText(bodyArr);
                var html = BodyHtml(bodyArr);
                if (text.Length > 0) raw["text"] = text;
                if (html.Length > 0) raw["html"] = html;
            }
            raw["date"] = Json.Str(mo, "created_at");
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}