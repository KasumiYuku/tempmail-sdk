package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * ClawdEmail 渠道实现（clawdemail.com，API 域 api.clawdemail.com）
 * POST /register 建箱（无验证码，可选 JSON body {"name":"xxx"}，响应 success/email/token），
 * GET /inbox 读信列表（Header Authorization: Bearer <token>，响应 success/email/count/unread/emails[]），
 * GET /email/{id} 取单封详情（Bearer，响应 success/email{from_addr/subject/body_text/received_at}/code/links）。
 * 信件保留 30 分钟，仅接收不发送，正文仅纯文本。信件字段按 from_addr/subject/body_text/received_at/read 归一。
 */

const clawdemailBase = "https://api.clawdemail.com"

/* clawdemailRegisterResponse 注册响应 */
type clawdemailRegisterResponse struct {
	Success bool   `json:"success"`
	Email   string `json:"email"`
	Token   string `json:"token"`
	Error   string `json:"error"`
}

/* clawdemailInboxResponse 收件箱响应 */
type clawdemailInboxResponse struct {
	Success bool                     `json:"success"`
	Email   string                   `json:"email"`
	Count   int                      `json:"count"`
	Unread  int                      `json:"unread"`
	Emails  []map[string]interface{} `json:"emails"`
	Error   string                   `json:"error"`
}

/* clawdemailAuthHeaders 设置 clawdemail 请求的通用请求头 */
func clawdemailAuthHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

/*
 * ClawdEmailGenerate 创建 clawdemail.com 临时邮箱
 * POST /register（空 JSON body）返回 email 与 token
 * @param name - 可选的自定义名称，为空时服务端随机生成
 */
func ClawdEmailGenerate(name string) (*CreatedMailbox, error) {
	payload, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, clawdemailBase+"/register", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	clawdemailAuthHeaders(req, "")

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
		return nil, fmt.Errorf("clawdemail: 创建邮箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data clawdemailRegisterResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("clawdemail: 解析创建响应失败: %w", err)
	}
	if data.Email == "" || data.Token == "" || !strings.Contains(data.Email, "@") {
		return nil, fmt.Errorf("clawdemail: 创建邮箱响应缺少必要字段: %s", string(body))
	}

	return &CreatedMailbox{
		Channel: "clawdemail",
		Email:   data.Email,
		Token:   data.Token,
	}, nil
}

/*
 * ClawdEmailGetEmails 获取 clawdemail.com 邮件列表
 * 流程：GET /inbox?limit=50 取列表（{"emails":[...]}），对每个元素按 id 逐封 GET /email/{id}
 * 合并详情；详情失败时以列表摘要归一。列表元素按多候选字段归一。
 * @param email - 邮箱地址
 * @param token - 注册返回的认证令牌
 */
func ClawdEmailGetEmails(email, token string) ([]NormEmail, error) {
	u := clawdemailBase + "/inbox?limit=50"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	clawdemailAuthHeaders(req, token)

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
		return nil, fmt.Errorf("clawdemail: 获取邮件列表失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data clawdemailInboxResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("clawdemail: 解析邮件列表失败: %w", err)
	}
	if !data.Success {
		return nil, fmt.Errorf("clawdemail: 读取收件箱失败: %s", data.Error)
	}

	out := make([]NormEmail, 0, len(data.Emails))
	for _, m := range data.Emails {
		id := messageIDOf(m)
		if id == "" {
			out = append(out, NormalizeMap(m, email))
			continue
		}
		detail, err := clawdemailGetDetail(token, id)
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
 * clawdemailGetDetail 获取 clawdemail.com 单封邮件详情
 * GET /email/{id}（Bearer token），响应含 email 嵌套对象（from_addr/body_text/received_at）
 */
func clawdemailGetDetail(token, messageID string) (map[string]interface{}, error) {
	req, err := http.NewRequest(http.MethodGet, clawdemailBase+"/email/"+url.PathEscape(path.Base(messageID)), nil)
	if err != nil {
		return nil, err
	}
	clawdemailAuthHeaders(req, token)

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
		return nil, fmt.Errorf("clawdemail: 获取邮件详情失败 http %d: %s", resp.StatusCode, string(body))
	}

	var detail map[string]interface{}
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("clawdemail: 解析邮件详情失败: %w", err)
	}
	/* 详情为 {success,email:{...},code,links} 时提升嵌套 email 对象 */
	if nested, ok := detail["email"].(map[string]interface{}); ok {
		return nested, nil
	}
	return detail, nil
}
