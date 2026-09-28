package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Huskmail 渠道实现（huskmail.xyz）
 * POST /api/v1/accounts 建箱（body {}，响应 id/address/password/token/expiresAt/tier，token 为 JWT），
 * GET /v1/messages 读信（Header Authorization: Bearer <token>，响应 {"messages":[...]}），
 * GET /v1/messages/{id} 取单封详情（Bearer）。
 * 收信域固定为 @huskmail.xyz（huskmail.space 无 MX）。
 */

const huskmailBase = "https://api.huskmail.space"

/* huskmailGenerateResponse 建箱响应 */
type huskmailGenerateResponse struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	Password  string `json:"password"`
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"`
	Tier      string `json:"tier"`
}

/* huskmailMessagesResponse 邮件列表响应 */
type huskmailMessagesResponse struct {
	Messages []map[string]interface{} `json:"messages"`
}

/* huskmailAuthHeaders 设置 huskmail（huskmail.space API）请求的通用请求头 */
func huskmailAuthHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

/*
 * HuskmailGenerate 创建 huskmail（@huskmail.xyz）临时邮箱
 * POST /v1/accounts（空 JSON body）返回 address 与 JWT token
 */
func HuskmailGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest(http.MethodPost, huskmailBase+"/v1/accounts", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	huskmailAuthHeaders(req, "")

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
		return nil, fmt.Errorf("huskmail: 创建邮箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data huskmailGenerateResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("huskmail: 解析创建响应失败: %w", err)
	}
	if data.Address == "" || data.Token == "" {
		return nil, fmt.Errorf("huskmail: 创建邮箱响应缺少必要字段: %s", string(body))
	}

	return &CreatedMailbox{
		Channel:   "huskmail",
		Email:     data.Address,
		Token:     data.Token,
		ExpiresAt: data.ExpiresAt,
	}, nil
}

/*
 * HuskmailGetEmails 获取 huskmail 邮件列表
 * 流程：GET /v1/messages 取列表（{"messages":[...]}），对每个元素按 id 逐封 GET /v1/messages/{id}
 * 合并详情；详情失败时以列表摘要归一。列表元素按多候选字段归一。
 * @param email - 邮箱地址（@huskmail.xyz）
 * @param token - 建箱返回的 JWT 认证令牌
 */
func HuskmailGetEmails(email, token string) ([]NormEmail, error) {
	req, err := http.NewRequest(http.MethodGet, huskmailBase+"/v1/messages", nil)
	if err != nil {
		return nil, err
	}
	huskmailAuthHeaders(req, token)

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
		return nil, fmt.Errorf("huskmail: 获取邮件列表失败 http %d: %s", resp.StatusCode, string(body))
	}

	var listResp huskmailMessagesResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		return nil, fmt.Errorf("huskmail: 解析邮件列表失败: %w", err)
	}

	out := make([]NormEmail, 0, len(listResp.Messages))
	for _, m := range listResp.Messages {
		id := messageIDOf(m)
		if id == "" {
			out = append(out, NormalizeMap(m, email))
			continue
		}
		detail, err := huskmailGetDetail(token, id)
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
 * huskmailGetDetail 获取 huskmail 单封邮件详情
 * 列表元素若含嵌套 body/from 对象会经 getStr 的 %v 序列化提取
 */
func huskmailGetDetail(token, messageID string) (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, huskmailBase+"/v1/messages/"+messageID, nil)
	if err != nil {
		return nil, err
	}
	huskmailAuthHeaders(req, token)

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
		return nil, fmt.Errorf("huskmail: 获取邮件详情失败 http %d: %s", resp.StatusCode, string(body))
	}

	var detail map[string]interface{}
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("huskmail: 解析邮件详情失败: %w", err)
	}
	return detail, nil
}
