package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Zerodrop 渠道实现（zerodrop.dev）
 * 无认证 REST：建箱无需请求，本地生成 "sdk"+8 位随机名，地址为 <名>@zerodrop-sandbox.online；
 * 读信 GET /api/inbox/{name}?source=sdk，响应形如 {"emails":[...],"count":N}。
 * 平台邮件对象只有 id/from/to/subject/receivedAt/raw/otp/magicLink：正文仅存在于
 * raw（完整 MIME 原文，头部与 body 以 \r\n\r\n 空行分隔），无 text/html 字段，
 * 故须从 raw 中剥离头部提取纯文本 body（Content-Type: text/plain; charset=UTF-8）
 * 填入 text，html 留空由 NormalizeMap 互转（SSE /api/inbox/{name}/stream 暂不接入）。
 */

const zerodropBase = "https://zerodrop.dev"
const zerodropDomain = "zerodrop-sandbox.online"

/* zerodropInboxResponse 收件箱响应 */
type zerodropInboxResponse struct {
	Emails []map[string]interface{} `json:"emails"`
	Count  int                      `json:"count"`
}

/* pickStr 多候选字段取值：返回第一个存在且为字符串的键值 */
func pickStr(m map[string]interface{}, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t, true
				}
			default:
				if t != nil {
					return fmt.Sprintf("%v", t), true
				}
			}
		}
	}
	return "", false
}

/* zerodropLocalName 生成 "sdk"+8 位随机本地名 */
func zerodropLocalName() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	b.WriteString("sdk")
	for i := 0; i < 8; i++ {
		b.WriteByte(chars[rand.Intn(len(chars))])
	}
	return b.String()
}

/* zerodropRawBody 从 raw（完整 MIME 原文）提取纯文本正文：
 * 定位首个空行（RFC 5322 头部/正文分隔，\r\n\r\n 或 \n\n），其后部分即 body；
 * 平台转发件正文为 text/plain，直接用 raw 转义形式原样保留（含 \r\n 换行）。
 */
func zerodropRawBody(raw string) string {
	if idx := strings.Index(raw, "\r\n\r\n"); idx >= 0 {
		return raw[idx+4:]
	}
	if idx := strings.Index(raw, "\n\n"); idx >= 0 {
		return raw[idx+2:]
	}
	return ""
}

/*
 * ZerodropGenerate 创建临时邮箱
 * 建箱无需请求，本地生成随机名，token 复用完整地址以便收件箱回查
 */
func ZerodropGenerate() (*CreatedMailbox, error) {
	email := zerodropLocalName() + "@" + zerodropDomain
	return &CreatedMailbox{Channel: "zerodrop", Email: email, Token: email}, nil
}

/*
 * ZerodropGetEmails 读取收件箱
 * GET /api/inbox/{name}?source=sdk，多候选字段交由 NormalizeMap 归一化
 */
func ZerodropGetEmails(email, token string) ([]NormEmail, error) {
	parts := strings.SplitN(strings.TrimSpace(email), "@", 2)
	if len(parts) != 2 || parts[1] != zerodropDomain {
		return nil, fmt.Errorf("zerodrop 读信: 非 %s 域邮箱地址", zerodropDomain)
	}

	req, err := http.NewRequest("GET", zerodropBase+"/api/inbox/"+url.PathEscape(parts[0])+"?source=sdk", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("zerodrop 读信: http %d", resp.StatusCode)
	}

	var data zerodropInboxResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}

	out := make([]NormEmail, 0, len(data.Emails))
	for _, m := range data.Emails {
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		// 平台响应无 to 字段，收件人固定为当前邮箱
		flat["to"] = email
		// 正文仅存在于 raw（完整 MIME 原文），无 text/html 字段：提取纯文本 body 作 text
		if v, ok := pickStr(m, "raw"); ok {
			if rawBody := zerodropRawBody(v); rawBody != "" {
				flat["text"] = rawBody
			}
		}
		// from/subject 原字段名与 NormalizeMap 候选一致；date 用 receivedAt（NormalizeMap 支持）
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
