package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	http "github.com/bogdanfinn/fhttp"
)

/* tempgo.email 临时邮箱服务商
 * 流程：POST /api/generate 创建邮箱（无 body、无 Content-Type），返回 email + mailbox_id
 *       GET /api/inbox?email={email}&mailbox_id={mailbox_id} 获取邮件列表
 *       GET /api/message/{id}?mailbox_id={mailbox_id} 获取单封邮件详情
 * token 存储为 mailbox_id，generate 响应中的 token 字段仅平台内部使用，读信一律使用 mailbox_id
 */

const tempgoEmailBaseURL = "https://tempgo.email"

/* tempgoEmailCreateResponse 创建邮箱接口响应 */
type tempgoEmailCreateResponse struct {
	Email     string `json:"email"`
	ExpiresAt string `json:"expires_at"`
	ExpiresIn int    `json:"expires_in"`
	MailboxID string `json:"mailbox_id"`
	Token     string `json:"token"`
}

/* tempgoEmailInboxResponse 获取邮件列表接口响应 */
type tempgoEmailInboxResponse struct {
	Email     string            `json:"email"`
	ExpiresIn int               `json:"expires_in"`
	Messages  []json.RawMessage `json:"messages"`
}

/* TempgoEmailGenerate 创建 tempgo.email 临时邮箱
 * API: POST /api/generate（无 body，不发送 Content-Type header）
 * 返回邮箱地址，token 为 mailbox_id
 */
func TempgoEmailGenerate() (*CreatedMailbox, error) {
	client := HTTPClient()

	req, err := http.NewRequest("POST", tempgoEmailBaseURL+"/api/generate", nil)
	if err != nil {
		return nil, fmt.Errorf("tempgo-email: 创建请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	ua := getCurrentUA()
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempgo-email: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckHTTPStatus(resp, "tempgo-email create mailbox"); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tempgo-email: 读取响应失败: %w", err)
	}

	var result tempgoEmailCreateResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("tempgo-email: 解析响应失败: %w", err)
	}

	if result.Email == "" || result.MailboxID == "" {
		return nil, fmt.Errorf("tempgo-email: 响应缺少必要字段（email 或 mailbox_id）")
	}

	return &CreatedMailbox{
		Channel:   "tempgo-email",
		Email:     result.Email,
		Token:     result.MailboxID,
		ExpiresAt: result.ExpiresAt,
	}, nil
}

/* tempgoEmailFetchDetail 获取单封邮件详情并解析为原始 JSON map
 * API: GET /api/message/{id}?mailbox_id={mailbox_id}
 * 列表端点不返回正文，正文只能从详情端点取得；读取失败时返回原始错误
 */
func tempgoEmailFetchDetail(mailboxID, id string) (map[string]interface{}, error) {
	u := fmt.Sprintf("%s/api/message/%s?mailbox_id=%s",
		tempgoEmailBaseURL,
		url.PathEscape(id),
		url.QueryEscape(mailboxID),
	)

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("tempgo-email: 创建详情请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	ua := getCurrentUA()
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempgo-email: 获取邮件详情请求失败: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckHTTPStatus(resp, "tempgo-email get message"); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tempgo-email: 读取邮件详情响应失败: %w", err)
	}

	var detail map[string]interface{}
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("tempgo-email: 解析邮件详情失败: %w", err)
	}
	return detail, nil
}

/* TempgoEmailGetEmails 获取 tempgo.email 邮件列表
 * API: GET /api/inbox?email={email}&mailbox_id={mailbox_id}
 * token 即 mailbox_id
 * 列表不带正文，逐条请求详情端点，详情字段覆盖列表项后统一归一化
 */
func TempgoEmailGetEmails(token, email string) ([]NormEmail, error) {
	if token == "" {
		return nil, fmt.Errorf("tempgo-email: mailbox_id（token）为空")
	}
	if email == "" {
		return nil, fmt.Errorf("tempgo-email: 邮箱地址为空")
	}

	client := HTTPClient()

	u := fmt.Sprintf("%s/api/inbox?email=%s&mailbox_id=%s",
		tempgoEmailBaseURL,
		url.QueryEscape(email),
		url.QueryEscape(token),
	)

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("tempgo-email: 创建获取邮件请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	ua := getCurrentUA()
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempgo-email: 获取邮件请求失败: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckHTTPStatus(resp, "tempgo-email get inbox"); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tempgo-email: 读取邮件响应失败: %w", err)
	}

	var result tempgoEmailInboxResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("tempgo-email: 解析邮件列表失败: %w", err)
	}

	if len(result.Messages) == 0 {
		return []NormEmail{}, nil
	}

	/* 逐条拉取详情端点，详情字段覆盖列表项，补齐正文等缺失字段 */
	raw := make([]json.RawMessage, 0, len(result.Messages))
	for _, item := range result.Messages {
		m := map[string]interface{}{}
		if err := json.Unmarshal(item, &m); err != nil {
			return nil, fmt.Errorf("tempgo-email: 解析邮件列表项失败: %w", err)
		}
		id := fmt.Sprintf("%v", m["id"])
		if detail, err := tempgoEmailFetchDetail(token, id); err == nil {
			for k, v := range detail {
				m[k] = v
			}
		}
		merged, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("tempgo-email: 序列化合并邮件失败: %w", err)
		}
		raw = append(raw, merged)
	}

	return NormalizeRawMessages(raw, email)
}
