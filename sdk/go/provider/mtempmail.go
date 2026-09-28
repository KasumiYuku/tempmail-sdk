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
 * Mtempmail 渠道实现（mtempmail.com，公共 key 认证）
 * 建箱: POST /api/emails/{apiKey}（body {}）→
 *   {"status":true,"data":{"email":"xxx@domain","domain":"..","ip":"..",
 *    "fingerprint":"..","expire_at":"..","created_at":"..","id":213000,"email_token":"..."}}
 * 读信: GET /api/messages/{apiKey}/{email} →
 *   {"status":true,"mailbox":"..","email_token":"..","messages":[]}
 *   消息列表元素实测为 map[string]interface{}（示例：
 *   {"to":[{..}],"body":[{content_type:"text/html",value:".."}],
 *    "created_at":"2026-01-45 45:45:45","id":123,
 *    "from":[{"full":"Sender <a@b.com>"}],"subject":"..","flags":[..]，
 *    mailgun 入站 webhook 风格}）。
 * 域名: GET /api/domains/{apiKey}/all → {"status":true,"data":{"domains":[...]}}
 * 邮箱 24 小时有效（建箱响应过期时间为北京时间，标注不一致，以服务端为准）。
 */

const mtempmailBase = "https://mtempmail.com"

/* mtempmailPublicKey 公共固定 API key（mtempmail.com 官方提供） */
const mtempmailPublicKey = "pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw"

/* mtempmailCreateResponse 建箱响应 */
type mtempmailCreateResponse struct {
	Status bool `json:"status"`
	Data   struct {
		Email       string `json:"email"`
		Domain      string `json:"domain"`
		Fingerprint string `json:"fingerprint"`
		ExpireAt    string `json:"expire_at"`
		CreatedAt   string `json:"created_at"`
		ID          int64  `json:"id"`
		EmailToken  string `json:"email_token"`
	} `json:"data"`
}

/* mtempmailMessagesResponse 读信响应 */
type mtempmailMessagesResponse struct {
	Status     bool                     `json:"status"`
	Mailbox    string                   `json:"mailbox"`
	EmailToken string                   `json:"email_token"`
	Messages   []map[string]interface{} `json:"messages"`
}

/* mtempmailBulletRe 剥离后台拼接的 "• " 前缀（或，实收分隔符） */
var mtempmailBulletRe = regexp.MustCompile(`^[•·]+\s*`)

/* mtempmailSubject 清洗主题前导分隔符（后台可能以 "• " 开头拼接微件） */
func mtempmailSubject(s string) string {
	return mtempmailBulletRe.ReplaceAllString(strings.TrimSpace(s), "")
}

/* mtempmailBodyText 拼接正文纯文本（body[].value 按序） */
func mtempmailBodyText(parts []map[string]interface{}) string {
	var b strings.Builder
	for _, p := range parts {
		if v, ok := p["value"].(string); ok {
			b.WriteString(v)
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

/* mtempmailBodyHTML 提取首个 text/html 段 */
func mtempmailBodyHTML(parts []map[string]interface{}) string {
	for _, p := range parts {
		ct, _ := p["content_type"].(string)
		if ct == "text/html" {
			if v, ok := p["value"].(string); ok {
				return v
			}
		}
	}
	return ""
}

/*
 * MtempmailGenerate 创建 mtempmail 临时邮箱
 * POST /api/emails/{apiKey}（空 JSON body）
 */
func MtempmailGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest("POST", mtempmailBase+"/api/emails/"+mtempmailPublicKey, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", getCurrentUA())

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
		return nil, fmt.Errorf("mtempmail create: http %d", resp.StatusCode)
	}

	var data mtempmailCreateResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if !data.Status || data.Data.Email == "" {
		return nil, fmt.Errorf("mtempmail create: 响应缺少邮箱")
	}

	return &CreatedMailbox{
		Channel:   "mtempmail",
		Email:     data.Data.Email,
		Token:     data.Data.EmailToken,
		ExpiresAt: data.Data.ExpireAt,
		CreatedAt: data.Data.CreatedAt,
	}, nil
}

/*
 * MtempmailGetEmails 读取 mtempmail 收件箱
 * GET /api/messages/{apiKey}/{email}；token 元数据仅为校验，不参与请求。
 */
func MtempmailGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("mtempmail: 邮箱为空")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("mtempmail: token 为空")
	}

	req, err := http.NewRequest("GET", mtempmailBase+"/api/messages/"+mtempmailPublicKey+"/"+email, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", getCurrentUA())

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
		return nil, fmt.Errorf("mtempmail inbox: http %d", resp.StatusCode)
	}

	var data mtempmailMessagesResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}

	out := make([]NormEmail, 0, len(data.Messages))
	for _, m := range data.Messages {
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		flat["to"] = email
		if subj, ok := m["subject"].(string); ok {
			flat["subject"] = mtempmailSubject(subj)
		}
		if bodyArr, ok := m["body"].([]interface{}); ok {
			parts := make([]map[string]interface{}, 0, len(bodyArr))
			for _, seg := range bodyArr {
				if pm, ok := seg.(map[string]interface{}); ok {
					parts = append(parts, pm)
				}
			}
			if text := mtempmailBodyText(parts); text != "" {
				flat["text"] = text
			}
			if htmlPart := mtempmailBodyHTML(parts); htmlPart != "" {
				flat["html"] = htmlPart
			}
		}
		flat["date"] = m["created_at"]
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
