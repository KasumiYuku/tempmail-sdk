package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

/* mailcat.ai 临时邮箱服务商
 * 流程：POST /mailboxes 创建邮箱（无 body），返回 email + token
 *       GET /inbox 获取邮件列表，需要 Authorization: Bearer {token}
 */

const mailcatAiBaseURL = "https://api.mailcat.ai"

/* mailcatAiCreateResponse 创建邮箱接口响应 */
type mailcatAiCreateResponse struct {
	Data struct {
		Email string `json:"email"`
		Token string `json:"token"`
	} `json:"data"`
}

/* mailcatAiInboxResponse 获取邮件列表接口响应 */
type mailcatAiInboxResponse struct {
	Data []json.RawMessage `json:"data"`
	Meta struct {
		Mailbox    string `json:"mailbox"`
		Unread     int    `json:"unread"`
		Pagination struct {
			Offset     int  `json:"offset"`
			Limit      int  `json:"limit"`
			TotalCount int  `json:"totalCount"`
			HasMore    bool `json:"hasMore"`
		} `json:"pagination"`
	} `json:"meta"`
}

/* mailcatAiEmailDetail 详情响应 data.email 子结构 */
type mailcatAiEmailDetail struct {
	ID         string `json:"id"`
	From       string `json:"from"`
	To         string `json:"to"`
	Subject    string `json:"subject"`
	Text       string `json:"text"`
	HTML       string `json:"html"`
	ReceivedAt string `json:"receivedAt"`
	Size       int64  `json:"size"`
}

/* mailcatAiDetailData 详情响应 data 对象（email / code / links） */
type mailcatAiDetailData struct {
	Email mailcatAiEmailDetail `json:"email"`
	Code  string               `json:"code"`
	Links struct {
		Text string `json:"text"`
	} `json:"links"`
}

/* mailcatAiDetailResponse 邮件详情接口响应（GET /emails/{id}），详情嵌套在 data.email 中 */
type mailcatAiDetailResponse struct {
	Data *mailcatAiDetailData `json:"data"`
}

/* MailcatAiGenerate 创建 mailcat.ai 临时邮箱
 * API: POST /mailboxes（无 body）
 * 返回邮箱地址和 Bearer token
 */
func MailcatAiGenerate() (*CreatedMailbox, error) {
	client := HTTPClient()

	req, err := http.NewRequest("POST", mailcatAiBaseURL+"/mailboxes", nil)
	if err != nil {
		return nil, fmt.Errorf("mailcat-ai: 创建请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	ua := getCurrentUA()
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mailcat-ai: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckHTTPStatus(resp, "mailcat-ai create mailbox"); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mailcat-ai: 读取响应失败: %w", err)
	}

	var result mailcatAiCreateResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("mailcat-ai: 解析响应失败: %w", err)
	}

	if result.Data.Email == "" || result.Data.Token == "" {
		return nil, fmt.Errorf("mailcat-ai: 响应缺少必要字段（email 或 token）")
	}

	return &CreatedMailbox{
		Channel: "mailcat-ai",
		Email:   result.Data.Email,
		Token:   result.Data.Token,
	}, nil
}

/* MailcatAiGetEmails 获取 mailcat.ai 邮件列表
 * API: GET /inbox
 * Headers: Authorization: Bearer {token}
 */
func MailcatAiGetEmails(token, email string) ([]NormEmail, error) {
	if token == "" {
		return nil, fmt.Errorf("mailcat-ai: token 为空")
	}

	client := HTTPClient()

	req, err := http.NewRequest("GET", mailcatAiBaseURL+"/inbox", nil)
	if err != nil {
		return nil, fmt.Errorf("mailcat-ai: 创建获取邮件请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	ua := getCurrentUA()
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mailcat-ai: 获取邮件请求失败: %w", err)
	}
	defer resp.Body.Close()

	if err := CheckHTTPStatus(resp, "mailcat-ai get inbox"); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mailcat-ai: 读取邮件响应失败: %w", err)
	}

	var result mailcatAiInboxResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("mailcat-ai: 解析邮件列表失败: %w", err)
	}

	if len(result.Data) == 0 {
		return []NormEmail{}, nil
	}

	/* 逐封拉取详情并小并发合并展开 */
	type flattenedDetail struct {
		raw map[string]interface{}
		err error
	}

	auth := "Bearer " + strings.TrimSpace(token)
	flatResults := make([]flattenedDetail, len(result.Data))
	var wg sync.WaitGroup

	for i, rawItem := range result.Data {
		wg.Add(1)
		go func(idx int, rawList json.RawMessage) {
			defer wg.Done()

			current := mailcatAiFlattenInboxItem(rawList, email)
			id := mailcatAiGetStr(current, "id", "_id", "eid", "mailboxId", "messageId", "mail_id")
			if id == "" {
				flatResults[idx] = flattenedDetail{raw: current}
				return
			}

			flatResults[idx] = flattenedDetail{raw: mailcatAiFetchDetail(client, auth, id, current)}
		}(i, rawItem)
	}

	wg.Wait()

	raws := make([]json.RawMessage, 0, len(flatResults))
	for _, r := range flatResults {
		if r.err != nil {
			return nil, r.err
		}
		encoded, err := json.Marshal(r.raw)
		if err != nil {
			return nil, fmt.Errorf("mailcat-ai: 构建归一化输入失败: %w", err)
		}
		raws = append(raws, encoded)
	}

	return NormalizeRawMessages(raws, email)
}

/* mailcatAiFlattenInboxItem 将列表项转为归一化 map，并解出邮件 id
 * 列表项可能是 {"_id": …} 或 {"id": …}，两种键都保留在输出中 */
func mailcatAiFlattenInboxItem(rawList json.RawMessage, recipientEmail string) map[string]interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal(rawList, &m); err != nil {
		m = map[string]interface{}{}
	}
	if id, ok := m["_id"]; ok {
		m["id"] = id
	}
	if m["to"] == nil || m["to"] == "" {
		m["to"] = recipientEmail
	}
	return m
}

/* mailcatAiGetStr 按候选键顺序提取字符串字段 */
func mailcatAiGetStr(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key]; ok && v != nil {
			switch val := v.(type) {
			case string:
				if strings.TrimSpace(val) != "" {
					return val
				}
			case float64:
				return fmt.Sprintf("%d", int64(val))
			}
		}
	}
	return ""
}

/* mailcatAiFetchDetail 调用 GET /emails/{id} 拉取正文，合并进归一化输入
 * 详情内容位于 data.email，code 为验证码候选；详情失败时不中断整体流程 */
func mailcatAiFetchDetail(client tls_client.HttpClient, auth, id string, current map[string]interface{}) map[string]interface{} {
	req, err := http.NewRequest("GET", mailcatAiBaseURL+"/emails/"+id, nil)
	if err != nil {
		return current
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", auth)
	ua := getCurrentUA()
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	resp, err := client.Do(req)
	if err != nil {
		return current
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return current
	}

	if resp.StatusCode >= 400 {
		return current
	}

	var detail mailcatAiDetailResponse
	if err := json.Unmarshal(body, &detail); err != nil {
		return current
	}
	if detail.Data == nil {
		return current
	}

	if detail.Data.Email.Text != "" {
		current["text"] = detail.Data.Email.Text
	}
	if detail.Data.Email.HTML != "" {
		current["html"] = detail.Data.Email.HTML
	}
	if detail.Data.Email.Subject != "" {
		current["subject"] = detail.Data.Email.Subject
	}
	if detail.Data.Email.ReceivedAt != "" {
		current["receivedAt"] = detail.Data.Email.ReceivedAt
	}
	if detail.Data.Email.From != "" && strings.TrimSpace(mailcatAiGetStr(current, "from")) == "" {
		current["from"] = detail.Data.Email.From
	}
	if detail.Data.Code != "" && mailcatAiGetStr(current, "code") == "" {
		current["code"] = detail.Data.Code
	}
	if detail.Data.Links.Text != "" {
		current["links_html_url"] = detail.Data.Links.Text
	}

	return current
}
