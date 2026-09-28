using System;
using System.Collections.Generic;
using System.Text.Json.Nodes;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// MailTicking 渠道（www.mailticking.com，旧域名 temporary-mail.net 的更名站）。
/// 三步协议（均 application/json POST）：
/// 1) 建箱 POST /get-mailbox body {"types":["4"]}（4=独立域名，排除 Gmail 别名），
///    响应 {"success":true,"email":"...","activate_token":"..."}（联合视图读 code 字段，
///    为空则 token=email）；
/// 2) 激活 POST /activate-email body {"email","source":"api","activate_token":&lt;token&gt;}；
/// 3) 列信 POST /get-emails?lang=en body {"email","code":&lt;token&gt;}，
///    空箱 {"emails":[],"success":true}；success 非真且 needNewEmail=true 时
///    报「mailbox expired」的换箱语义错误。
/// 请求头：Accept / Accept-Language en-US,en;q=0.9 / Content-Type application/json /
/// UA + Referer 与 Origin 取当前主机。
/// 列表字段多候选迁移：mail_from/from_mail/from_email/sender_address/from_address/
/// send_addr/mail_addr/address_from/ho_from/fromname/fromS → from；sender 缺取 from；
/// id 缺取 mail_id；date 缺取 received_at；随后归一化。
/// 官网从未提供独立读信端点，本渠道只具备列表能力（列表-only 形态参与维护）。
/// </summary>
public static class Mailticking
{
    private const string BaseUrl = "https://www.mailticking.com";
    private const string Ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0";

    /// <summary>构造 mailticking 请求头（含 Referer/Origin 站点特征，UA 共享）</summary>
    private static Dictionary<string, string> BuildHeaders()
    {
        return new Dictionary<string, string>
        {
            ["Accept"] = "application/json",
            ["Accept-Language"] = "en-US,en;q=0.9",
            ["Content-Type"] = "application/json",
            ["User-Agent"] = Ua,
            ["Referer"] = BaseUrl + "/",
            ["Origin"] = BaseUrl,
        };
    }

    /// <summary>执行 POST 并解析 JSON 响应；非 2xx 时优先使用响应体中的 error/message 文案</summary>
    private static JsonObject PostJson(string path, object? payload)
    {
        var body = payload is null ? null : Json.Serialize(payload);
        var resp = Http.Post(path, body, "application/json", BuildHeaders());
        var root = Json.Parse(resp.Body) as JsonObject;
        if (!resp.Ok)
        {
            var msg = Json.Str(root, "error");
            if (msg.Length == 0) msg = Json.Str(root, "message");
            if (msg.Length == 0) msg = resp.Body.Trim();
            throw new Exception($"mailticking: http {resp.StatusCode}: {msg}");
        }
        if (root is null)
            throw new Exception("mailticking: parse response");
        return root;
    }

    /// <summary>响应 success 非真时的报错文案（error/message 均空取 unknown error）</summary>
    private static string FailMessage(JsonObject? root)
    {
        var msg = Json.Str(root, "error");
        if (msg.Length == 0) msg = Json.Str(root, "message");
        return msg.Length == 0 ? "unknown error" : msg;
    }

    /// <summary>
    /// 创建 mailticking 邮箱账号：POST /get-mailbox（type=4 固定取独立域名）→
    /// POST /activate-email 激活。token 必须携带 activate code（列信协议依赖激活会话），
    /// 响应流程：联合视图读 code 字段，为空则 token=email 兜底。
    /// </summary>
    public static EmailInfo Generate()
    {
        var box = PostJson(BaseUrl + "/get-mailbox",
            new Dictionary<string, object?> { ["types"] = new[] { "4" } });
        if (!((box["success"] as JsonValue)?.GetValue<bool>() ?? false))
            throw new Exception($"mailticking: get-mailbox failed: {FailMessage(box)}");
        var email = Json.Str(box, "email").Trim();
        var token = Json.Str(box, "code").Trim();
        if (token.Length == 0) token = email;
        if (token.Length == 0)
            throw new Exception("mailticking: get-mailbox returned empty email/activate_token");
        if (email.Length == 0)
            throw new Exception("mailticking: get-mailbox returned empty email");

        // 激活邮箱，使后续列信请求与服务器记录的最新会话一致
        var act = PostJson(BaseUrl + "/activate-email",
            new Dictionary<string, object?>
            {
                ["email"] = email,
                ["source"] = "api",
                ["activate_token"] = token,
            });
        if (!((act["success"] as JsonValue)?.GetValue<bool>() ?? false))
            throw new Exception($"mailticking: activate-email failed: {FailMessage(act)}");

        return new EmailInfo("mailticking", email, token);
    }

    /// <summary>
    /// 获取邮件列表：POST /get-emails?lang=en（body {"email","code"}）。
    /// 空箱返回空列表不报错；success 非真且 needNewEmail=true 时报「mailbox expired」换箱语义错误。
    /// 列表字段名多候选提取，未命中的字段留给 Normalize 既有候选策略。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        var addr = (email ?? "").Trim();
        if (addr.Length == 0) throw new Exception("mailticking: empty email");
        var tok = (token ?? "").Trim();
        if (tok.Length == 0) throw new Exception("mailticking: empty activate code");

        var data = PostJson(BaseUrl + "/get-emails?lang=en",
            new Dictionary<string, string> { ["email"] = addr, ["code"] = tok });
        if (!((data["success"] as JsonValue)?.GetValue<bool>() ?? false))
        {
            if ((data["needNewEmail"] as JsonValue)?.GetValue<bool>() ?? false)
                throw new Exception("mailticking: mailbox expired, please renew");
            throw new Exception("mailticking: get-emails failed");
        }

        var result = new List<Email>();
        var emails = data["emails"] as JsonArray;
        if (emails is null) return result;
        foreach (var m in emails)
        {
            if (m is not JsonObject mo) continue;
            result.Add(Normalize.NormalizeEmail(MigratedFields(mo), addr));
        }
        return result;
    }

    /// <summary>
    /// 将列表字段尽量映射到统一候选字段（发件人多候选 → from；from 兜底 sender；
    /// mail_id → id；received_at → date），未命中的字段保留原样交给 Normalize。
    /// </summary>
    private static Dictionary<string, object?> MigratedFields(JsonObject raw)
    {
        var flat = Json.ToDict(raw);
        var fromCandidates = new[]
        {
            "mail_from", "from_mail", "from_email", "sender_address", "from_address",
            "send_addr", "mail_addr", "address_from", "ho_from", "fromname", "fromS",
        };
        if (!flat.ContainsKey("from"))
        {
            foreach (var key in fromCandidates)
            {
                if (flat.TryGetValue(key, out var v) && v is string s && s.Trim().Length > 0)
                {
                    flat["from"] = s;
                    break;
                }
            }
        }
        // sender 缺失时以 from 兜底
        if (!flat.ContainsKey("sender") &&
            flat.TryGetValue("from", out var fv) && fv is string fs && fs.Trim().Length > 0)
        {
            flat["sender"] = fs;
        }
        // id 缺失取 mail_id
        if (!flat.ContainsKey("id") && flat.TryGetValue("mail_id", out var mid))
            flat["id"] = mid;
        // date 缺失取 received_at
        if (!flat.ContainsKey("date") && flat.TryGetValue("received_at", out var recv))
            flat["date"] = recv;
        return flat;
    }
}