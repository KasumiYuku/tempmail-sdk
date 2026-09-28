package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Crazymailing 渠道实现（crazymailing.com）
 * Next.js 全栈站点，API 结构：
 * - 域名列表 GET /api/domains，响应 {"domains":[{"id":"...","domain":"crazymailing.com",
 *   "isSiteDomain":true}]}。
 * - 建箱 POST /api/mailbox（空 JSON body），响应
 *   {"mailbox":{"id":"...","address":"...@crazymailing.com","expiresAt":"<RFC3339>"}}。
 * - 读信 GET /api/messages?mailbox=<完整地址 URL 编码>，响应 {"messages":[...]}。
 * - 单封正文 GET /api/message/{id}/body，响应为完整 HTML 页面（站点以 iframe 嵌入）。
 * - SSE 实时推送 /api/stream?mailbox=... 可选，暂不接入。
 */

const crazymailingBase = "https://crazymailing.com"

/* crazymailingDomainsResponse 域名列表响应 */
type crazymailingDomainsResponse struct {
	Domains []map[string]interface{} `json:"domains"`
}

/* crazymailingMailboxResponse 建箱响应 */
type crazymailingMailboxResponse struct {
	Mailbox *struct {
		ID        string `json:"id"`
		Address   string `json:"address"`
		ExpiresAt string `json:"expiresAt"`
	} `json:"mailbox"`
}

/* crazymailingMessagesResponse 读信响应 */
type crazymailingMessagesResponse struct {
	Messages []map[string]interface{} `json:"messages"`
}

/* crazymailingSetHeaders 设置 crazymailing API 请求通用头 */
func crazymailingSetHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", crazymailingBase)
	req.Header.Set("Referer", crazymailingBase+"/")
	req.Header.Set("User-Agent", GetCurrentUA())
}

/*
 * CrazymailingGenerate 创建临时邮箱
 * POST /api/mailbox（空 JSON body）；不预取域名列表，
 * 域名由服务端统一分配（当前仅 @crazymailing.com）。
 */
func CrazymailingGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest(http.MethodPost, crazymailingBase+"/api/mailbox", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	crazymailingSetHeaders(req)

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
		return nil, fmt.Errorf("crazymailing: 创建邮箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data crazymailingMailboxResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("crazymailing: 解析创建响应失败: %w", err)
	}
	if data.Mailbox == nil || strings.TrimSpace(data.Mailbox.Address) == "" {
		return nil, fmt.Errorf("crazymailing: 创建响应缺少 mailbox.address: %s", string(body))
	}

	return &CreatedMailbox{
		Channel:   "crazymailing",
		Email:     data.Mailbox.Address,
		Token:     data.Mailbox.ID,
		ExpiresAt: data.Mailbox.ExpiresAt,
	}, nil
}

/*
 * CrazymailingGetEmails 读取收件箱
 * GET /api/messages?mailbox=<完整地址 URL 编码>，响应 {"messages":[...]}；
 * 对每个元素逐封 GET /api/message/{id}/body 拉取正文（响应为完整 HTML 页面），
 * 详情失败时以列表摘要（from/fromName/subject/preview/receivedAt/seen）归一化，不阻断列表。
 * @param email - 完整邮箱地址
 * @param token - 建箱返回的 mailbox id
 */
func CrazymailingGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("crazymailing: 邮箱地址为空")
	}

	u := crazymailingBase + "/api/messages?mailbox=" + url.QueryEscape(email)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	crazymailingSetHeaders(req)

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
		return nil, fmt.Errorf("crazymailing: 读取收件箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data crazymailingMessagesResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("crazymailing: 解析收件箱响应失败: %w", err)
	}

	out := make([]NormEmail, 0, len(data.Messages))
	for _, m := range data.Messages {
		/* 注入收件人地址（to 字段缺失时保底） */
		if _, ok := m["to"]; !ok {
			m["to"] = email
		}
		/* receivedAt 为日期字符串（RFC3339），normalizeDate 候选键已含 */
		/* 列表元素为摘要，正文须逐封二拉 */
		if id := messageIDOf(m); id != "" {
			if html, err := crazymailingGetBody(id); err == nil && strings.TrimSpace(html) != "" {
				m["html"] = html
			}
		}
		out = append(out, NormalizeMap(m, email))
	}
	return out, nil
}

/*
 * crazymailingGetBody 拉取单封正文
 * GET /api/message/{id}/body，响应为完整 HTML 页面（站点 iframe 嵌入）。
 */
func crazymailingGetBody(id string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, crazymailingBase+"/api/message/"+url.PathEscape(id)+"/body", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Origin", crazymailingBase)
	req.Header.Set("Referer", crazymailingBase+"/")
	req.Header.Set("User-Agent", GetCurrentUA())

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("crazymailing: 获取正文失败 http %d", resp.StatusCode)
	}
	return string(body), nil
}
