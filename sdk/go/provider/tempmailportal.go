package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Tempmailportal 渠道实现（api.tempmailportal.com）
 * POST /api/v2/inbox 建箱（body {}，响应 address/token/private/expiresAt/retentionMs，token 为 p2 前缀），
 * GET /api/messages 读信（Header Authorization: Bearer <token>），
 * GET /api/messages/{id} 取单封详情（Bearer），GET /api/domains 返回收信域名池（Bearer）。
 * 读信端点当前仅实测空列表，单封 id 结构与列表元素字段按多候选归一。
 */

const tempmailportalBase = "https://api.tempmailportal.com"

/* tempmailportalGenerateResponse 建箱响应 */
type tempmailportalGenerateResponse struct {
	Address     string `json:"address"`
	Token       string `json:"token"`
	Private     bool   `json:"private"`
	ExpiresAt   string `json:"expiresAt"`
	RetentionMs int64  `json:"retentionMs"`
}

/* tempmailportalAuthHeaders 设置 tempmailportal 请求的通用请求头 */
func tempmailportalAuthHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

/*
 * TempmailportalGenerate 创建 tempmailportal 临时邮箱
 * POST /api/v2/inbox（空 JSON body）返回 address 与 token
 */
func TempmailportalGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest(http.MethodPost, tempmailportalBase+"/api/v2/inbox", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	tempmailportalAuthHeaders(req, "")

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
		return nil, fmt.Errorf("tempmailportal: 创建邮箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data tempmailportalGenerateResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("tempmailportal: 解析创建响应失败: %w", err)
	}
	if data.Address == "" || data.Token == "" {
		return nil, fmt.Errorf("tempmailportal: 创建邮箱响应缺少必要字段: %s", string(body))
	}

	return &CreatedMailbox{
		Channel:   "tempmailportal",
		Email:     data.Address,
		Token:     data.Token,
		ExpiresAt: data.ExpiresAt,
	}, nil
}

/*
 * TempmailportalGetEmails 获取 tempmailportal 邮件列表
 * 流程：GET /api/messages 取列表，对每个元素按 id 逐封 GET /api/messages/{id} 合并详情，
 * 详情失败时以列表摘要归一。单封 id 结构实测未取到样例（/api/messages/{id} 仅见空列表），
 * 列表元素已按多候选归一，遇真实结构差异时以 API 实际返回为准。
 * @param email - 邮箱地址
 * @param token - 建箱返回的 p2 前缀认证令牌
 */
func TempmailportalGetEmails(email, token string) ([]NormEmail, error) {
	req, err := http.NewRequest(http.MethodGet, tempmailportalBase+"/api/messages", nil)
	if err != nil {
		return nil, err
	}
	tempmailportalAuthHeaders(req, token)

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
		return nil, fmt.Errorf("tempmailportal: 获取邮件列表失败 http %d: %s", resp.StatusCode, string(body))
	}

	var list []map[string]interface{}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("tempmailportal: 解析邮件列表失败: %w", err)
	}

	out := make([]NormEmail, 0, len(list))
	for _, m := range list {
		id := messageIDOf(m)
		if id == "" {
			out = append(out, NormalizeMap(m, email))
			continue
		}
		detail, err := tempmailportalGetDetail(token, id)
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
 * tempmailportalGetDetail 获取 tempmailportal 单封邮件详情
 * 列表元素若含嵌套 body/from 对象会经 getStr 的 %v 序列化提取
 */
func tempmailportalGetDetail(token, messageID string) (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, tempmailportalBase+"/api/messages/"+messageID, nil)
	if err != nil {
		return nil, err
	}
	tempmailportalAuthHeaders(req, token)

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
		return nil, fmt.Errorf("tempmailportal: 获取邮件详情失败 http %d: %s", resp.StatusCode, string(body))
	}

	var detail map[string]interface{}
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("tempmailportal: 解析邮件详情失败: %w", err)
	}
	return detail, nil
}
