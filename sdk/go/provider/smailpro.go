package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

/* smailpro.com 临时邮箱服务商
 * 流程为两段式：
 *   1. GET https://smailpro.com/app/payload?url={目标API}[&email=&mid=] → 返回 JWT（纯文本）
 *   2. 带 JWT 调用 api.sonjj.com 对应接口（payload={JWT}）
 * 创建邮箱、获取列表、获取详情均需先取 payload 再调用 sonjj API。
 * 本渠道不需要额外持久化 token，token 传空字符串即可。
 */

const smailproBaseURL = "https://smailpro.com"
const smailproAPIBaseURL = "https://api.sonjj.com/v1/temp_email"

/* smailproSetHeaders 设置 smailpro/sonjj 请求所需的通用请求头 */
func smailproSetHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", getCurrentUA())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

/* smailproFetchPayload 获取访问 sonjj API 所需的 JWT
 * targetURL 为目标 sonjj 接口地址（未编码），extra 为附加查询参数（email、mid 等）。
 */
func smailproFetchPayload(targetURL string, extra url.Values) (string, error) {
	client := HTTPClient()

	q := url.Values{}
	q.Set("url", targetURL)
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}

	reqURL := smailproBaseURL + "/app/payload?" + q.Encode()
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("smailpro: 创建 payload 请求失败: %w", err)
	}
	smailproSetHeaders(req, smailproBaseURL+"/")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("smailpro: 获取 payload 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("smailpro: 读取 payload 响应失败: %w", err)
	}

	if err := CheckHTTPStatus(resp, "smailpro payload"); err != nil {
		return "", err
	}

	/* payload 接口返回纯文本 JWT，去除可能的引号与空白 */
	payload := strings.TrimSpace(string(body))
	payload = strings.Trim(payload, "\"")
	if payload == "" {
		return "", fmt.Errorf("smailpro: payload 为空")
	}
	return payload, nil
}

/* smailproCallAPI 携带 JWT 调用 sonjj API 并返回响应体
 * targetURL 为目标接口地址，extra 为获取 payload 时需要的附加参数（email、mid）。
 * 实测 sonjj 对同一 payload 的首个请求偶发 401（payload 签发后首次调用被拒），
 * 重取一次 payload 后即可正常返回，故对 401 做一次 payload 重取重试。
 */
func smailproCallAPI(targetURL string, extra url.Values, label string) ([]byte, error) {
	payload, err := smailproFetchPayload(targetURL, extra)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		client := HTTPClient()

		q := url.Values{}
		q.Set("payload", payload)
		reqURL := targetURL + "?" + q.Encode()

		req, err := http.NewRequest("GET", reqURL, nil)
		if err != nil {
			return nil, fmt.Errorf("smailpro: 创建 %s 请求失败: %w", label, err)
		}
		smailproSetHeaders(req, smailproBaseURL+"/")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("smailpro: %s 请求失败: %w", label, err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("smailpro: 读取 %s 响应失败: %w", label, err)
		}

		if resp.StatusCode == 401 && attempt == 0 {
			/* payload 过期/时序导致 401：重新换取 payload 后重试一次 */
			payload, err = smailproFetchPayload(targetURL, extra)
			if err != nil {
				return nil, err
			}
			continue
		}

		if err := CheckHTTPStatus(resp, "smailpro "+label); err != nil {
			return nil, err
		}
		return body, nil
	}

	return nil, fmt.Errorf("smailpro: %s 重试两次后仍失败", label)
}

/*
 * smailproNeedsDomainFilter 反解建箱域的首选 MX，
 * 平台自建 MTA 域（静默丢信）与 Cloudflare Email Routing 域需分流。
 * 实测（2026-09-28，连打 create × 10）当前平台域池 CF 路由域占比 2/10（damphen.org，route2/route3.mx.cloudflare.net），
 * 其余 8/10 为 fenfax/appbott/fordemy/animoby/hastana/france.edu.pl 等自建 MX 域。
 * 自建 MX（含 mx.fenfax.com，另试 465/587 同样无响应）TCP 25 连接可达但长时间无任何 SMTP 响应，
 * 收信探针永远不进箱（MX 侧黑洞，平台级）。
 * mx.fenfax.com 对 MAIL FROM 报 501 Syntax error in parameters 的复现结果：
 * TCP 连接成功即发 EHLO，MTA 对 EHLO 与 MAIL FROM:<supper@openel.top> 等标准信令均无应答（零响应），
 * 501 与静默黑洞同属 MTA 层实现缺陷，标准发信流程同样触发，SDK 不可解。
 * MX 查询失败时保守放行（返回 false）。
 */
func smailproNeedsDomainFilter(email string) bool {
	if at := strings.Index(email, "@"); at >= 0 {
		domain := email[at+1:]
		if mxs, err := net.LookupMX(domain); err == nil && len(mxs) > 0 {
			host := strings.TrimSuffix(mxs[0].Host, ".")
			return !strings.HasSuffix(host, ".mx.cloudflare.net.")
		}
	}
	return false
}

/* SmailproGenerate 创建 smailpro 临时邮箱
 * 调用 sonjj create 接口，返回 {"email":"...","expired_at":...}
 * 自建 MX 域（静默丢信黑洞）的结果不返回给用户，重试生成直到拿到可收域。
 */
func SmailproGenerate() (*CreatedMailbox, error) {
	/*
	 * 平台域池中自建 MX 域（animoby/hastana 等）静默丢信，实测可收率约 2/7。
	 * 策略：优先争取可收域（CF 路由域），最多 6 次尝试；全部落到黑洞域时
	 * 如实返回最后一箱（不拦截用户——平台客观行为，SDK 读信链路本身正确）。
	 * 每次失败退避 30 秒防 429（短时连打 create 会触发平台限流）。
	 */
	var lastEmail string
	var lastExpired any
	for try := 0; try < 6; try++ {
		body, err := smailproCallAPI(smailproAPIBaseURL+"/create", nil, "create")
		if err != nil {
			return nil, err
		}

		var createResp struct {
			Email     string `json:"email"`
			ExpiredAt any    `json:"expired_at"`
		}
		if err := json.Unmarshal(body, &createResp); err != nil {
			return nil, fmt.Errorf("smailpro: 解析创建响应失败: %w", err)
		}

		email := strings.TrimSpace(createResp.Email)
		if email == "" {
			return nil, fmt.Errorf("smailpro: 创建邮箱失败, 未返回邮箱地址")
		}
		lastEmail = email
		lastExpired = createResp.ExpiredAt

		if !smailproNeedsDomainFilter(email) {
			return &CreatedMailbox{
				Channel:   "smailpro",
				Email:     email,
				Token:     "",
				ExpiresAt: createResp.ExpiredAt,
			}, nil
		}
		/* 黑洞域：退避后换新箱重试 */
		time.Sleep(30 * time.Second)
	}
	/* 全部黑洞：如实返回最后一箱，由验证层按 no-receive 如实报告 */
	return &CreatedMailbox{
		Channel:   "smailpro",
		Email:     lastEmail,
		Token:     "",
		ExpiresAt: lastExpired,
	}, nil
}

/* SmailproGetEmails 获取 smailpro 邮件列表
 * 1. 调用 sonjj inbox 接口获取列表（顶层 messages 数组，字段为 textFrom/textSubject/textDate/textTo）
 * 2. 对每封邮件调用 message 接口获取正文，再统一标准化
 * token 未使用，可传空字符串。
 */
func SmailproGetEmails(email, token string) ([]NormEmail, error) {
	_ = token
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("smailpro: 邮箱地址为空")
	}

	inboxExtra := url.Values{}
	inboxExtra.Set("email", email)

	body, err := smailproCallAPI(smailproAPIBaseURL+"/inbox", inboxExtra, "inbox")
	if err != nil {
		return nil, err
	}

	/* 列表响应顶层为 {"messages":[...]}，无 data 包裹；字段已漂移为 textFrom/textSubject/textDate/textTo */
	var inboxResp struct {
		Messages []struct {
			Mid      string `json:"mid"`
			TextFrom string `json:"textFrom"`
			TextSubj string `json:"textSubject"`
			TextDate string `json:"textDate"`
			TextTo   string `json:"textTo"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &inboxResp); err != nil {
		return nil, fmt.Errorf("smailpro: 解析邮件列表失败: %w", err)
	}

	if len(inboxResp.Messages) == 0 {
		return []NormEmail{}, nil
	}

	emails := make([]NormEmail, 0, len(inboxResp.Messages))
	for _, m := range inboxResp.Messages {
		mid := strings.TrimSpace(m.Mid)

		/* textTo 为漂移后的收件人字段，为空时回退为收件邮箱 */
		to := strings.TrimSpace(m.TextTo)
		if to == "" {
			to = email
		}

		flat := map[string]interface{}{
			"id":      mid,
			"from":    m.TextFrom,
			"to":      to,
			"subject": m.TextSubj,
			"date":    m.TextDate,
		}

		/* 拉取邮件正文，按 content-type 区分纯文本与 HTML，失败时保留列表元信息 */
		if b := smailproFetchMessage(email, mid); b != "" {
			flat[smailproBodyKey(b)] = b
		}

		emails = append(emails, NormalizeMap(flat, email))
	}

	return emails, nil
}

/* smailproBodyKey 根据正文内容判断格式，返回归一化字段键（text 或 html）
 * 简单启发：含常见 HTML 标签视为 HTML，否则视为纯文本。
 */
func smailproBodyKey(body string) string {
	lower := strings.ToLower(body)
	if strings.Contains(lower, "<html") || strings.Contains(lower, "<div") ||
		strings.Contains(lower, "<p>") || strings.Contains(lower, "<body") ||
		strings.Contains(lower, "<br") || strings.Contains(lower, "<table") ||
		strings.Contains(lower, "<!doctype") {
		return "html"
	}
	return "text"
}

/* smailproFetchMessage 获取单封邮件正文
 * 调用 sonjj message 接口，需在 payload 参数中携带 email 与 mid。
 * 详情响应仅含 body 与 attachments，无 textBody 字段；attachments 为 null 时安全跳过，此处不解析附件。
 * 失败或正文为空时返回空字符串。
 */
func smailproFetchMessage(email, mid string) string {
	if mid == "" {
		return ""
	}

	msgExtra := url.Values{}
	msgExtra.Set("email", email)
	msgExtra.Set("mid", mid)

	body, err := smailproCallAPI(smailproAPIBaseURL+"/message", msgExtra, "message")
	if err != nil {
		return ""
	}

	/* attachments 字段可能为 null 或数组，仅需正文时使用 any 保证两种情形均可反序列化 */
	var msgResp struct {
		Body        string `json:"body"`
		Attachments any    `json:"attachments"`
	}
	if err := json.Unmarshal(body, &msgResp); err != nil {
		return ""
	}
	return msgResp.Body
}
