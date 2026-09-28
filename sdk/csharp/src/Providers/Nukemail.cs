using System;
using System.Collections.Generic;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Nukemail 渠道（nukemail.app，SHA-256 PoW 建箱）。
/// 建箱：1) GET /api/pow/challenge?difficulty=4 → {"id","challenge","difficulty"}；
/// 2) 本地求最小 nonce 使 SHA-256(challenge+nonce) 十六进制前 difficulty 位为 0；
/// 3) GET /api/domains 取首个非 premium 域；4) POST /api/inbox/create
/// {"address":"nuke"+10位随机,"domain","pow_id","pow_nonce":&lt;十进制字符串&gt;} → {"token","email"}。
/// 读信 GET /api/inbox 带 Cookie: nukemail_token=&lt;token&gt;；state=="expired" 或空或失败时
/// POST /api/inbox/resume 重设会话后重试一次。会话以显式 Cookie 头逐请求携带（无 Cookie 罐模式）。
/// </summary>
public static class Nukemail
{
    private const string BaseUrl = "https://nukemail.app";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private const string LocalChars = "abcdefghijklmnopqrstuvwxyz0123456789";

    /// <summary>求解 PoW：最小 nonce 使 SHA-256(UTF-8 "<challenge><nonce>") 小写十六进制前缀为 0×difficulty</summary>
    private static long SolvePow(string challenge, int difficulty)
    {
        var prefix = new string('0', difficulty);
        using var sha = SHA256.Create();
        for (long nonce = 0; nonce < long.MaxValue; nonce++)
        {
            var hex = Convert.ToHexString(sha.ComputeHash(
                Encoding.UTF8.GetBytes(challenge + nonce))).ToLowerInvariant();
            if (hex.StartsWith(prefix, StringComparison.Ordinal))
                return nonce;
        }
        throw new Exception("nukemail generate: PoW 求解失败");
    }

    /// <summary>生成本地随机名（"nuke"+10 位 [a-z0-9]）</summary>
    private static string RandomAddress()
    {
        var sb = new StringBuilder("nuke");
        for (var i = 0; i < 10; i++) sb.Append(LocalChars[Random.Shared.Next(LocalChars.Length)]);
        return sb.ToString();
    }

    /// <summary>取第一个非 premium 的活跃域名</summary>
    private static string PickDomain()
    {
        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        var resp = Http.Get($"{BaseUrl}/api/domains", headers);
        resp.EnsureSuccess();
        var root = Json.Parse(resp.Body) as JsonObject;
        var domains = root?["domains"] as JsonArray;
        if (domains is null)
            throw new Exception("nukemail generate: 域名列表解析失败");
        foreach (var d in domains)
        {
            if (d is not JsonObject domObj) continue;
            var isPremium = (domObj["is_premium_only"] as JsonValue)?.GetValue<bool>() ?? false;
            var dom = Json.Str(domObj, "domain").Trim();
            if (!isPremium && dom.Length > 0) return dom;
        }
        throw new Exception("nukemail generate: 无可用非 premium 域名");
    }

    /// <summary>创建临时邮箱（PoW 建箱）：token 为平台返回的 NUKE-&lt;随机&gt; 访问码</summary>
    public static EmailInfo Generate()
    {
        // 1) 取 PoW 挑战
        var headers = new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        var chResp = Http.RawGet($"{BaseUrl}/api/pow/challenge?difficulty=4", headers);
        if (!chResp.Ok)
            throw new Exception($"nukemail generate: challenge http {chResp.StatusCode}");
        var root = Json.Parse(chResp.Body) as JsonObject;
        var challenge = Json.Str(root, "challenge").Trim();
        var powId = Json.Str(root, "id").Trim();
        if (powId.Length == 0 || challenge.Length == 0)
            throw new Exception("nukemail generate: challenge 响应缺少 id/challenge");
        var difficulty = 4;
        if (root?["difficulty"] is JsonValue dv && dv.TryGetValue<long>(out var dl) && dl > 0)
            difficulty = (int)dl;

        // 2) 本地求 PoW 解
        var nonce = SolvePow(challenge, difficulty);

        // 3) 取域名并建箱
        var dom = PickDomain();
        var body = Json.Serialize(new Dictionary<string, object?>
        {
            ["address"] = RandomAddress(),
            ["domain"] = dom,
            ["pow_id"] = powId,
            ["pow_nonce"] = nonce.ToString(System.Globalization.CultureInfo.InvariantCulture),
        });
        var createHeaders = new Dictionary<string, string>
        {
            ["Content-Type"] = "application/json",
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        var cResp = Http.RawPost($"{BaseUrl}/api/inbox/create", body, "application/json", createHeaders);
        if (!cResp.Ok)
            throw new Exception(
                $"nukemail generate: create http {cResp.StatusCode}: {cResp.Body.Trim()}");
        var cRoot = Json.Parse(cResp.Body) as JsonObject;
        var token = Json.Str(cRoot, "token").Trim();
        var email = Json.Str(cRoot, "email").Trim();
        if (token.Length == 0 || email.Length == 0)
            throw new Exception("nukemail generate: create 响应缺少 token/email");
        return new EmailInfo("nukemail", email, token);
    }

    /// <summary>
    /// 读取收件箱：GET /api/inbox 带 Cookie: nukemail_token=&lt;token&gt;；
    /// 会话已失效（state 空/expired 或请求失败）时经 POST /api/inbox/resume 重设会话后重试一次。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        token = (token ?? "").Trim();
        if (token.Length == 0) throw new Exception("nukemail: token 为空");
        var cookie = "nukemail_token=" + token;

        JsonObject? Do()
        {
            var headers = new Dictionary<string, string>
            {
                ["Accept"] = "application/json",
                ["User-Agent"] = Ua,
                ["Cookie"] = cookie,
            };
            var resp = Http.RawGet($"{BaseUrl}/api/inbox", headers);
            if (!resp.Ok)
                throw new Exception($"nukemail 读信: http {resp.StatusCode}");
            return Json.Parse(resp.Body) as JsonObject;
        }

        JsonObject? data;
        try
        {
            data = Do();
        }
        catch
        {
            data = null;
        }
        var state = data is null ? "" : Json.Str(data, "state").Trim();
        if (data is null || state == "expired" || state.Length == 0)
        {
            // 会话可能已过期：经 resume 重设会话后重试
            try
            {
                var resumeBody = Json.Serialize(new Dictionary<string, string> { ["accessCode"] = token });
                var resumeHeaders = new Dictionary<string, string>
                {
                    ["Content-Type"] = "application/json",
                    ["User-Agent"] = Ua,
                };
                Http.RawPost($"{BaseUrl}/api/inbox/resume", resumeBody, "application/json", resumeHeaders);
            }
            catch { /* resume 失败按重试失败处理 */ }
            data = Do();
        }

        var result = new List<Email>();
        var msgs = data?["messages"] as JsonArray;
        if (msgs is null) return result;
        foreach (var m in msgs)
        {
            if (m is not JsonObject mo) continue;
            var raw = Json.ToDict(mo);
            raw["to"] = email;
            // 平台消息字段为 body_html/body_text：text 空取 body_text、html 空取 body_html
            if (!raw.TryGetValue("text", out var tv) || tv is null)
                if (raw.TryGetValue("body_text", out var bt) && bt is not null)
                    raw["text"] = bt;
            if (!raw.TryGetValue("html", out var hv) || hv is null)
                if (raw.TryGetValue("body_html", out var bh) && bh is not null)
                    raw["html"] = bh;
            raw["date"] = raw.TryGetValue("received_at", out var ra) ? ra : null;
            raw["read"] = raw.TryGetValue("read", out var rd) ? rd : null;
            // sender 是小写发件人地址，sender_name 是展示名（Normalize 用 sender_email/sender 提取）
            raw["sender_email"] = raw.TryGetValue("sender", out var sd) ? sd : null;
            result.Add(Normalize.NormalizeEmail(raw, email));
        }
        return result;
    }
}