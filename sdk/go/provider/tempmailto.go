package provider

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Tempmailto 渠道实现（tempmailto.com，Laravel）
 *
 * 调研实证结论（2026-09-27 三轮抓包 + 现场回验）：
 *   官方 REST apiKey 通道 401，可用路径为前端同款协议：
 *   - GET https://tempmailto.com/ 首页：建立 Cookie 会话（权威邮箱藏在
 *     加密 Cookie，故必须用带 Cookie 罐的客户端），并从
 *     <meta name="csrf-token" content="..."> 提取 CSRF _token；
 *     首页由服务端渲染好一个当前邮箱（#mainEmail value），同一会话内
 *     不变，因此建箱只需 GET + 提取即可。
 *   - POST https://tempmailto.com/get_messages（表单：_token=<CSRF>、
 *     captcha 留空）返回 JSON：{status, mailbox, email_token,
 *     messages:[], histories:[]}，200 即成功。全链路已 curl 实测。
 *     window.check_recaptcha=true 由服务端模板注入使验证码短路（浏览器
 *     AXIOS 的 postData 实为 {"_token":"..."}，无 captcha 字段）。
 *   - messages 元素字段（app.js 模板 + 实测响应）：id / from /
 *     from_email / subject / receivedAt / is_seen；详情页为站内
 *     GET /view/{id}。
 *
 * 会话粘性：邮箱由会话 Cookie 承载，无独立密钥。SDK 层约定
 * Token=邮箱（注册表以 token 非空作防御），GetEmails 读到的当前邮箱
 * 与请求邮箱不一致时用 change 以请求邮箱名拉回目标邮箱。
 */

const tempmailtoOrigin = "https://tempmailto.com"

/* tempmailtoMessagesResponse POST /get_messages（与 /change 同构）响应 */
type tempmailtoMessagesResponse struct {
	Status     bool                     `json:"status"`
	Mailbox    string                   `json:"mailbox"`
	EmailToken string                   `json:"email_token"`
	Messages   []map[string]interface{} `json:"messages"`
	Histories  []map[string]interface{} `json:"histories"`
}

var (
	tmtoCSRFRe      = regexp.MustCompile(`<meta\s+name="csrf-token"\s+content="([^"]+)"`)
	tmtoMainEmailRe = regexp.MustCompile(`(?is)id="mainEmail"[^>]*\bvalue="([^"]+)"`)
	tmtoLocalPartRe = regexp.MustCompile(`^[^@]+`)
)

/* tempmailtoHTMLToText 将详情 HTML 转为纯文本（去 script/style/标签 + 反转义） */
func tempmailtoHTMLToText(src string) string {
	s := regexp.MustCompile(`(?is)<(script|style)[\s\S]*?</\1>`).ReplaceAllString(src, " ")
	s = regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(html.UnescapeString(s)), " ")
}

/* tempmailtoSetBrowserHeaders 设置同站浏览器特征头（与前端同源调用一致） */
func tempmailtoSetBrowserHeaders(req *http.Request, accept string) {
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", tempmailtoOrigin)
	req.Header.Set("Referer", tempmailtoOrigin+"/")
}

/*
 * TempmailtoGenerate 创建 tempmailto.com 临时邮箱
 * GET 首页建立会话并提取服务端渲染的当前邮箱；Token 约定为邮箱本身
 * （注册表需 token 非空）。邮箱约 10 分钟无活动过期。
 */
func TempmailtoGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest("GET", tempmailtoOrigin, nil)
	if err != nil {
		return nil, err
	}
	tempmailtoSetBrowserHeaders(req, "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempmailto: 建立会话失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tempmailto: 首页 http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	email := tmtoExtractMailbox(string(body))
	if email == "" {
		return nil, fmt.Errorf("tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱")
	}

	return &CreatedMailbox{
		Channel:   "tempmailto",
		Email:     email,
		Token:     email,
		ExpiresAt: time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}, nil
}

/* tmtoExtractMailbox 从首页 HTML 提取 #mainEmail 的 value（服务端渲染的当前邮箱） */
func tmtoExtractMailbox(page string) string {
	if m := tmtoMainEmailRe.FindStringSubmatch(page); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

/* tempmailtoCSRF 拉取首页并提取 CSRF _token（读信/换箱共用） */
func tempmailtoCSRF() (string, error) {
	req, err := http.NewRequest("GET", tempmailtoOrigin, nil)
	if err != nil {
		return "", err
	}
	tempmailtoSetBrowserHeaders(req, "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if m := tmtoCSRFRe.FindStringSubmatch(string(body)); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("tempmailto: 首页未找到 csrf-token")
}

/*
 * tempmailtoChange 换箱（POST /change：_token + name + domain）
 * @param email 目标邮箱（作为 name+domain 来源）
 * @returns 变更后的当前邮箱
 */
func tempmailtoChange(email string) (string, error) {
	token, err := tempmailtoCSRF()
	if err != nil {
		return "", err
	}
	name := tmtoLocalPartRe.FindString(email)
	if name == "" {
		name = "TmSdk"
	}
	domain := "tempmailto.com"
	if i := strings.LastIndexByte(email, '@'); i >= 0 && i+1 < len(email) {
		domain = email[i+1:]
	}

	form := url.Values{}
	form.Set("_token", token)
	form.Set("name", name)
	form.Set("domain", domain)
	req, err := http.NewRequest("POST", tempmailtoOrigin+"/change", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	tempmailtoSetBrowserHeaders(req, "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var data tempmailtoMessagesResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return "", fmt.Errorf("tempmailto: 解析 change 响应失败: %w", err)
	}
	if data.Mailbox == "" {
		return "", fmt.Errorf("tempmailto: change 响应异常: %s", strings.TrimSpace(string(body)))
	}
	return data.Mailbox, nil
}

/*
 * tempmailtoFetchMessages POST /get_messages（_token + captcha 留空）
 * @returns 完整响应（含 mailbox 与 messages 列表）
 */
func tempmailtoFetchMessages() (*tempmailtoMessagesResponse, error) {
	token, err := tempmailtoCSRF()
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("_token", token)
	form.Set("captcha", "")
	req, err := http.NewRequest("POST", tempmailtoOrigin+"/get_messages", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	tempmailtoSetBrowserHeaders(req, "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

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
		return nil, fmt.Errorf("tempmailto 读信: http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var data tempmailtoMessagesResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("tempmailto: 解析读信响应失败: %w", err)
	}
	return &data, nil
}

/*
 * tempmailtoBuild 将 messages 列表元素组装为 NormEmail
 * 正文来源：详情页 /view/{id}（同 Cookie 会话），提取失败回退列表字段。
 */
func tempmailtoBuild(row map[string]interface{}, email string) *NormEmail {
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := row[k]; ok && v != nil {
				if s := strings.TrimSpace(fmt.Sprintf("%v", v)); s != "" && s != "<nil>" {
					return s
				}
			}
		}
		return ""
	}
	fromEmail := str("from_email", "from")
	fromName := str("from_name")
	if fromName == "" {
		fromName = fromEmail
	}
	ne := &NormEmail{
		ID:      str("id"),
		From:    fromName,
		To:      email,
		Subject: str("subject"),
		Date:    str("receivedAt", "received_at", "createdAt"),
	}
	if ne.Date == "" {
		ne.Date = time.Now().UTC().Format(time.RFC3339)
	}
	isSeen := false
	if v, ok := row["is_seen"]; ok {
		switch b := v.(type) {
		case bool:
			isSeen = b
		case float64:
			isSeen = b != 0
		case string:
			isSeen = b == "1" || strings.EqualFold(b, "true")
		}
	}
	ne.IsRead = isSeen

	if ne.ID != "" {
		if htmlBody := tempmailtoViewDetail(ne.ID); htmlBody != "" {
			ne.HTML = htmlBody
			ne.Text = tempmailtoHTMLToText(htmlBody)
		}
	}
	if ne.Text == "" {
		ne.Text = str("body", "text", "snippet", "preview")
		if ne.Text == "" {
			ne.Text = ne.Subject
		}
	}
	if ne.HTML == "" {
		ne.HTML = "<html><body><pre>" + html.EscapeString(ne.Text) + "</pre></body></html>"
	}
	return ne
}

/*
 * tempmailtoViewDetail GET /view/{id} 提取邮件正文 HTML（同 Cookie 会话）。
 * 平台视图页结构以候选 class 依次尝试，全失败回退 <main>/<article> 区块；
 * 仍失败返回空串（列表归一不因此中断）。
 */
func tempmailtoViewDetail(id string) string {
	req, err := http.NewRequest("GET", tempmailtoOrigin+"/view/"+id, nil)
	if err != nil {
		return ""
	}
	tempmailtoSetBrowserHeaders(req, "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	page := string(body)

	for _, class := range []string{"mail-body", "mail_content", "email-body", "content-body", "message-content", "mail-content"} {
		re := regexp.MustCompile(`(?is)<[^>]+class="[^"]*\b` + regexp.QuoteMeta(class) + `\b[^"]*"[^>]*>([\s\S]*?)</(?:div|section|article)>`)
		if m := re.FindStringSubmatch(page); m != nil && strings.TrimSpace(m[1]) != "" {
			return strings.TrimSpace(m[1])
		}
	}
	if m := regexp.MustCompile(`(?is)<(main|article)[^>]*>([\s\S]*?)</\1>`).FindStringSubmatch(page); m != nil {
		if strings.TrimSpace(m[2]) != "" {
			return strings.TrimSpace(m[2])
		}
	}
	return ""
}

/*
 * TempmailtoGetEmails 读取 tempmailto.com 当前邮箱的收件箱。
 * 全局 Cookie 罐粘住会话；读到的当前邮箱与请求邮箱不一致时用 change
 * 拉回。token 为 Generate 时约定的邮箱（防御性非空即可，无密钥用途）。
 */
func TempmailtoGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("tempmailto: 邮箱为空，请重新 Generate")
	}

	data, err := tempmailtoFetchMessages()
	if err != nil {
		return nil, err
	}

	/* 会话当前邮箱与请求目标不一致 -> change 拉回（change 内置重新取 CSRF） */
	if data.Mailbox != "" && !strings.EqualFold(data.Mailbox, email) {
		changed, err := tempmailtoChange(email)
		if err != nil {
			return nil, fmt.Errorf("tempmailto: 会话邮箱与请求不一致且拉起失败: %w", err)
		}
		if !strings.EqualFold(changed, email) {
			return nil, fmt.Errorf("tempmailto: 会话邮箱无法拉回请求邮箱（%s != %s），请重新 Generate", changed, email)
		}
		if data, err = tempmailtoFetchMessages(); err != nil {
			return nil, err
		}
	}

	out := make([]NormEmail, 0, len(data.Messages))
	for _, m := range data.Messages {
		ne := tempmailtoBuild(m, email)
		if ne == nil || ne.ID == "" {
			continue
		}
		out = append(out, *ne)
	}
	return out, nil
}
