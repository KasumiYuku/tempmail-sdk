using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Shadowmail 渠道（shadowmail.win，固定密码 "Abcd1234!"）。
/// 注册 POST /api/register {"email":"sdk"+8位随机小写字母+"@gmail.com","password":密码}
/// （幂等 "Email already in use"）；登录 POST /api/login 同 body → "Successfull Login"
/// （注意此拼写）+ Set-Cookie sessionId=&lt;uuid&gt; 取纯 uuid；
/// 建箱 POST /api/new-address body {} 带 Cookie: sessionId=&lt;uuid&gt; → {"address":"&lt;id&gt;@shadowmail.win"}。
/// token = "shadowmail|&lt;account&gt;|&lt;password&gt;|&lt;sessionId&gt;"（读信按 | 拆 3 段）。
/// 会话以显式 Cookie 头逐请求携带（无 Cookie 罐模式），会话失效自动重登录重试一次。
/// </summary>
public static class Shadowmail
{
    private const string BaseUrl = "https://shadowmail.win";
    private const string Password = "Abcd1234!";
    private const string Domain = "shadowmail.win";
    private const string TokenPrefix = "shadowmail|";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    private const string LocalChars = "abcdefghijklmnopqrstuvwxyz";

    /// <summary>生成随机注册邮箱前缀（sdk+8 位小写字母）</summary>
    private static string RandomAccount()
    {
        var sb = new System.Text.StringBuilder("sdk");
        for (var i = 0; i < 8; i++) sb.Append(LocalChars[Random.Shared.Next(LocalChars.Length)]);
        return sb.ToString();
    }

    /// <summary>携带显式 Cookie 的 JSON POST，返回 (响应体, 状态码)</summary>
    private static (string Body, int Status) DoPost(string path, object? payload, string cookie)
    {
        var body = payload is null ? null : Json.Serialize(payload);
        var headers = new Dictionary<string, string>
        {
            ["Content-Type"] = "application/json",
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        if (cookie.Length > 0) headers["Cookie"] = cookie;
        var resp = Http.RawPost(BaseUrl + path, body, "application/json", headers);
        return (resp.Body, resp.StatusCode);
    }

    /// <summary>从 Set-Cookie 中提取 sessionId 值（纯 uuid，不含键名）</summary>
    private static string SessionFromSetCookie(HttpResult resp)
    {
        foreach (var sc in resp.SetCookies)
        {
            var kv = sc[..(sc.IndexOf(';') > 0 ? sc.IndexOf(';') : sc.Length)];
            if (kv.StartsWith("sessionId=", StringComparison.Ordinal))
                return kv["sessionId=".Length..];
        }
        return "";
    }

    /// <summary>注册或登录（POST /api/register、/api/login），完成后取得会话 Cookie</summary>
    private static string RegisterLogin(string account, bool isLogin)
    {
        var path = isLogin ? "/api/login" : "/api/register";
        var payload = new Dictionary<string, string> { ["email"] = account, ["password"] = Password };
        var headers = new Dictionary<string, string>
        {
            ["Content-Type"] = "application/json",
            ["Accept"] = "application/json",
            ["User-Agent"] = Ua,
        };
        var resp = Http.RawPost(BaseUrl + path, Json.Serialize(payload), "application/json", headers);
        if (!resp.Ok)
            throw new Exception($"shadowmail {path}: http {resp.StatusCode}");
        var root = Json.Parse(resp.Body) as JsonObject;
        var message = Json.Str(root, "message");
        if (isLogin && message != "Successfull Login")
            throw new Exception($"shadowmail login: {message}");
        // 重复注册（幂等）：消息为 Email already in use 时视为账号已存在，继续走登录
        if (!isLogin && message != "Successfully Registered" && message != "Email already in use")
            throw new Exception($"shadowmail register: {message}");
        var session = SessionFromSetCookie(resp);
        if (isLogin && session.Length == 0)
            throw new Exception("shadowmail login: 未下发 sessionId Cookie");
        return session;
    }

    /// <summary>注册账号并创建临时邮箱地址；token 凭据串："shadowmail|<account>|<password>|<sessionId>"</summary>
    public static EmailInfo Generate()
    {
        var account = RandomAccount() + "@gmail.com";

        // 1) 注册（幂等：已存在同名账号则跳过）
        RegisterLogin(account, false);
        // 2) 登录取得 sessionId（纯 uuid，不合成键值对）
        var session = RegisterLogin(account, true);
        // 3) 创建地址（每账号 12 槽）：显式 Cookie 头传 sessionId
        var (body, status) = DoPost("/api/new-address", new Dictionary<string, object?>(), "sessionId=" + session);
        if (status < 200 || status >= 300)
            throw new Exception($"shadowmail new-address: http {status}");
        var root = Json.Parse(body) as JsonObject;
        var address = Json.Str(root, "address").Trim().ToLowerInvariant();
        if (address.Length == 0 || !address.EndsWith("@" + Domain, StringComparison.Ordinal))
            throw new Exception("shadowmail new-address: 响应缺少有效地址");

        // Token 持久化：account|password|sessionId（sessionId 为 uuid，无分隔符冲突）
        var token = TokenPrefix + account + "|" + Password + "|" + session;
        return new EmailInfo("shadowmail", address, token);
    }

    /// <summary>解析凭据串为 account/password/sessionId 三元组（按 | 拆 3 段）</summary>
    private static (string Account, string Pwd, string Session) ParseToken(string token)
    {
        if (!token.StartsWith(TokenPrefix, StringComparison.Ordinal))
            throw new Exception("shadowmail: token 格式错误");
        var parts = token[TokenPrefix.Length..].Split('|');
        if (parts.Length != 3)
            throw new Exception("shadowmail: token 字段缺失");
        var account = parts[0];
        var pwd = parts[1];
        var session = parts[2];
        if (account.Length == 0 || pwd.Length == 0 || session.Length == 0)
            throw new Exception("shadowmail: token 凭据字段为空");
        return (account, pwd, session);
    }

    /// <summary>
    /// 读取收件箱：POST /api/get-emails {"address":email} 带 Cookie；
    /// 401/404 重新登录换 sessionId 重试一次；响应 {"message":"Emails read","mails":[...]}。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        var (account, _, session) = ParseToken((token ?? "").Trim());

        (string Body, int Status) Fetch(string currentSession)
        {
            var headers = new Dictionary<string, string>
            {
                ["Content-Type"] = "application/json",
                ["Accept"] = "application/json",
                ["User-Agent"] = Ua,
                ["Cookie"] = "sessionId=" + currentSession,
            };
            var body = Json.Serialize(new Dictionary<string, string> { ["address"] = email });
            var resp = Http.RawPost(BaseUrl + "/api/get-emails", body, "application/json", headers);
            return (resp.Body, resp.StatusCode);
        }

        var (raw, status) = Fetch(session);
        // sessionId 最长 1 小时（Max-Age 3600），过期后重登录重试一次
        if (status == 401 || status == 404)
        {
            try
            {
                var fresh = RegisterLogin(account, true);
                if (fresh.Length > 0)
                {
                    session = fresh;
                    (raw, status) = Fetch(session);
                }
            }
            catch { /* 重登录失败按原响应处理 */ }
        }
        if (status < 200 || status >= 300)
            throw new Exception($"shadowmail get-emails: http {status}");

        var root = Json.Parse(raw) as JsonObject;
        if (root is null || Json.Str(root, "message") != "Emails read")
            throw new Exception($"shadowmail get-emails: {Json.Str(root, "message")}");

        var result = new List<Email>();
        var mails = root["mails"] as JsonArray;
        if (mails is null) return result;
        foreach (var m in mails)
        {
            if (m is not JsonObject mo) continue;
            var flat = Json.ToDict(mo);
            flat["from"] = Json.Str(mo, "sender");
            flat["to"] = email;
            flat["date"] = Json.Str(mo, "created_at");
            // 平台无 text/html 区分，body 为正文（默认按纯文本处理）
            flat["text"] = Json.Str(mo, "body");
            result.Add(Normalize.NormalizeEmail(flat, email));
        }
        return result;
    }
}