package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * NowtempMail 渠道实现（nowtempmail.com）
 * POST /mailbox 建箱（无 body 或 {}，响应 token（JWT）/mailbox，域为 allogebra.com 等轮换域），
 * GET /messages 读信列表（Header Authorization: Bearer <token>，响应 {"messages":[...]}），
 * GET /message/{id} 取单封详情（Bearer）。
 * 前端字段映射为 from（"Name <email>" 或裸地址）/subject/timestamp/text/html/attachments；详情响应其余字段与列表字段同源，均按多候选归一。
 * 实测列表为空时返回 {"messages":[]}。
 */

const nowtempmailBase = "https://nowtempmail.com"

/* nowtempmailGenerateResponse 建箱响应 */
type nowtempmailGenerateResponse struct {
	Token   string `json:"token"`
	Mailbox string `json:"mailbox"`
}

/* nowtempmailMessagesResponse 邮件列表响应 */
type nowtempmailMessagesResponse struct {
	Messages []map[string]interface{} `json:"messages"`
}

/* nowtempmailAuthHeaders 设置 nowtempmail 请求的通用请求头 */
func nowtempmailAuthHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

/*
 * NowtempMailGenerate 创建 nowtempmail.com 临时邮箱
 * POST /mailbox（空 JSON body）返回 token（JWT）与 mailbox 地址；实测空 body 与 {"name":""} 均可用
 */
func NowtempMailGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest(http.MethodPost, nowtempmailBase+"/mailbox", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	nowtempmailAuthHeaders(req, "")

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
		return nil, fmt.Errorf("nowtempmail: 创建邮箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data nowtempmailGenerateResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("nowtempmail: 解析创建响应失败: %w", err)
	}
	if data.Token == "" || data.Mailbox == "" || !strings.Contains(data.Mailbox, "@") {
		return nil, fmt.Errorf("nowtempmail: 创建邮箱响应缺少必要字段: %s", string(body))
	}

	return &CreatedMailbox{
		Channel: "nowtempmail",
		Email:   data.Mailbox,
		Token:   data.Token,
	}, nil
}

/*
 * NowtempMailGetEmails 获取 nowtempmail.com 邮件列表
 * 流程：GET /messages 取列表（{"messages":[...]}），对每个元素按 id 逐封 GET /message/{id}
 * 合并详情；详情失败时以列表摘要归一。列表元素按多候选字段归一。
 * @param email - 邮箱地址
 * @param token - 建箱返回的 JWT 认证令牌
 */
func NowtempMailGetEmails(email, token string) ([]NormEmail, error) {
	req, err := http.NewRequest(http.MethodGet, nowtempmailBase+"/messages", nil)
	if err != nil {
		return nil, err
	}
	nowtempmailAuthHeaders(req, token)

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
		return nil, fmt.Errorf("nowtempmail: 获取邮件列表失败 http %d: %s", resp.StatusCode, string(body))
	}

	var listResp nowtempmailMessagesResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, fmt.Errorf("nowtempmail: 解析邮件列表失败: %w", err)
	}

	out := make([]NormEmail, 0, len(listResp.Messages))
	for _, m := range listResp.Messages {
		id := messageIDOf(m)
		if id == "" {
			out = append(out, NormalizeMap(m, email))
			continue
		}
		detail, err := nowtempmailGetDetail(token, id)
		if err != nil {
			/* 详情失败时回退为列表摘要 */
			out = append(out, NormalizeMap(m, email))
			continue
		}
		for k, v := range detail {
			if _, ok := m[k]; !ok {
				m[k] = v
			}
		}
		out = append(out, NormalizeMap(m, email))
	}
	return out, nil
}

/*
 * nowtempmailGetDetail 获取 nowtempmail.com 单封邮件详情
 * GET /message/{id}（Bearer token），响应为单封对象，含 text/html 等完整字段
 */
func nowtempmailGetDetail(token, messageID string) (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, nowtempmailBase+"/message/"+path.Base(messageID), nil)
	if err != nil {
		return nil, err
	}
	nowtempmailAuthHeaders(req, token)

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
		return nil, fmt.Errorf("nowtempmail: 获取邮件详情失败 http %d: %s", resp.StatusCode, string(body))
	}

	var detail map[string]interface{}
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("nowtempmail: 解析邮件详情失败: %w", err)
	}
	return detail, nil
}
