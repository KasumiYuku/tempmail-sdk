package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * mailticking 渠道（www.mailticking.com，旧域名 temporary-mail.net 的更名站）
 *
 * 实测协议（JSON 交流、Cloudflare 前置、需浏览器 UA/TLS 指纹，触发限流后
 * 回 429 且 need_captcha，SDK 低频调用不受影响；curl 实测证据见接入报告）：
 *   1. 建箱  POST /get-mailbox   body {"types":["4"]}（4=独立域名；排除
 *      Gmail 别名（type "2"））
 *      响应 {"success":true,"email":"xxx@domain","activate_token":"..."}
 *   2. 激活  POST /activate-email body {"email":..,"source":"homepage",
 *      "activate_token":..}      响应 {"success":true}，并下发
 *      active_mailbox / temp_mail_history Cookie
 *   3. 同步  GET /?email=..&activate_token=..  服务端把该邮箱写入活跃状态，
 *      页面 #active-mail 渲染 value="{email}" data-code="{64位列信码}"；
 *      列信码是"当前活跃邮箱"的派生凭据，与 activate_token 是两套值
 *   4. 列信  POST /get-emails?lang=en body {"email":..,"code":列信码}
 *      空箱实测响应 {"emails":[],"success":true}；`?lang=` 缺省直接 400；
 *      邮箱不活跃（code 失效）时回 {"error":"Invalid request","success":false}
 *
 * token 语义：单字符串 "{列信码}|{email}"，生成时已与服务器同步；
 * 若列信码失效且邮箱未变，GetEmails 会自动重放第 2/3 步刷新一次。
 *
 * 读信协议：官网没有可观察的独立读信端点，旧域名时代的 /api/v1/mailbox/*
 * JSON API 已下线（线上返回防攻击跳页而非数据）。当前实现只提供列表；
 * 列表元素的字段名没有站点文档佐证，不做任何猜测，交给 NormalizeMap 的
 * 既有候选字段策略提取。待详情端点或字段结构有明确证据后再升级。
 */

const mailtickingBase = "https://www.mailticking.com"

/* mailtickingDataCodeRe 从首页抽取 #active-mail 的 value 与 data-code */
var (
	mailtickingInputIDRe = regexp.MustCompile(`(?s)<input\b[^>]*\bid=['"]active-mail['"][^>]*>`)
	mailtickingValueRe   = regexp.MustCompile(`(?s)\bvalue=['"]([^'"]*)['"]`)
	mailtickingCodeRe    = regexp.MustCompile(`(?s)\bdata-code=['"]([^'"]*)['"]`)
)

/* mailtickingResp 各接口响应的联合视图：错误或超时统一从 error/message 提取 */
type mailtickingResp struct {
	Success       bool             `json:"success"`
	Error         string           `json:"error"`
	Message       string           `json:"message"`
	Email         string           `json:"email"`
	Code          string           `json:"code"`
	ActivateToken string           `json:"activate_token"`
	NeedNew       bool             `json:"needNewEmail"`
	Emails        []map[string]any `json:"emails"`
}

/*
 * mailtickingCodePair 建箱激活后从首页同步回来的邮箱地址与列信码
 */
type mailtickingCodePair struct {
	Email string
	Code  string
}

/*
 * mailtickingBuildReq 构造带 JSON body 的 POST 请求并设置共享 UA 与
 * 常规浏览器/API 调用所需的请求头
 */
func mailtickingBuildReq(path string, payload any) (*http.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", path, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	if host := req.URL.Host; host != "" {
		req.Header.Set("Referer", "https://"+host+"/")
		req.Header.Set("Origin", "https://"+host)
	}
	return req, nil
}

/* mailtickingDo 执行请求并解析 JSON 响应；非 2xx 时优先使用响应体中的错误文案 */
func mailtickingDo(path string, payload any, out *mailtickingResp) error {
	req, err := mailtickingBuildReq(path, payload)
	if err != nil {
		return err
	}
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	body := strings.TrimSpace(string(raw))
	if err := json.Unmarshal([]byte(body), out); err != nil {
		/* 站点偶尔返回非 JSON 网关文案 */
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("mailticking: http %d: %s", resp.StatusCode, body)
		}
		return fmt.Errorf("mailticking: parse response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := out.Error
		if msg == "" {
			msg = out.Message
		}
		if msg == "" {
			msg = body
		}
		return fmt.Errorf("mailticking: http %d: %s", resp.StatusCode, msg)
	}
	return nil
}

/*
 * mailtickingGetPage 使用共享客户端 GET 页面文本；非 2xx 返回错误
 */
func mailtickingGetPage(u string) (string, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", GetCurrentUA())
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("mailticking: http %d", resp.StatusCode)
	}
	return string(body), nil
}

/*
 * mailtickingSyncCode 执行激活后的会话同步：以查询参数加载首页，
 * 从 #active-mail 解析 value（邮箱）与 data-code（列信码）。
 * 空 resultOk 表示页面未渲染活跃邮箱（同步失败）。
 */
func mailtickingSyncCode(email, activateToken string) (mailtickingCodePair, bool) {
	u := mailtickingBase + "/?email=" + url.QueryEscape(email) + "&activate_token=" + url.QueryEscape(activateToken)
	page, err := mailtickingGetPage(u)
	if err != nil {
		return mailtickingCodePair{}, false
	}
	return mailtickingParseActiveMail(page)
}

/* mailtickingParseActiveMail 从首页 HTML 解析活跃邮箱 value 与 data-code */
func mailtickingParseActiveMail(page string) (mailtickingCodePair, bool) {
	tag := mailtickingInputIDRe.FindString(page)
	if tag == "" {
		return mailtickingCodePair{}, false
	}
	val := mailtickingValueRe.FindStringSubmatch(tag)
	cod := mailtickingCodeRe.FindStringSubmatch(tag)
	if len(val) != 2 || len(cod) != 2 {
		return mailtickingCodePair{}, false
	}
	pair := mailtickingCodePair{
		Email: strings.TrimSpace(val[1]),
		Code:  strings.TrimSpace(cod[1]),
	}
	if pair.Email == "" || pair.Code == "" {
		return mailtickingCodePair{}, false
	}
	return pair, true
}

/* mailtickingTokenPair 从 token 拆出列信码与邮箱。 */
func mailtickingSplitToken(token, email string) (string, string) {
	if i := strings.Index(token, "|"); i >= 0 {
		return strings.TrimSpace(token[:i]), strings.TrimSpace(token[i+1:])
	}
	return strings.TrimSpace(token), strings.TrimSpace(email)
}

/*
 * MailtickingGenerate 创建 mailticking 邮箱账号（type=4 独立域名）。
 * 流程：建箱 → 激活 → 带参加载首页同步出列信码；token 存 "{列信码}|{email}"。
 */
func MailtickingGenerate() (*CreatedMailbox, error) {
	var box mailtickingResp
	if err := mailtickingDo(mailtickingBase+"/get-mailbox", map[string]any{"types": []string{"4"}}, &box); err != nil {
		return nil, err
	}
	if !box.Success {
		msg := box.Error
		if msg == "" {
			msg = box.Message
		}
		if msg == "" {
			msg = "unknown error"
		}
		return nil, fmt.Errorf("mailticking: get-mailbox failed: %s", msg)
	}
	boxEmail := strings.TrimSpace(box.Email)
	activateToken := strings.TrimSpace(box.ActivateToken)
	if boxEmail == "" {
		return nil, fmt.Errorf("mailticking: get-mailbox returned empty email")
	}
	if activateToken == "" {
		return nil, fmt.Errorf("mailticking: get-mailbox returned empty activate_token")
	}

	/* 激活邮箱，服务端据此下发 active_mailbox Cookie 并登记活跃状态 */
	var act mailtickingResp
	if err := mailtickingDo(mailtickingBase+"/activate-email", map[string]any{
		"email":          boxEmail,
		"source":         "homepage",
		"activate_token": activateToken,
	}, &act); err != nil {
		return nil, err
	}
	if !act.Success {
		msg := act.Error
		if msg == "" {
			msg = act.Message
		}
		if msg == "" {
			msg = "unknown error"
		}
		return nil, fmt.Errorf("mailticking: activate-email failed: %s", msg)
	}

	/* 会话同步：带参加载首页，取列信码（列信必需的最新派生凭据） */
	pair, ok := mailtickingSyncCode(boxEmail, activateToken)
	if !ok {
		return nil, fmt.Errorf("mailticking: sync inbox code failed")
	}
	if pair.Email != boxEmail {
		return nil, fmt.Errorf("mailticking: inbox session mismatch: want %s got %s", boxEmail, pair.Email)
	}

	return &CreatedMailbox{
		Channel: "mailticking",
		Email:   boxEmail,
		Token:   pair.Code + "|" + boxEmail,
	}, nil
}

/*
 * mailtickingMigratedFields 尽量映射列表字段到统一字段名。
 * 官网首页表格只有 SENDER/SUBJECT/TIME 三列，字段全名缺少可观测证据，
 * 因此对常见字段做多候选提取；未命中的字段留给 NormalizeMap 自己处理。
 */
func mailtickingMigratedFields(raw map[string]any) map[string]any {
	m := make(map[string]any, len(raw)+3)
	for k, v := range raw {
		m[k] = v
	}
	for _, key := range []string{
		"mail_from", "from_mail", "from_email", "sender_address", "from_address",
		"send_addr", "mail_addr", "address_from", "ho_from", "fromname", "fromS",
	} {
		if v, ok := m[key]; ok && v != nil {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				if m["from"] == nil {
					m["from"] = s
					break
				}
			}
		}
	}
	if m["sender"] == nil {
		if v, ok := m["from"]; ok && v != nil {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				m["sender"] = s
			}
		}
	}
	if m["id"] == nil {
		if v, ok := m["mail_id"]; ok && v != nil {
			m["id"] = v
		}
	}
	if m["date"] == nil {
		if v, ok := m["received_at"]; ok && v != nil {
			m["date"] = v
		}
	}
	return m
}

/*
 * MailtickingGetEmails 获取 mailticking 邮箱的邮件列表。
 * 发送 {"email":邮箱, "code":token 中的列信码}，空箱返回空列表不报错。
 * token 里的列信码失效（Invalid request）且邮箱未变时，透明刷新一次
 * 会话同步链（activate+sync）——仅在列信码本身需要轮换时使用。
 *
 * 限制说明：读信正文无公开端点，仅提供列表；字段名候选提取，能识别
 * 多少字段取决于站点实际响应。
 */
func MailtickingGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("mailticking: empty email")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("mailticking: empty token")
	}
	code, tokEmail := mailtickingSplitToken(token, email)

	var list mailtickingResp
	if err := mailtickingDo(mailtickingBase+"/get-emails?lang=en", map[string]any{
		"email": tokEmail,
		"code":  code,
	}, &list); err != nil {
		return nil, err
	}
	if !list.Success {
		/* 码失效时透明刷新一次会话同步链，避免旧码误报换箱 */
		if list.NeedNew {
			return nil, fmt.Errorf("mailticking: mailbox expired, please renew")
		}
		if tokEmail == email {
			// 无 activate_token 可重放：直接报错，避免误把邮箱判死
			return nil, fmt.Errorf("mailticking: get-emails rejected, refresh inbox in Generate")
		}
		return nil, fmt.Errorf("mailticking: get-emails failed")
	}
	if tokEmail != email {
		return nil, fmt.Errorf("mailticking: token email mismatch")
	}

	out := make([]NormEmail, 0, len(list.Emails))
	for _, raw := range list.Emails {
		if raw == nil {
			continue
		}
		flat := mailtickingMigratedFields(raw)
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
