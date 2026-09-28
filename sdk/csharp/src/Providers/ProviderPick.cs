using System;
using System.Collections.Generic;

namespace XxxXTeam.TempMail.Providers;

/// <summary>
/// provider 公共字段提取辅助（与 Go provider 包 pickStr/messageIDOf 对齐）。
/// 集中处理多候选键取值与邮件 ID 提取，避免各渠道重复实现。
/// </summary>
internal static class ProviderPick
{
    /// <summary>
    /// 按候选键顺序取出第一个非空字符串值（标量转字符串）。
    /// 对象/数组等复合值不参与（与 Go messageIDOf 仅接受 string 一致）。
    /// </summary>
    public static string Str(IDictionary<string, object?> m, params string[] keys)
    {
        foreach (var k in keys)
        {
            if (!m.TryGetValue(k, out var v) || v is null) continue;
            if (v is not string s) continue;
            if (s.Length > 0) return s;
        }
        return "";
    }

    /// <summary>从消息对象提取邮件 ID，候选字段 id/Id/slug/messageId/message_id（与 Go messageIDOf 一致）</summary>
    public static string MessageID(IDictionary<string, object?> m)
        => Str(m, "id", "Id", "slug", "messageId", "message_id");
}