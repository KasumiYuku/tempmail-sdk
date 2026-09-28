using System;
using System.Collections.Generic;
using System.Text.RegularExpressions;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// GeneratorEmail 渠道实现（generator.email，PHP SSR 网页型）。
/// 无独立建箱 API：SSR 直接生成随机邮箱并写进页面内联 window.SITE_DATA
/// （cur_user/cur_domain，邮箱=user@domain）；读信同为 SSR：GET /inbox4/
/// 带 inbox_ctx Cookie 返回该邮箱渲染页。列表行为 class 含 list-group-item2
/// 的条目（平台实测真实类为 list-group-item2，非 list-group-item），内含
/// from_div_45g45gg / subj_div_45g45gg / time_div_45g45gg 三要素块。
/// 本站不提供原文正文，SDK 按列表三要素归一。
/// token 语义：{email, domain, user} JSON 快照。
/// </summary>
public static class GeneratorEmail
{
    private const string Base = "https://generator.email";
    private const string InboxUrl = Base + "/inbox4/";
    private const string UA =
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36";

    /// <summary>模块级 inbox_ctx Cookie 状态（本渠道独占）</summary>
    private static string _inboxCtx = "";

    /* SITE_DATA 快照提取（cur_user / cur_domain） */
    private static readonly Regex UserRe = new(@"cur_user:""([^""]*)""", RegexOptions.Compiled);
    private static readonly Regex DomainRe = new(@"cur_domain:""([^""]*)""", RegexOptions.Compiled);
    /* 列表条目与三要素块（平台实测真实类为 list-group-item2，按类名后缀锚定） */
    private static readonly Regex ItemRe = new(
        @"<div[^>]*class=""[^""]*list-group-item2[^""]*""[^>]*>([\s\S]*?)(?:</div>\s*){3}",
        RegexOptions.Compiled);
    private static readonly Regex FromRe = new(
        @"class=""[^""]*from_div_45g45gg[^""]*""[^>]*>([\s\S]*?)</div>", RegexOptions.Compiled);
    private static readonly Regex SubjRe = new(
        @"class=""[^""]*subj_div_45g45gg[^""]*""[^>]*>([\s\S]*?)</div>", RegexOptions.Compiled);
    private static readonly Regex TimeRe = new(
        @"class=""[^""]*time_div_45g45gg[^""]*""[^>]*>([\s\S]*?)</div>", RegexOptions.Compiled);
    private static readonly Regex ScriptRe = new(@"<(script|style)[\s\S]*?</\1>", RegexOptions.Compiled | RegexOptions.IgnoreCase);
    private static readonly Regex TagRe = new(@"<[^>]+>", RegexOptions.Compiled);

    /// <summary>收下响应 Set-Cookie 中 inbox_ctx 值（URL 编码态）</summary>
    private static void MergeInboxCtx(HttpResult resp)
    {
        foreach (var line in resp.SetCookies)
        {
            var kv = line.Split(';')[0].Trim();
            if (kv.StartsWith("inbox_ctx=", StringComparison.Ordinal)
                && kv.Length > "inbox_ctx=".Length)
                _inboxCtx = kv["inbox_ctx=".Length..];
        }
    }

    /// <summary>请求收件箱渲染页并返回 HTML（邮箱由服务端依据 inbox_ctx Cookie 选择）</summary>
    private static string FetchPage()
    {
        var headers = new Dictionary<string, string>
        {
            ["User-Agent"] = UA,
            ["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            ["Accept-Language"] = "en-US,en;q=0.9",
            ["Referer"] = Base + "/",
        };
        if (_inboxCtx.Length > 0) headers["Cookie"] = "inbox_ctx=" + _inboxCtx;
        var resp = Http.Get(InboxUrl, headers);
        MergeInboxCtx(resp);
        resp.EnsureSuccess();
        return resp.Body;
    }

    /// <summary>取正则第一个捕获组，无匹配返回空串</summary>
    private static string MatchFirst(Regex re, string src)
    {
        var m = re.Match(src);
        return m.Success ? m.Groups[1].Value : "";
    }

    /// <summary>去标签与脚本/样式块，压缩空白</summary>
    private static string StripTags(string s)
    {
        var outText = ScriptRe.Replace(s, " ");
        outText = TagRe.Replace(outText, " ");
        return Regex.Replace(outText, @"\s+", " ").Trim();
    }

    /// <summary>
    /// 创建 generator.email 临时邮箱
    /// 解析首页 SITE_DATA 快照（cur_user/cur_domain）得到邮箱地址。
    /// </summary>
    public static EmailInfo Generate()
    {
        var src = FetchPage();
        var user = MatchFirst(UserRe, src);
        var domain = MatchFirst(DomainRe, src);
        if (user.Length == 0 || domain.Length == 0)
            throw new Exception("generator-email: 首页未携带邮箱快照（cur_user/cur_domain）");
        var email = user + "@" + domain;
        var tokenJson = new System.Text.Json.Nodes.JsonObject
        {
            ["email"] = email,
            ["domain"] = domain,
            ["user"] = user,
        }.ToJsonString();
        return new EmailInfo("generator-email", email, tokenJson);
    }

    /// <summary>
    /// 获取 generator.email 收件箱
    /// 解析收件箱渲染页的列表条目（from/subj/time 三要素）；本站不提供
    /// 原文正文，SDK 按摘要归一。
    /// </summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        var sess = System.Text.Json.Nodes.JsonNode.Parse(token ?? "") as System.Text.Json.Nodes.JsonObject
            ?? throw new Exception("generator-email: 会话凭据解析失败");
        if (Json.Str(sess, "email") != email)
            throw new Exception("generator-email: 会话邮箱与查询邮箱不匹配");
        var sessDomain = Json.Str(sess, "domain");

        var src = FetchPage();
        /* 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换 */
        var gotDomain = MatchFirst(DomainRe, src);
        if (gotDomain != sessDomain)
            throw new Exception($"generator-email: 会话域名已切换（token {sessDomain}，服务端 {gotDomain}）");

        /* 列表区域锚定（#email-table ... #markodile 之间） */
        var listStart = src.IndexOf(@"id=""email-table""", StringComparison.Ordinal);
        var listEnd = src.IndexOf(@"id=""markodile""", StringComparison.Ordinal);
        var region = listStart >= 0 && listEnd > listStart ? src.Substring(listStart, listEnd - listStart) : "";

        var result = new List<Email>();
        foreach (Match m in ItemRe.Matches(src))
        {
            var raw = m.Groups[1].Value;
            /* 跳过列表容器外的候选：要求条目文本来自列表区域 */
            if (region.Length > 0 && !region.Contains(raw, StringComparison.Ordinal)) continue;
            var from = StripTags(MatchFirst(FromRe, raw));
            var subject = StripTags(MatchFirst(SubjRe, raw));
            var when = StripTags(MatchFirst(TimeRe, raw));
            if (from.Length == 0 && subject.Length == 0 && when.Length == 0) continue;

            var row = new Dictionary<string, object?>
            {
                ["from"] = from,
                ["to"] = email,
                ["subject"] = subject,
                ["date"] = when,
            };
            result.Add(Normalize.NormalizeEmail(row, email));
        }
        return result;
    }
}