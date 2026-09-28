using System;
using System.Collections.Generic;
using System.Globalization;
using System.Linq;
using System.Text.Json.Nodes;
using System.Text.RegularExpressions;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// temp-mail.gg 渠道（Laravel + Livewire v3）。
/// 与 Go 端 temp_mail_gg.go 同构：GET / 首页取 data-csrf 令牌与初始
/// wire:snapshot → POST /livewire/update 调 generateEmail 生成邮箱，
/// 读信以空 calls 轮询取 Inbox 列表，再逐封 selectEmail 取详情。
/// 平台每次 livewire/update 都轮换会话 Cookie，本渠道以静态 Cookie 字典 +
/// 显式 Cookie 头维护（无 Cookie 罐裸客户端，逐响应覆写）。
/// token 为前缀 + JSON{email, csrf, snapshot}；邮箱约 30 分钟过期。
/// </summary>
public static class TempMailGg
{
    private const string Base = "https://temp-mail.gg";

    /// <summary>识别本渠道会话凭据串的前缀</summary>
    private const string TokenPrefix = "temp-mail-gg|";

    private static readonly Regex DataCsrfRe = new("data-csrf=\"([^\"]*)\"", RegexOptions.Compiled);
    private static readonly Regex WireSnapshotRe = new("wire:snapshot=\"([^\"]*)\"", RegexOptions.Compiled);

    // 列表行容器（wire:click="selectEmail(<数字id>)"）与其成员（h3=发件人、p(text-zinc-300)=主题、
    // p(line-clamp-2)=预览、span(text-xs)=相对时间）
    private static readonly Regex RowRe = new("(?is)<div\\b(?=[^>]*wire:click=\"selectEmail\\((\\d+)\\)\")[^>]*>([\\s\\S]*?)</div>", RegexOptions.Compiled);
    private static readonly Regex FromRe = new("(?is)<h3\\b[^>]*class=\"[^\"]*\\bfont-semibold\\b[^\"]*\"[^>]*>([\\s\\S]*?)</h3>", RegexOptions.Compiled);
    private static readonly Regex SubjectRe = new("(?is)<p\\b[^>]*class=\"[^\"]*\\btext-zinc-300\\b[^\"]*\"[^>]*>([\\s\\S]*?)</p>", RegexOptions.Compiled);
    private static readonly Regex PreviewRe = new("(?is)<p\\b[^>]*class=\"[^\"]*\\bline-clamp-2\\b[^\"]*\"[^>]*>([\\s\\S]*?)</p>", RegexOptions.Compiled);
    private static readonly Regex WhenRe = new("(?is)<span\\b[^>]*class=\"[^\"]*\\btext-xs\\b[^\"]*\"[^>]*>([\\s\\S]*?)</span>", RegexOptions.Compiled);

    // 详情视图：h3(text-xl)=主题、span 以 "From:" 开头=发件人、div x-show 含文本页签=正文
    private static readonly Regex DetailSubjectRe = new("(?is)<h3\\b[^>]*class=\"[^\"]*\\btext-xl\\b[^\"]*\"[^>]*>([\\s\\S]*?)</h3>", RegexOptions.Compiled);
    private static readonly Regex DetailFromRe = new("(?is)<span\\b[^>]*>\\s*From:\\s*([\\s\\S]*?)</span>", RegexOptions.Compiled);
    private static readonly Regex DetailTextRe = new("(?is)<div\\b[^>]*x-show=\"[^\"]*activeTab === 'text'[^\"]*\"[^>]*>([\\s\\S]*?)</div>", RegexOptions.Compiled);

    private static readonly Regex RelativeRe = new("^\\s*(\\d+)\\s+(second|minute|hour|day)s?\\s+ago\\s*$",
        RegexOptions.IgnoreCase | RegexOptions.Compiled);

    // 渠道类内部静态 Cookie 字典：temp_email / tempmail_session / XSRF-TOKEN 会话凭据（逐响应覆写）
    private static readonly Dictionary<string, string> SessionCookies = new(StringComparer.Ordinal);
    private static readonly object CookiesLock = new();

    /// <summary>取当前会话 Cookie 头（"k=v; k=v"）；无会话则为空串</summary>
    private static string CookieHeader()
    {
        lock (CookiesLock)
        {
            return string.Join("; ", SessionCookies.Select(kv => $"{kv.Key}={kv.Value}"));
        }
    }

    /// <summary>用响应 Set-Cookie 覆写会话 Cookie 字典（平台每次 update 轮换，必须逐响应覆写）</summary>
    private static void ApplyCookies(HttpResult resp)
    {
        lock (CookiesLock)
        {
            foreach (var raw in resp.SetCookies)
            {
                var semi = raw.IndexOf(';');
                var kv = (semi < 0 ? raw : raw[..semi]).Trim();
                var eq = kv.IndexOf('=');
                if (eq <= 0) continue;
                var k = kv[..eq].Trim();
                var v = kv[(eq + 1)..].Trim();
                if (k.Length > 0) SessionCookies[k] = v;
            }
        }
    }

    /// <summary>截断正文用于错误消息</summary>
    private static string Truncate(string s, int max = 200)
    {
        s = (s ?? "").Trim();
        return s.Length <= max ? s : s[..max];
    }

    /// <summary>HTML 片段转纯文本：去 script/style/标签 + 实体反转义 + 空白压缩</summary>
    private static string StripHtml(string src)
    {
        var s = Regex.Replace(src, "(?is)<(script|style)[\\s\\S]*?</\\1>", " ");
        s = Regex.Replace(s, "(?s)<[^>]+>", " ");
        s = System.Net.WebUtility.HtmlDecode(s).Replace("\x00", "");
        return string.Join(" ", s.Split((char[]?)null, StringSplitOptions.RemoveEmptyEntries));
    }

    /// <summary>
    /// POST /livewire/update（唯一边界）；响应 Set-Cookie 逐响应覆写会话字典。
    /// method 为空表示纯轮询（平台 20s 自动刷新形态），否则为组件方法调用。
    /// 返回（新快照文本，effects.html）。
    /// </summary>
    private static (string Snapshot, string EffectsHtml) Update(string snapshot, string csrf,
        string method, params long[] args)
    {
        var calls = new JsonArray();
        if (method.Length > 0)
        {
            var p = new JsonArray();
            foreach (var a in args) p.Add(a);
            calls.Add(new JsonObject { ["path"] = "", ["method"] = method, ["params"] = p });
        }
        var payload = new JsonObject
        {
            ["_token"] = csrf,
            ["components"] = new JsonArray(new JsonObject
            {
                ["snapshot"] = snapshot,
                ["updates"] = new JsonObject(),
                ["calls"] = calls,
            }),
        };

        var headers = new Dictionary<string, string>
        {
            ["User-Agent"] = TempmailEEUtil.UA,
            ["Accept"] = "text/html, application/xhtml+xml",
            ["Accept-Language"] = "en-US,en;q=0.9",
            ["Content-Type"] = "application/json",
            ["X-Livewire"] = "",
            ["X-Requested-With"] = "XMLHttpRequest",
            ["Origin"] = Base,
            ["Referer"] = Base + "/",
        };
        var cookie = CookieHeader();
        if (cookie.Length > 0) headers["Cookie"] = cookie;

        var resp = Http.RawPost(Base + "/livewire/update", Json.Serialize(payload), "application/json", headers);
        ApplyCookies(resp);
        if (resp.StatusCode == 419)
            throw new Exception("temp-mail-gg: livewire 会话过期（419），请重新 Generate");
        if (resp.StatusCode < 200 || resp.StatusCode >= 300)
            throw new Exception($"temp-mail-gg: livewire/update http {resp.StatusCode}: {Truncate(resp.Body)}");

        var root = Json.Parse(resp.Body) as JsonObject;
        var comps = root?["components"] as JsonArray;
        if (comps is not { Count: > 0 } || comps[0] is not JsonObject c0)
            throw new Exception("temp-mail-gg: 解析 update 响应失败");
        var snap = Json.NodeToString(c0["snapshot"]).Trim();
        var html = Json.Str(c0["effects"] as JsonObject, "html");
        return (snap, html);
    }

    /// <summary>创建 temp-mail.gg 临时邮箱：首页取 CSRF + 初始快照 → update(generateEmail)</summary>
    public static EmailInfo Generate()
    {
        var homeHeaders = new Dictionary<string, string>
        {
            ["User-Agent"] = TempmailEEUtil.UA,
            ["Accept"] = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
            ["Accept-Language"] = "en-US,en;q=0.9",
        };
        var home = Http.RawGet(Base, homeHeaders);
        ApplyCookies(home);
        if (home.StatusCode < 200 || home.StatusCode >= 300)
            throw new Exception($"temp-mail-gg: 首页 http {home.StatusCode}: {Truncate(home.Body)}");

        var mCsrf = DataCsrfRe.Match(home.Body);
        var mSnap = WireSnapshotRe.Match(home.Body);
        if (!mCsrf.Success || !mSnap.Success)
            throw new Exception("temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱");
        var csrf = mCsrf.Groups[1].Value.Trim();
        // HTML 属性内的快照 JSON 是双转义态（&quot;），反转义一层还原为原始 JSON
        var snapRaw = System.Net.WebUtility.HtmlDecode(mSnap.Groups[1].Value);

        // 平台免费额度为免登录每时段 5 个，耗尽时响应快照无 email
        var (snap2, _) = Update(snapRaw, csrf, "generateEmail");
        if (snap2.Length == 0)
            throw new Exception("temp-mail-gg: generateEmail 响应异常（components 缺失）");
        var snapObj = Json.Parse(snap2) as JsonObject;
        var email = Json.Str(snapObj?["data"] as JsonObject, "email").Trim();
        if (email.Length == 0)
            throw new Exception("temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录配额（每时段 5 个）");

        // 凭据串打包 {email, csrf, snapshot}；邮箱约 30 分钟过期
        var tokenJson = Json.Serialize(new Dictionary<string, object?>
        {
            ["email"] = email,
            ["csrf"] = csrf,
            ["snapshot"] = snap2,
        });
        return new EmailInfo("temp-mail-gg", email, TokenPrefix + tokenJson,
            createdAt: DateTimeOffset.UtcNow.AddMinutes(30).ToString("o", CultureInfo.InvariantCulture));
    }

    /// <summary>读取 temp-mail.gg 收件箱：轮询列表 → 逐封 selectEmail 拉详情</summary>
    public static List<Email> GetEmails(string email, string? token)
    {
        email = (email ?? "").Trim();
        if (email.Length == 0)
            throw new Exception("temp-mail-gg: 邮箱为空，请重新 Generate");

        token = (token ?? "").Trim();
        if (!token.StartsWith(TokenPrefix, StringComparison.Ordinal))
            throw new Exception("temp-mail-gg: 凭据串前缀不符，请重新 Generate");
        JsonObject? sess;
        try { sess = Json.Parse(token[TokenPrefix.Length..]) as JsonObject; }
        catch { sess = null; }
        if (sess is null) throw new Exception("temp-mail-gg: 解析凭据串失败");

        var sessEmail = Json.Str(sess, "email").Trim();
        var csrf = Json.Str(sess, "csrf").Trim();
        var snapshot = Json.Str(sess, "snapshot").Trim();
        if (sessEmail.Length == 0 || snapshot.Length == 0)
            throw new Exception("temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate");
        if (!string.Equals(sessEmail, email, StringComparison.OrdinalIgnoreCase))
            throw new Exception($"temp-mail-gg: 邮箱与凭据不匹配（{sessEmail} != {email}），请重新 Generate");

        // 轮询刷新（空 calls = 平台 20s 自动刷新形态）
        var (pollSnap, pollHtml) = Update(snapshot, csrf, "");
        if (pollSnap.Length > 0)
        {
            var pollObj = Json.Parse(pollSnap) as JsonObject;
            var current = Json.Str(pollObj?["data"] as JsonObject, "email").Trim();
            if (current.Length > 0 && !string.Equals(current, email, StringComparison.OrdinalIgnoreCase))
                throw new Exception($"temp-mail-gg: 会话已被切换至 {current}（与请求邮箱 {email} 不一致），请重新 Generate");
        }
        if (pollHtml.Trim().Length == 0)
            throw new Exception("temp-mail-gg: 轮询响应无 effects.html，会话可能已失效");

        var rows = ParseList(pollHtml);
        var result = new List<Email>();
        if (rows.Count == 0) return result;

        // 逐封拉详情正文；详情失败回退列表字段（不中断整批）
        var latestSnap = pollSnap.Length > 0 ? pollSnap : snapshot;
        var now = DateTime.UtcNow;
        foreach (var row in rows)
        {
            if (row.Id.Length == 0) continue;

            var from = row.From;
            var subject = row.Subject;
            var text = row.Preview;

            if (long.TryParse(row.Id, NumberStyles.None, CultureInfo.InvariantCulture, out var idNum))
            {
                try
                {
                    var (dSnap, dHtml) = Update(latestSnap, csrf, "selectEmail", idNum);
                    if (dSnap.Length > 0) latestSnap = dSnap;
                    var d = ParseDetail(dHtml);
                    if (d.Ok)
                    {
                        if (d.From.Length > 0) from = d.From;
                        if (d.Subject.Length > 0) subject = d.Subject;
                        if (d.Text.Length > 0) text = d.Text;
                    }
                }
                catch { /* 详情偶发 419/网络抖动不回滚已折叠列表 */ }
            }

            if (text.Length == 0) text = subject;
            var html = TempmailEEUtil.TextToHtml(text);

            result.Add(Normalize.NormalizeEmail(new Dictionary<string, object?>
            {
                ["id"] = row.Id,
                ["from"] = from,
                ["to"] = email,
                ["subject"] = subject,
                ["content"] = text,
                ["html"] = html,
                ["date"] = ParseRelative(row.When, now),
            }, email));
        }
        return result;
    }

    /// <summary>列表行（从轮询 effects.html 解析）</summary>
    private sealed class ListRow
    {
        public string Id = "";
        public string From = "";
        public string Subject = "";
        public string Preview = "";
        public string When = "";
    }

    /// <summary>
    /// 解析 Inbox 条目：容器 div wire:click="selectEmail(<数字id>)"，
    /// 其内 h3(font-semibold)=发件人、p(text-zinc-300)=主题、
    /// p(line-clamp-2)=预览、span(text-xs)=相对时间。
    /// </summary>
    private static List<ListRow> ParseList(string htmlBlock)
    {
        var rows = new List<ListRow>();
        foreach (Match m in RowRe.Matches(htmlBlock))
        {
            var inner = m.Groups[2].Value;
            var row = new ListRow { Id = m.Groups[1].Value };
            var mFrom = FromRe.Match(inner);
            if (mFrom.Success) row.From = StripHtml(mFrom.Groups[1].Value);
            var mSubject = SubjectRe.Match(inner);
            if (mSubject.Success) row.Subject = StripHtml(mSubject.Groups[1].Value);
            var mPreview = PreviewRe.Match(inner);
            if (mPreview.Success) row.Preview = StripHtml(mPreview.Groups[1].Value);
            var mWhen = WhenRe.Match(inner);
            if (mWhen.Success) row.When = StripHtml(mWhen.Groups[1].Value);
            if (row.Id.Length > 0) rows.Add(row);
        }
        return rows;
    }

    /// <summary>解析详情视图：span "From:" 前缀取发件人、h3(text-xl) 取主题、文本页签 div 取正文</summary>
    private static (string From, string Subject, string Text, bool Ok) ParseDetail(string htmlBlock)
    {
        var from = "";
        var subject = "";
        var text = "";
        var mFrom = DetailFromRe.Match(htmlBlock);
        if (mFrom.Success) from = StripHtml(mFrom.Groups[1].Value);
        var mSubject = DetailSubjectRe.Match(htmlBlock);
        if (mSubject.Success) subject = StripHtml(mSubject.Groups[1].Value);
        var mText = DetailTextRe.Match(htmlBlock);
        if (mText.Success) text = StripHtml(mText.Groups[1].Value);
        if (from.Length == 0 && subject.Length == 0 && text.Length == 0) return ("", "", "", false);
        return (from, subject, text, true);
    }

    /// <summary>解析相对时间（"N seconds/minutes/hours/days ago"）为 UTC ISO 时间；失败回退当前 UTC</summary>
    private static string ParseRelative(string s, DateTime now)
    {
        var m = RelativeRe.Match((s ?? "").Trim());
        if (!m.Success) return now.ToString("o", CultureInfo.InvariantCulture);
        var seconds = m.Groups[2].Value.ToLowerInvariant() switch
        {
            "second" => 1L,
            "minute" => 60L,
            "hour" => 3600L,
            "day" => 86400L,
            _ => 0L,
        };
        if (seconds == 0 || !long.TryParse(m.Groups[1].Value, NumberStyles.None,
                CultureInfo.InvariantCulture, out var n))
            return now.ToString("o", CultureInfo.InvariantCulture);
        return now.Subtract(TimeSpan.FromSeconds(n * seconds)).ToString("o", CultureInfo.InvariantCulture);
    }
}