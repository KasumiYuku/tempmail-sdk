package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Flybymail 渠道实现（flybymail.com）
 * POST /api/recipients 建箱（空 JSON body，响应 id/email/createdAt/expiresAt，email 形如 temp_xxx@flybymail.com），
 * GET /api/recipients/{email}/emails 读信（按邮箱地址、非 id 查询，响应 {"emails":[...]}，官网 widget 源码确认该路径）。
 * 信件字段：id/from/to/subject/body（纯文本）/htmlBody（HTML 正文）/preview/time/read，无附件信息。
 *
 * 平台限制（实测确认，非 SDK 缺陷）：
 * MX 为 mail.flybymail.com (51.178.87.108) 单机，25/465/587/2525 全端口无 SMTP 应答横幅，连接即静默关闭，
 * TLS 握手 unexpected eof；向 temp_*@flybymail.com 发信 5 分 20 秒后 /api/recipients/{email}/emails 恒空、无退信。
 * 官网前端无 refresh/fetch 类拉信端点（仅 15 秒间隔轮询收件箱列表）且建箱只可能得到 flybymail.com 域。
 * 结论：平台 MX 静默 + API 不落件，判平台级不可收；SDK 读信路径正确，实现保持原样。
 */

const flybymailBase = "https://flybymail.com"

/* flybymailGenerateResponse 建箱响应 */
type flybymailGenerateResponse struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

/* flybymailInboxResponse 收件箱响应 */
type flybymailInboxResponse struct {
	Emails []map[string]interface{} `json:"emails"`
}

/* flybymailJsonHeaders 设置 flybymail 请求的通用 JSON 请求头 */
func flybymailJSONHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
}

/*
 * FlybymailGenerate 创建 flybymail.com 临时邮箱
 * POST /api/recipients（空 JSON body）返回 id/email/createdAt/expiresAt，expiresAt 为毫秒时间戳（约 4 小时）
 */
func FlybymailGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest(http.MethodPost, flybymailBase+"/api/recipients", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	flybymailJSONHeaders(req)

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("flybymail: 创建邮箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data flybymailGenerateResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("flybymail: 解析创建响应失败: %w", err)
	}
	if data.ID == "" || data.Email == "" || !strings.Contains(data.Email, "@") {
		return nil, fmt.Errorf("flybymail: 创建邮箱响应缺少必要字段: %s", string(body))
	}

	/* expiresAt 为毫秒时间戳，转换为秒供 EmailInfo 统一展示 */
	var expiresAt int64
	if data.ExpiresAt > 0 {
		expiresAt = data.ExpiresAt / 1000
	}

	return &CreatedMailbox{
		Channel:   "flybymail",
		Email:     data.Email,
		Token:     data.ID,
		ExpiresAt: expiresAt,
	}, nil
}

/*
 * FlybymailGetEmails 获取 flybymail.com 邮件列表
 * GET /api/recipients/{email}（按地址查询）返回 {"emails":[...]}，直接按多候选字段归一。
 * @param email - 邮箱地址
 * @param token - 建箱返回的收件人 ID（当前读信端点按邮箱地址查询，令牌作为保留参数）
 */
func FlybymailGetEmails(email, token string) ([]NormEmail, error) {
	address := strings.TrimSpace(email)
	if address == "" || !strings.Contains(address, "@") {
		return nil, fmt.Errorf("flybymail: 邮箱地址为空或格式错误")
	}

	u := flybymailBase + "/api/recipients/" + url.PathEscape(address) + "/emails"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	flybymailJSONHeaders(req)

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("flybymail: 获取邮件列表失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data flybymailInboxResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("flybymail: 解析邮件列表失败: %w", err)
	}

	out := make([]NormEmail, 0, len(data.Emails))
	for _, raw := range data.Emails {
		/* id 为数字时转换发件页保持一致，time 为毫秒时间戳时按 timestamp 归一 */
		entries := map[string]interface{}{
			"from":        raw["from"],
			"to":          raw["to"],
			"subject":     raw["subject"],
			"text":        raw["body"],
			"html":        raw["htmlBody"],
			"time":        raw["time"],
			"read":        raw["read"],
			"attachments": raw["attachments"],
		}
		entries["id"] = flybymailAnyString(raw, "id")
		entries["timestamp"] = flybymailTimestamp(raw)

		out = append(out, NormalizeMap(entries, address))
	}
	return out, nil
}

/* flybymailAnyString 从 map 提取候选键的首个非空字符串 */
func flybymailAnyString(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok && v != nil {
			switch val := v.(type) {
			case string:
				if strings.TrimSpace(val) != "" {
					return strings.TrimSpace(val)
				}
			case float64:
				return strconv.FormatInt(int64(val), 10)
			}
		}
	}
	return ""
}

/* flybymailTimestamp 归一邮件时间，候选 time/date 字段，time 为毫秒时间戳 */
func flybymailTimestamp(m map[string]interface{}) interface{} {
	if v, ok := m["time"]; ok && v != nil {
		return v
	}
	if v, ok := m["date"]; ok && v != nil {
		return v
	}
	return nil
}
