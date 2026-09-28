package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * ShitpostEmail 渠道实现（shitpost.email 公共实例）
 * 无认证 REST：POST /api/create 建箱（username/domain/ttl → email/token/type/expires），
 * GET /api/inbox?email=&token= 读信（messages[] 含 from/fromName/subject/text/html/date）。
 * 域名池：shitpost.email / letsfuckingpiss.party（克隆自 shamu4life/throwaway-email 公共实例）。
 */

const shitpostEmailBase = "https://shitpost.email"

/* shitpostEmailCreateResponse 建箱响应 */
type shitpostEmailCreateResponse struct {
	Email   string `json:"email"`
	Token   string `json:"token"`
	Type    string `json:"type"`
	Expires int64  `json:"expires"`
}

/* shitpostEmailInboxResponse 收件箱响应 */
type shitpostEmailInboxResponse struct {
	Email    string                   `json:"email"`
	Messages []map[string]interface{} `json:"messages"`
	Count    int                      `json:"count"`
}

var shitpostEmailDomains = []string{"shitpost.email", "letsfuckingpiss.party"}

func shitpostEmailLocal() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	b.WriteString("sdk")
	for i := 0; i < 10; i++ {
		b.WriteByte(chars[rand.Intn(len(chars))])
	}
	return b.String()
}

/*
 * ShitpostEmailGenerate 创建临时邮箱
 * POST /api/create body {username,domain,ttl}
 */
func ShitpostEmailGenerate() (*CreatedMailbox, error) {
	dom := shitpostEmailDomains[rand.Intn(len(shitpostEmailDomains))]
	body, _ := json.Marshal(map[string]interface{}{
		"username": shitpostEmailLocal(),
		"domain":   dom,
		"ttl":      3600,
	})
	req, err := http.NewRequest("POST", shitpostEmailBase+"/api/create", bytes.NewReader(body))
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

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("shitpost-email create: http %d", resp.StatusCode)
	}

	var data shitpostEmailCreateResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.Email == "" || data.Token == "" {
		return nil, fmt.Errorf("shitpost-email create: missing email or token")
	}

	return &CreatedMailbox{
		Channel:   "shitpost-email",
		Email:     data.Email,
		Token:     data.Token,
		ExpiresAt: fmt.Sprintf("%d", data.Expires),
	}, nil
}

/*
 * ShitpostEmailGetEmails 读取收件箱
 * GET /api/inbox?email=&token=
 */
func ShitpostEmailGetEmails(email, token string) ([]NormEmail, error) {
	req, err := http.NewRequest("GET", shitpostEmailBase+"/api/inbox?email="+url.QueryEscape(email)+"&token="+url.QueryEscape(token), nil)
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
		return nil, fmt.Errorf("shitpost-email inbox: http %d", resp.StatusCode)
	}

	var data shitpostEmailInboxResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}

	out := make([]NormEmail, 0, len(data.Messages))
	for _, m := range data.Messages {
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		flat["from"] = m["from"]
		flat["to"] = email
		flat["text"] = m["text"]
		flat["html"] = m["html"]
		flat["date"] = m["date"]
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
