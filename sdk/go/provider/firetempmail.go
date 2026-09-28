package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Firetempmail 渠道实现（firetempmail.com）
 * 无认证 REST：建箱无需请求，本地生成 随机词+0-999@<域>（域池与官网一致：
 * offrework.click / service-today.click / jobsdeforyou.sa.com）；
 * 读信 GET https://mail.firetempmail.com/mail/get?address=<邮箱 URL 编码>，
 * 必须携带 Header Origin: https://firetempmail.com（否则 403 'Origin not allowed'）。
 * 实测（官网 JS 与黑盒探测）：address 精确形态为完整邮箱，域名白名单为上述三个平台域
 * 以及 gmail.com / googlemail.com（firetempmail.com 自身不在白名单内，返回 400
 * 'Valid address query required!'）。响应形如
 * {"status":"ok","code":200,"msg":"...","stats":{},"mails":[...]}，
 * 邮件字段以 sender/subject/date/recipient/suffix + content-html/content-text/content-plain
 * 多候选归一化（为空则体现为 "No available emails"）。
 */

const firetempmailAPIBase = "https://mail.firetempmail.com"
const firetempmailOrigin = "https://firetempmail.com"

/* firetempmailDomains 官网 JS chunk IYxzyqkp 中的完整平台域池，顺序与官网一致 */
var firetempmailDomains = []string{"offrework.click", "service-today.click", "jobsdeforyou.sa.com"}

/* firetempmailInboxResponse 收件箱响应（错误与成功共用同一信封） */
type firetempmailInboxResponse struct {
	Status string                   `json:"status"`
	Code   int                      `json:"code"`
	Msg    string                   `json:"msg"`
	Stats  map[string]interface{}   `json:"stats"`
	Mails  []map[string]interface{} `json:"mails"`
}

/* firetempmailLocal 随机小写单词 + 0-999（与官网 faker unique 词 + 1e3 取整一致） */
func firetempmailLocal() string {
	const chars = "abcdefghijklmnopqrstuvwxyz"
	var b strings.Builder
	n := 3 + rand.Intn(4) // 3-6 位，模拟官网 1-5 字母词的观感
	for i := 0; i < n; i++ {
		b.WriteByte(chars[rand.Intn(len(chars))])
	}
	return b.String()
}

/*
 * FiretempmailGenerate 创建临时邮箱
 * 建箱无需请求，本地生成 随机词+0-999@域名 的形式
 */
func FiretempmailGenerate() (*CreatedMailbox, error) {
	dom := firetempmailDomains[rand.Intn(len(firetempmailDomains))]
	email := firetempmailLocal() + fmt.Sprintf("%d", rand.Intn(1000)) + "@" + dom
	// token 复用完整地址：注册层对空 token 有统一兜底
	return &CreatedMailbox{Channel: "firetempmail", Email: email, Token: email}, nil
}

/*
 * FiretempmailGetEmails 读取收件箱
 * GET https://mail.firetempmail.com/mail/get?address=<URL 编码完整邮箱>，必带 Origin 头
 */
func FiretempmailGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("firetempmail 读信: 邮箱地址为空")
	}

	req, err := http.NewRequest("GET", firetempmailAPIBase+"/mail/get?address="+url.QueryEscape(email), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", firetempmailOrigin)
	req.Header.Set("Referer", firetempmailOrigin+"/")
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
		return nil, fmt.Errorf("firetempmail 读信: http %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var data firetempmailInboxResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.Status != "" && data.Status != "ok" {
		return nil, fmt.Errorf("firetempmail 读信: %s", data.Msg)
	}

	out := make([]NormEmail, 0, len(data.Mails))
	for _, m := range data.Mails {
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		// 官网 JSON 无统一 to 字段，收件人固定为当前邮箱
		flat["to"] = email
		// 正文多候选：content-html 优先，其次 content-text / content-plain / text / html
		if v, ok := pickStr(m, "content-html", "html"); ok {
			flat["html"] = v
		}
		if v, ok := pickStr(m, "content-text", "content-plain", "text"); ok {
			flat["text"] = v
		}
		if v, ok := pickStr(m, "sender", "from", "from_address"); ok {
			flat["from"] = v
		}
		if v, ok := pickStr(m, "subject", "title"); ok {
			flat["subject"] = v
		}
		if v, ok := pickStr(m, "date", "received_at", "created_at"); ok {
			flat["date"] = v
		}
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
