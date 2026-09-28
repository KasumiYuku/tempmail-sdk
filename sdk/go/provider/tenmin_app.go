package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * TenminApp 渠道实现（tenmin.app，真实 API 域 api.tenmin.app）
 * 建箱+读信共用 GET /api/inbox/{localpart}（Accept: application/json）。
 * localpart 为随机 6 位小写十六进制串（首访即建箱，mallbox 无显式创建接口）；
 * 响应：{"inboxId":..,"address":"..@tenmin.app","ttl":600,"count":0,"messages":[]}，
 * messages[] 元素字段：id/from/subject/text/html/receivedAt（from 为 {name,address} 对象）。
 * 非 /[0-9a-z.-]+ 的 localpart 会返回 400 invalid_inbox_id；仅支持十六进制域名入箱。
 */

const tenminAppBase = "https://api.tenmin.app"

/* tenminAppInboxResponse 收件箱响应（建箱与读信共用） */
type tenminAppInboxResponse struct {
	InboxID  string                   `json:"inboxId"`
	Address  string                   `json:"address"`
	TTL      int                      `json:"ttl"`
	Count    int                      `json:"count"`
	Messages []map[string]interface{} `json:"messages"`
}

/* tenminAppLocal 生成 6 位小写十六进制随机 localpart */
func tenminAppLocal() string {
	const hexChars = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < 6; i++ {
		b.WriteByte(hexChars[rand.Intn(len(hexChars))])
	}
	return b.String()
}

/*
 * tenminAppFetchInbox 请求 /api/inbox/{localpart}
 * @param localpart 邮箱 localpart
 * @returns 解析后的收件箱响应
 */
func tenminAppFetchInbox(localpart string) (*tenminAppInboxResponse, error) {
	req, err := http.NewRequest("GET", tenminAppBase+"/api/inbox/"+localpart, nil)
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
		return nil, fmt.Errorf("tenmin-app inbox: http %d", resp.StatusCode)
	}

	var data tenminAppInboxResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

/*
 * TenminAppGenerate 创建 tenmin.app 临时邮箱
 * 首次 GET 随机 localpart 即自动建箱（10 分钟 TTL），token 存储 localpart。
 */
func TenminAppGenerate() (*CreatedMailbox, error) {
	local := tenminAppLocal()
	data, err := tenminAppFetchInbox(local)
	if err != nil {
		return nil, fmt.Errorf("tenmin-app: 创建邮箱失败: %w", err)
	}
	if data.Address == "" {
		data.Address = local + "@tenmin.app"
	}
	return &CreatedMailbox{
		Channel:   "tenmin-app",
		Email:     data.Address,
		Token:     local,
		ExpiresAt: time.Now().Add(time.Duration(data.TTL) * time.Second).UTC().Format(time.RFC3339),
	}, nil
}

/*
 * TenminAppGetEmails 读取 tenmin.app 收件箱
 * 复用建箱同一 localpart 轮询；from 为 {name,address} 对象，展开后交归一化处理。
 */
func TenminAppGetEmails(email, token string) ([]NormEmail, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("tenmin-app: token 为空")
	}

	data, err := tenminAppFetchInbox(token)
	if err != nil {
		return nil, err
	}

	out := make([]NormEmail, 0, len(data.Messages))
	for _, m := range data.Messages {
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		/* from 为对象（{name,address}）时拆出地址字段 */
		if fromObj, ok := m["from"].(map[string]interface{}); ok {
			addr, _ := fromObj["address"].(string)
			name, _ := fromObj["name"].(string)
			if addr != "" && name != "" {
				flat["from"] = name + " <" + addr + ">"
			} else if addr != "" {
				flat["from"] = addr
			} else {
				flat["from"] = name
			}
		}
		flat["to"] = email
		flat["text"] = m["text"]
		flat["html"] = m["html"]
		flat["date"] = m["receivedAt"]
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
