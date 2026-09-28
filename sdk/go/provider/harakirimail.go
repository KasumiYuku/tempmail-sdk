package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * 平台级结论（2026-09-28 深诊，keep 实现不变）：
 *   1. 平台 API 面仅 harakirimail.com 域名下的 /api/v1/inbox/{name} 与 /api/v1/email/{id}，
 *      官网前端 JS 未引用任何备用子域或旧端点（仅提及 blog.harakirimail.com），无可绕路径。
 *   2. 非浏览器 UA（如 curl/8.5.0）会触发 Cloudflare "Just a moment..." 挑战页；
 *      SDK 的 tls-client 浏览器指纹请求实测可正常通过（HTTP 200 JSON），CF 不构成主因。
 *   3. 实测经 smtp.exmail.qq.com:465 发信到新建收件箱，12 分钟以上 total_count 恒为 0，
 *      邮件从未入箱（平台收信链路故障，MTA mail.harakirimail.com 侧无 SMTP 可达响应）。
 *   4. 平台已实锤把 UTF-8 中文替换为 U+FFFD 导致正文损坏。
 * verify 结论 no-receive 属平台级：收信不进 + 正文损坏，SDK 读信链路本身正确，保持当前实现。
 */
const harakirimailBase = "https://harakirimail.com"

/* harakirimailDefaultHeaders 设置通用请求头 */
func harakirimailDefaultHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0")
}

/* 随机生成 12 位字母数字字符串作为收件箱名 */
func harakirimailRandomName() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

/* HarakirimailGenerate 创建 harakirimail 临时邮箱 */
func HarakirimailGenerate() (*CreatedMailbox, error) {
	name := harakirimailRandomName()
	email := fmt.Sprintf("%s@harakirimail.com", name)

	/* 可选：调用收件箱接口验证地址可用 */
	u := fmt.Sprintf("%s/api/v1/inbox/%s", harakirimailBase, name)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	harakirimailDefaultHeaders(req)

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("harakirimail: 验证收件箱失败 %d", resp.StatusCode)
	}

	return &CreatedMailbox{Channel: "harakirimail", Email: email, Token: ""}, nil
}

/* HarakirimailGetEmails 获取 harakirimail 邮件列表 */
func HarakirimailGetEmails(email string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("harakirimail: 邮箱地址为空")
	}

	/* 从邮箱地址提取收件箱名 */
	name := email
	if at := strings.Index(email, "@"); at > 0 {
		name = email[:at]
	}

	/* 获取收件箱列表 */
	u := fmt.Sprintf("%s/api/v1/inbox/%s", harakirimailBase, name)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	harakirimailDefaultHeaders(req)

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
		return nil, fmt.Errorf("harakirimail inbox: %d", resp.StatusCode)
	}

	var inboxResp struct {
		Emails []struct {
			ID       string `json:"_id"`
			From     string `json:"from"`
			Subject  string `json:"subject"`
			Received string `json:"received"`
		} `json:"emails"`
	}
	if err := json.Unmarshal(body, &inboxResp); err != nil {
		return nil, err
	}

	emails := make([]NormEmail, 0, len(inboxResp.Emails))
	for _, raw := range inboxResp.Emails {
		/* 获取单封邮件详情以拿到正文与附件 */
		textBody, htmlBody, attachments, err := harakirimailFetchDetail(raw.ID)
		if err != nil {
			return nil, err
		}

		flat := map[string]interface{}{
			"id":          raw.ID,
			"from":        raw.From,
			"to":          email,
			"subject":     raw.Subject,
			"date":        raw.Received,
			"text":        textBody,
			"html":        htmlBody,
			"attachments": attachments,
			"isRead":      false,
		}
		emails = append(emails, NormalizeMap(flat, email))
	}
	return emails, nil
}

/*
 * harakirimailFetchDetail 获取单封邮件的详情（正文与附件）
 * 平台详情接口的真实正文字段为 bodytext（无独立 html 字段），
 * 因此 bodytext 同时作为纯文本与 HTML 的来源；
 * parts 为附件数组，平台当前恒为空数组，属平台限制。
 * 详情为空时返回错误，交由上层如实处理，不静默丢弃。
 */
func harakirimailFetchDetail(id string) (string, string, []map[string]interface{}, error) {
	if id == "" {
		return "", "", nil, fmt.Errorf("harakirimail: 邮件 ID 为空")
	}
	u := fmt.Sprintf("%s/api/v1/email/%s", harakirimailBase, id)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", "", nil, err
	}
	harakirimailDefaultHeaders(req)

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", nil, fmt.Errorf("harakirimail: 邮件详情 HTTP %d", resp.StatusCode)
	}

	var detail map[string]interface{}
	if err := json.Unmarshal(body, &detail); err != nil {
		return "", "", nil, err
	}

	/* 纯文本候选：首选平台真实字段 bodytext，保留旧候选回退 */
	textBody := firstNonEmptyString(detail, "bodytext", "body_text", "text")
	/* HTML 候选：平台无 html 字段，回退到 bodytext 作 HTML */
	htmlBody := firstNonEmptyString(detail, "body_html", "html", "html_content", "bodytext", "body_text", "text")

	if textBody == "" && htmlBody == "" {
		return "", "", nil, fmt.Errorf("harakirimail: 详情响应缺少正文字段 bodytext")
	}

	/* 解析 parts 附件数组：平台当前恒为空数组，属平台限制 */
	attachments := make([]map[string]interface{}, 0)
	if v, ok := detail["parts"].([]interface{}); ok {
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				attachments = append(attachments, m)
			}
		}
	}

	return textBody, htmlBody, attachments, nil
}

/*
 * firstNonEmptyString 按候选键顺序提取第一个非空字符串值
 */
func firstNonEmptyString(detail map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := detail[key].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
