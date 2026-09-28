package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

const (
	neighboursShBase   = "https://neighbours.sh/api/v1"
	neighboursShDomain = "neighbours.sh"
)

/* neighboursShListResponse 收件箱列表响应：{"success":true,"data":[{"uid":N,...}],"count":N} */
type neighboursShListResponse struct {
	Success bool `json:"success"`
	Data    []struct {
		UID     *int64 `json:"uid"`
		Subject string `json:"subject"`
		Date    string `json:"date"`
	} `json:"data"`
}

/*
neighboursShDetailResponse 单邮件详情响应：{"success":true,"data":{...}}

	平台 text 稳定为字符串，html 实测可能为布尔 false，两种字段均用 any 承接
*/
type neighboursShDetailResponse struct {
	Success bool `json:"success"`
	Data    *struct {
		UID  *int64 `json:"uid"`
		From struct {
			Value []struct {
				Address string `json:"address"`
				Name    string `json:"name"`
			} `json:"value"`
			Text string `json:"text"`
		} `json:"from"`
		To struct {
			Text string `json:"text"`
		} `json:"to"`
		Subject     string        `json:"subject"`
		Text        any           `json:"text"`
		HTML        any           `json:"html"`
		Date        string        `json:"date"`
		Attachments []interface{} `json:"attachments"`
	} `json:"data"`
}

/*
 * neighboursShRandomUsername 生成随机用户名，前缀 sdk + 10 位小写字母数字
 */
func neighboursShRandomUsername() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	b.WriteString("sdk")
	for i := 0; i < 10; i++ {
		b.WriteByte(chars[rand.Intn(len(chars))])
	}
	return b.String()
}

/*
 * neighboursShClient 获取渠道 HTTP 客户端：优先使用注入的共享缓存客户端，
 * 包内独立运行时（无根包注入）自动降级为本地构建的 TLS 指纹客户端。
 */
func neighboursShClient() tls_client.HttpClient {
	if HTTPClient == nil {
		/* 包内独立运行（测试等）：按当前配置快照自建客户端 */
		options := []tls_client.HttpClientOption{
			tls_client.WithTimeoutSeconds(30),
			tls_client.WithCookieJar(tls_client.NewCookieJar()),
		}
		if GetConfigSnapshot != nil {
			cfg := GetConfigSnapshot()
			if cfg.Timeout > int64(time.Second) {
				options[0] = tls_client.WithTimeoutSeconds(int(cfg.Timeout / int64(time.Second)))
			}
			if cfg.Insecure {
				options = append(options, tls_client.WithInsecureSkipVerify())
			}
			if cfg.Proxy != "" {
				options = append(options, tls_client.WithProxyUrl(cfg.Proxy))
			}
		}
		client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
		if err == nil {
			return client
		}
		final, ferr := tls_client.NewHttpClient(tls_client.NewNoopLogger())
		if ferr != nil {
			return nil
		}
		return final
	}
	client := HTTPClient()
	if client == nil {
		return nil
	}
	return client
}

/*
 * neighboursShGetJSON 使用 SDK 共享客户端发起 GET 请求并读取响应体
 */
func neighboursShGetJSON(u string) ([]byte, int, error) {
	client := neighboursShClient()
	if client == nil {
		return nil, 0, fmt.Errorf("neighbours-sh: HTTP 客户端初始化失败")
	}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

/*
 * NeighboursShGenerate 创建 neighbours.sh 临时邮箱
 * neighbours.sh 裸域已被平台弃收（不在 /config/domains 现役列表），
 * 先在 /api/v1/config/domains 拉取现役域名随机选择，避免生成黑洞地址。
 */
func NeighboursShGenerate() (*CreatedMailbox, error) {
	body, status, err := neighboursShGetJSON(fmt.Sprintf("%s/config/domains", neighboursShBase))
	if err != nil {
		return nil, fmt.Errorf("neighbours-sh: 获取域名列表失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("neighbours-sh: 获取域名列表 http %d", status)
	}
	var cfg struct {
		Data struct {
			Domains         []string `json:"domains"`
			WildcardDomains []string `json:"wildcardDomains"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("neighbours-sh: 解析域名列表失败: %w", err)
	}
	domains := append([]string{}, cfg.Data.Domains...)
	for _, w := range cfg.Data.WildcardDomains {
		domains = append(domains, strings.TrimPrefix(w, "*."))
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("neighbours-sh: 平台无可用域名")
	}
	domain := domains[rand.Intn(len(domains))]
	email := neighboursShRandomUsername() + "@" + domain
	return &CreatedMailbox{
		Channel: "neighbours-sh",
		Email:   email,
		Token:   email,
	}, nil
}

/*
 * neighboursShFlatten 将 neighbours.sh 邮件详情映射为标准化中间 map
 */
func neighboursShFlatten(detail *neighboursShDetailResponse, recipient string) map[string]any {
	d := detail.Data
	from := ""
	if len(d.From.Value) > 0 {
		from = d.From.Value[0].Address
	}
	if strings.TrimSpace(from) == "" {
		from = d.From.Text
	}
	to := d.To.Text
	if strings.TrimSpace(to) == "" {
		to = recipient
	}
	id := ""
	if d.UID != nil {
		id = fmt.Sprintf("%d", *d.UID)
	}
	text := ""
	if s, ok := d.Text.(string); ok {
		text = s
	}
	htmlBody := ""
	if s, ok := d.HTML.(string); ok {
		htmlBody = s
	}
	return map[string]any{
		"id":          id,
		"from":        from,
		"to":          to,
		"subject":     d.Subject,
		"text":        text,
		"html":        htmlBody,
		"date":        d.Date,
		"attachments": d.Attachments,
	}
}

/*
 * neighboursShToNorm 无根包注入时的本地兜底归一化，保底返回非空邮件
 */
func neighboursShToNorm(raw map[string]any) NormEmail {
	str := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		return ""
	}
	return NormEmail{
		ID:      str(raw["id"]),
		From:    str(raw["from"]),
		To:      str(raw["to"]),
		Subject: str(raw["subject"]),
		Text:    str(raw["text"]),
		HTML:    str(raw["html"]),
		Date:    str(raw["date"]),
	}
}

/*
 * NeighboursShGetEmails 获取 neighbours.sh 邮件列表
 * API: GET /inbox/{address} 取列表拿 uid，再逐个 GET /inbox/{address}/{uid} 取完整正文
 * @param token - 邮箱地址
 * @param email - 邮箱地址
 */
func NeighboursShGetEmails(token, email string) ([]NormEmail, error) {
	address := strings.TrimSpace(token)
	if address == "" {
		address = strings.TrimSpace(email)
	}
	if address == "" {
		return nil, fmt.Errorf("neighbours-sh: 缺少邮箱地址")
	}

	listURL := fmt.Sprintf("%s/inbox/%s", neighboursShBase, url.PathEscape(address))
	body, status, err := neighboursShGetJSON(listURL)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("neighbours-sh: 获取邮件列表失败 http %d", status)
	}
	var list neighboursShListResponse
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}

	out := make([]NormEmail, 0, len(list.Data))
	for _, row := range list.Data {
		if row.UID == nil {
			continue
		}
		/* 详情接口返回完整正文 */
		detailURL := fmt.Sprintf("%s/inbox/%s/%d", neighboursShBase, url.PathEscape(address), *row.UID)
		detailBody, detailStatus, err := neighboursShGetJSON(detailURL)
		if err != nil || detailStatus < 200 || detailStatus >= 300 {
			continue
		}
		var detail neighboursShDetailResponse
		if err := json.Unmarshal(detailBody, &detail); err != nil {
			continue
		}
		if detail.Data == nil {
			continue
		}
		if NormalizeMap != nil {
			out = append(out, NormalizeMap(neighboursShFlatten(&detail, address), address))
			continue
		}
		/* 包内独立运行（无根包注入归一化）时本地兜底映射 */
		out = append(out, neighboursShToNorm(neighboursShFlatten(&detail, address)))
	}
	return out, nil
}
