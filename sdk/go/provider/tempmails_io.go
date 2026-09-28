package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * TempmailsIo 渠道实现（tempmails.io）
 * 无认证 REST：POST /api/temp-mail/generate 建箱（响应 email/token/expires_at），
 * GET /api/temp-mail/inbox/{token} 读信（messages[] 含 from_email/text_body/html_body/attachments）。
 * 邮箱借用 uberip.com 等公共域（10 分钟自动过期）。
 */

const tempmailsIoBase = "https://tempmails.io"

/* tempmailsIoGenerateResponse 建箱响应 */
type tempmailsIoGenerateResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Email         string `json:"email"`
		Token         string `json:"token"`
		ExpiresAt     string `json:"expires_at"`
		MinutesRemain int    `json:"minutes_remaining"`
	} `json:"data"`
}

/* tempmailsIoInboxResponse 收件箱响应 */
type tempmailsIoInboxResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Email      string                   `json:"email"`
		ExpiresAt  string                   `json:"expires_at"`
		MessageCnt int                      `json:"message_count"`
		Messages   []map[string]interface{} `json:"messages"`
	} `json:"data"`
}

/*
 * TempmailsIoGenerate 创建 10 分钟临时邮箱
 * POST /api/temp-mail/generate（空 body）
 */
func TempmailsIoGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest("POST", tempmailsIoBase+"/api/temp-mail/generate", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())

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
		return nil, fmt.Errorf("tempmails-io generate: http %d", resp.StatusCode)
	}

	var data tempmailsIoGenerateResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	if !data.Success || data.Data.Email == "" || data.Data.Token == "" {
		return nil, fmt.Errorf("tempmails-io generate: missing email or token")
	}

	return &CreatedMailbox{
		Channel:   "tempmails-io",
		Email:     data.Data.Email,
		Token:     data.Data.Token,
		ExpiresAt: data.Data.ExpiresAt,
	}, nil
}

/*
 * TempmailsIoGetEmails 读取收件箱
 * 注意：必须先 POST /api/temp-mail/fetch-emails/{token} 触发平台对上游
 * 信箱（uberip.com 域，mail.tm 别名）的主动同步，其后 GET /api/temp-mail/inbox/{token}
 * 才能读到新邮件。只轮询 inbox 会永远为空（实测 poll10 全 0，fetch 后 1 次即到）。
 */
func TempmailsIoGetEmails(email, token string) ([]NormEmail, error) {
	token = strings.TrimSpace(token)
	// 1) 触发同步（失败不致命，仍尝试静态读）
	fetchReq, err := http.NewRequest("POST", tempmailsIoBase+"/api/temp-mail/fetch-emails/"+token, nil)
	if err == nil {
		fetchReq.Header.Set("Accept", "application/json")
		fetchReq.Header.Set("User-Agent", GetCurrentUA())
		if fresp, ferr := HTTPClient().Do(fetchReq); ferr == nil {
			io.Copy(io.Discard, fresp.Body)
			fresp.Body.Close()
		}
	}

	// 2) 读静态收件箱
	req, err := http.NewRequest("GET", tempmailsIoBase+"/api/temp-mail/inbox/"+token, nil)
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tempmails-io inbox: http %d", resp.StatusCode)
	}

	var data tempmailsIoInboxResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}

	out := make([]NormEmail, 0, len(data.Data.Messages))
	for _, m := range data.Data.Messages {
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		// 直接映射字段；attachs 若存在则透传
		flat["from"] = m["from_email"]
		flat["to"] = email
		flat["text"] = m["text_body"]
		flat["html"] = m["html_body"]
		flat["date"] = m["received_at"]
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}

/*
 * TempmailsIoDomainsSnapshot 从公共 MCP 向导/页面试探当前活跃域名池（前端旁挂正则）
 * 仅作为域名可用性的批注，不影响主建箱流程。
 */
func TempmailsIoDomainsSnapshot() []string {
	const pageURL = "https://tempmails.io/api/temp-mail/config"
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out []string
	re := regexp.MustCompile(`"domain"\s*:\s*"([^"]+)"`)
	for _, m := range re.FindAllStringSubmatch(string(body), -1) {
		out = append(out, m[1])
	}
	return out
}
