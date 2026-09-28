package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Smails 渠道实现（smails.dev）
 * POST /api/mailbox 建箱（无 body 或 {}，响应 address/token），
 * GET /api/mailbox/messages 读信（Header Authorization: Bearer <token>，数组响应），
 * GET /api/mailbox/messages/{id} 取单封详情（Bearer）。
 * 读信端点当前仅实测空列表，单封 id 结构与列表元素字段按 from/subject/text/html/date 多候选归一。
 */

const smailsBase = "https://smails.dev"

/* smailsGenerateResponse 建箱响应 */
type smailsGenerateResponse struct {
	Address string `json:"address"`
	Token   string `json:"token"`
}

/* smailsAuthHeaders 设置 smails.dev 请求的通用请求头 */
func smailsAuthHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

/*
 * SmailsGenerate 创建 smails.dev 临时邮箱
 * POST /api/mailbox（空 JSON body）返回 address 与 token
 */
func SmailsGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest(http.MethodPost, smailsBase+"/api/mailbox", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	smailsAuthHeaders(req, "")

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
		return nil, fmt.Errorf("smails: 创建邮箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data smailsGenerateResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("smails: 解析创建响应失败: %w", err)
	}
	if data.Address == "" || data.Token == "" {
		return nil, fmt.Errorf("smails: 创建邮箱响应缺少必要字段: %s", string(body))
	}

	return &CreatedMailbox{
		Channel: "smails",
		Email:   data.Address,
		Token:   data.Token,
	}, nil
}

/*
 * SmailsGetEmails 获取 smails.dev 邮件列表
 * 流程：GET /api/mailbox/messages 取列表，对每个元素按 id 逐封 GET /api/mailbox/messages/{id}
 * 合并详情；详情失败时以列表摘要归一。列表元素按 from/subject/text/html/date 多候选归一，
 * 遇真实结构差异时以 API 实际返回为准。
 * @param email - 邮箱地址
 * @param token - 建箱返回的认证令牌
 */
func SmailsGetEmails(email, token string) ([]NormEmail, error) {
	req, err := http.NewRequest(http.MethodGet, smailsBase+"/api/mailbox/messages", nil)
	if err != nil {
		return nil, err
	}
	smailsAuthHeaders(req, token)

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
		return nil, fmt.Errorf("smails: 获取邮件列表失败 http %d: %s", resp.StatusCode, string(body))
	}

	var list []map[string]interface{}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("smails: 解析邮件列表失败: %w", err)
	}

	out := make([]NormEmail, 0, len(list))
	for _, m := range list {
		id := messageIDOf(m)
		if id == "" {
			out = append(out, NormalizeMap(m, email))
			continue
		}
		detail, err := smailsGetDetail(token, id)
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
 * smailsGetDetail 获取 smails.dev 单封邮件详情
 * 列表元素若含嵌套 body/from 对象会经 getStr 的 %v 序列化提取
 */
func smailsGetDetail(token, messageID string) (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, smailsBase+"/api/mailbox/messages/"+messageID, nil)
	if err != nil {
		return nil, err
	}
	smailsAuthHeaders(req, token)

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
		return nil, fmt.Errorf("smails: 获取邮件详情失败 http %d: %s", resp.StatusCode, string(body))
	}

	var detail map[string]interface{}
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("smails: 解析邮件详情失败: %w", err)
	}
	return detail, nil
}

/* messageIDOf 从列表元素中提取邮件 ID，候选字段 id/Id/slug/messageId/message_id */
func messageIDOf(m map[string]interface{}) string {
	for _, key := range []string{"id", "Id", "slug", "messageId", "message_id"} {
		if v, ok := m[key]; ok && v != nil {
			switch val := v.(type) {
			case string:
				if strings.TrimSpace(val) != "" {
					return val
				}
			}
		}
	}
	return ""
}
