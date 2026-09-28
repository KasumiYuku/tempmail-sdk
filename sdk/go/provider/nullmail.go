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
 * Nullmail 渠道实现（nullmail.cc / maildock.store）
 * 无认证 REST：POST /api/emails（空 JSON body）建箱，响应
 * {"address":"...@maildock.store","expiry":"2026-09-27T02:25:28.778Z"}；
 * 读信 GET /api/emails/{address}（URL 编码），响应 {"expiry":"...","emails":[...]}，
 * 列表项只有 id/sender/subject/delivered，正文须逐封二拉 GET /api/emails/{addr}/body/{id}
 * （address URL 编码，@ 编码为 %40 或保留原样均可，实测两者同效；响应 {"body":...}）；
 * 续期 PUT /api/emails/{addr}/extend/1h 暂不接入。
 */

const nullmailBase = "https://www.nullmail.cc"
const nullmailDomain = "maildock.store"

/* nullmailGenerateResponse 建箱响应 */
type nullmailGenerateResponse struct {
	Address string `json:"address"`
	Expiry  string `json:"expiry"`
}

/* nullmailInboxResponse 收件箱响应 */
type nullmailInboxResponse struct {
	Expiry string                   `json:"expiry"`
	Emails []map[string]interface{} `json:"emails"`
}

/* nullmailBodyResponse 单封正文响应 */
type nullmailBodyResponse struct {
	Body string `json:"body"`
}

/*
 * NullmailGenerate 创建临时邮箱
 * POST /api/emails（空 JSON body），token 复用完整地址以便收件箱回查
 */
func NullmailGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest("POST", nullmailBase+"/api/emails", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", nullmailBase)
	req.Header.Set("Referer", nullmailBase+"/")
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
		return nil, fmt.Errorf("nullmail 建箱: http %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var data nullmailGenerateResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	addr := strings.TrimSpace(data.Address)
	if addr == "" {
		return nil, fmt.Errorf("nullmail 建箱: 响应缺少 address 字段")
	}

	return &CreatedMailbox{
		Channel:   "nullmail",
		Email:     addr,
		Token:     addr,
		ExpiresAt: data.Expiry,
	}, nil
}

/*
 * nullmailGetBody 单封正文二拉
 * GET /api/emails/{addr}/body/{id}，响应 {"body":"<完整纯文本正文>"}
 */
func nullmailGetBody(addr string, id interface{}) (string, error) {
	req, err := http.NewRequest("GET", nullmailBase+"/api/emails/"+url.PathEscape(addr)+"/body/"+url.PathEscape(fmt.Sprintf("%v", id)), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", nullmailBase)
	req.Header.Set("Referer", nullmailBase+"/")
	req.Header.Set("User-Agent", GetCurrentUA())

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("nullmail 正文: http %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var data nullmailBodyResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", err
	}
	return data.Body, nil
}

/*
 * NullmailGetEmails 读取收件箱
 * GET /api/emails/{address}（完整地址 URL 编码，实测带 @ 域名编码形式可正确返回），
 * 列表项无正文，逐封二拉 body 端点取纯文本正文；服务端响应无 id 索引，归一化自动以序号兜底
 */
func NullmailGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("nullmail 读信: 邮箱地址为空")
	}

	req, err := http.NewRequest("GET", nullmailBase+"/api/emails/"+url.PathEscape(email), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", nullmailBase)
	req.Header.Set("Referer", nullmailBase+"/")
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
		return nil, fmt.Errorf("nullmail 读信: http %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var data nullmailInboxResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}

	out := make([]NormEmail, 0, len(data.Emails))
	for _, m := range data.Emails {
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		flat["to"] = email
		/* normalizeDate 候选键不含 delivered，显式映射为 date 后归一化 */
		if v, ok := pickStr(m, "delivered"); ok {
			flat["date"] = v
		}
		/* 列表只有 id/sender/subject/delivered，正文逐封二拉 body 端点，失败降级留空不阻断列表 */
		if id, ok := m["id"]; ok {
			if body, err := nullmailGetBody(email, id); err == nil && body != "" {
				flat["text"] = body
			}
		}
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
