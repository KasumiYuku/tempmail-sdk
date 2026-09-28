using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// Tmpkit 渠道实现（tmpkit.com，Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）。
/// POST /api/rpc/tempmail/{procedure}（body 为 tRPC 包裹 {"json":{...}}）：
/// initSession 建箱（sessionId 与邮箱同返），getEmails（offset/limit）列信，
/// getEmailDetail（mailId 为数字）取正文（body 键）。
/// 会话粘性：token 保存 sessionId（tempMailSession=<sid>），读信显式携带
/// Cookie: tempmail_session=<sid>，防并行会话串箱。
/// </summary>
public static class Tmpkit
{
    private const string Base = "https://tmpkit.com";
    private const string RpcPrefix = Base + "/api/rpc/tempmail";
    private const string UA =
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /// <summary>tRPC 调用请求头（Content-Type JSON、Accept 通配、同站 Origin/Referer）</summary>
    private static Dictionary<string, string> RpcHeaders(string? cookie)
    {
        var h = new Dictionary<string, string>
        {
            ["User-Agent"] = UA,
            ["Accept"] = "*/*",
            ["Content-Type"] = "application/json",
            ["Origin"] = Base,
            ["Referer"] = Base + "/en",
        };
        if (!string.IsNullOrEmpty(cookie)) h["Cookie"] = "tempmail_session=" + cookie;
        return h;
    }

    /// <summary>对 tmpkit 发起 rpc 调用，返回 {"json":{...}} 载荷；非 2xx 或缺 json 抛异常</summary>
    private static JsonObject Rpc(string procedure, JsonObject reqBody, string? cookie)
    {
        var wrapped = new JsonObject { ["json"] = reqBody };
        var resp = Http.Post(RpcPrefix + "/" + procedure, wrapped.ToJsonString(),
            "application/json", RpcHeaders(cookie));
        resp.EnsureSuccess();
        var outer = Json.Parse(resp.Body) as JsonObject
            ?? throw new Exception($"tmpkit: 解析 {procedure} 响应失败");
        return outer["json"] as JsonObject
            ?? throw new Exception($"tmpkit: {procedure} 响应缺 json 载荷");
    }

    /// <summary>从 JsonObject 容错取字符串（与 Go 端 tmpkitMapGet 语义一致）</summary>
    private static string MapGet(JsonObject? m, string key)
    {
        if (m is null || !m.TryGetPropertyValue(key, out var v) || v is null) return "";
        return Json.NodeToString(v);
    }

    /// <summary>
    /// 创建 tmpkit.com 临时邮箱
    /// 调 initSession（tRPC 包裹 {"json":{}}），sessionId 与邮箱地址同返。
    /// </summary>
    public static EmailInfo Generate()
    {
        var data = Rpc("initSession", new JsonObject(), null);
        var sess = data["session"] as JsonObject
            ?? throw new Exception("tmpkit: 创建会话响应缺 session 字段");
        var email = MapGet(sess, "email").Trim();
        var sessionId = MapGet(sess, "sessionId").Trim();
        if (email.Length == 0 || sessionId.Length == 0 || !email.Contains('@'))
            throw new Exception("tmpkit: 创建会话响应缺少必要字段（email/sessionId）");
        return new EmailInfo("tmpkit", email, "tempMailSession=" + sessionId);
    }

    /// <summary>
    /// 获取 tmpkit.com 邮件列表
    /// getEmails（offset 0 / limit 20）取摘要，逐封 getEmailDetail 拉详情并
    /// 将详情键并入摘要；详情失败回退列表摘要。getEmails 返回的 session.email
    /// 与收信邮箱不符时报错，防止会话被覆盖。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        var raw = (token ?? "").Trim();
        var mailbox = raw.StartsWith("tempMailSession=", StringComparison.Ordinal)
            ? raw["tempMailSession=".Length..]
            : raw;
        if (mailbox.Length == 0) throw new Exception("tmpkit: 会话 token 为空");

        var data = Rpc("getEmails", new JsonObject { ["offset"] = 0, ["limit"] = 20 }, mailbox);

        /* 会话指向校验：getEmails 响应自带 session.email（无 Cookie 时
           session 为 null，同样视为会话失效） */
        if (data["session"] is JsonObject s)
        {
            var got = MapGet(s, "email").Trim();
            if (got.Length > 0 && got != email)
                throw new Exception($"tmpkit: 会话邮箱不匹配（响应 {got}，请求 {email}）");
        }
        else
        {
            throw new Exception("tmpkit: 会话已失效（getEmails 返回空会话）");
        }

        var list = data["emails"] as JsonArray
            ?? throw new Exception("tmpkit: 邮件列表响应缺 emails 字段");

        var result = new List<Email>();
        foreach (var item in list)
        {
            if (item is not JsonObject m)
                continue;
            var flat = Json.ToDict(m);
            /* mailId 为数字：合法时逐封拉详情（详情键并入摘要，跳过会话类键） */
            if (m["mailId"] is JsonValue mv && mv.TryGetValue<long>(out var mid) && mid > 0)
            {
                try
                {
                    var detail = Rpc("getEmailDetail", new JsonObject { ["mailId"] = mid }, mailbox);
                    foreach (var kv in detail)
                    {
                        if (kv.Key is "session" or "emails" or "total" or "error") continue;
                        flat[kv.Key] = Json.ToRaw(kv.Value) ?? "";
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