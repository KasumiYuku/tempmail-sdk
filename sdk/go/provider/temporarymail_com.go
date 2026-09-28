package provider

import (
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"net/url"
	"regexp"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Temporarymail 渠道实现（temporarymail.com）
 * 无认证 REST（key 为空即随机建箱）：
 * - 建箱 GET /api/?action=requestEmailAccess&key=&value=random，
 *   响应 {"address":"...","secretKey":"..."}，secretKey 用于后续 checkInbox。
 * - 读信 GET /api/?action=checkInbox&value=<secretKey>，响应有两种形态：
 *   空收件箱为 []，有信时为 map[id]→邮件元数据对象
 *   {from,name,to,subject,date,id,sourceHash,attachments}（2026-09-28 实测）。
 * - 详情 POST /api/?action=getEmail&value=<id>（官网 mainScanner.js），
 *   响应同形态单元素对象，subject 为真实主题（列表常为 "[No Subject]"）；
 *   存在 code=429/captcha 风控，失败时列表元数据兜底。
 * - 全文 GET /view/?i=<id>（前端 iframe 嵌载的同源渲染端点，无风控），
 *   返回 text/plain→HTML 化网页（正文按行转 <br />），
 *   本地剥标签还原纯文本，覆盖全信正文（真实哨兵命中）。
 * 地址最长周期固定为 4 小时。
 *
 * 403 风控说明（2026-09-28 实测）：
 *   协调者直连 403 的根因不在固定头集：本地无头 curl、go-http-client UA、
 *   浏览器 UA、UA+Referer+Origin 各组合均 200（cloudflare 网关无 Set-Cookie），
 *   403 系出口 IP 在边缘被按连接特征标记（代理环境偶发）。
 *   策略：/api/ 请求铺浏览器形态头，403 换备用 UA 重试一次，
 *   429 平台限流报错交给 SDK 外层退避重试（checkInbox 调用间隔≥15s）。
 */

const temporarymailBase = "https://temporarymail.com"

/* temporarymailAltUA 备用浏览器 UA（403 重试用，规避共享池随机 UA 耗尽） */
const temporarymailAltUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

/* temporarymailGenerateResponse 建箱响应 */
type temporarymailGenerateResponse struct {
	Address   string `json:"address"`
	SecretKey string `json:"secretKey"`
}

/* temporarymailDetail 详情端点第一元素字段（映射 2026-09-28 实测全键） */
type temporarymailDetail struct {
	Subject   string `json:"subject"`
	From      string `json:"from"`
	Name      string `json:"name"`
	Date      int64  `json:"date"`
	Source    string `json:"source"`
	SourceSum string `json:"sourceHash"`
}

/*
 * temporarymailNewAPIRequest 构造 /api/ 请求并铺设浏览器形态头部集。
 * 平台对头不挑剔（裸 curl 亦 200），头部为固化的最稳组合，
 * 规避平台对脚本请求判别尺度收紧。
 * @param method  HTTP 方法（GET/POST）
 * @param apiPath 以 /api/ 开头的请求路径
 * @param ua      UA（调用方决定主用/备用）
 */
func temporarymailNewAPIRequest(method, apiPath, ua string) (*http.Request, error) {
	req, err := http.NewRequest(method, temporarymailBase+apiPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Referer", temporarymailBase+"/")
	req.Header.Set("Origin", temporarymailBase)
	req.Header.Set("User-Agent", ua)
	return req, nil
}

/*
 * TemporarymailGenerate 创建 temporarymail.com 临时邮箱
 * GET /api/?action=requestEmailAccess&key=&value=random，
 * key 为空时服务端随机分配地址；token 复用 secretKey。
 */
func TemporarymailGenerate() (*CreatedMailbox, error) {
	req, err := temporarymailNewAPIRequest(http.MethodGet, "/api/?action=requestEmailAccess&key=&value=random", GetCurrentUA())
	if err != nil {
		return nil, err
	}

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
		if resp.StatusCode == 429 {
			/* 平台限流：报出 Retry-After 供 SDK 外层退避重试 */
			ra := resp.Header.Get("Retry-After")
			return nil, fmt.Errorf("temporarymail: 创建邮箱平台限流(429 Retry-After=%s)，请稍后重试: %s", ra, string(body))
		}
		return nil, fmt.Errorf("temporarymail: 创建邮箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data temporarymailGenerateResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("temporarymail: 解析创建响应失败: %w", err)
	}
	addr := strings.TrimSpace(data.Address)
	if addr == "" || data.SecretKey == "" {
		return nil, fmt.Errorf("temporarymail: 创建响应缺少 address 或 secretKey: %s", string(body))
	}

	return &CreatedMailbox{
		Channel: "temporarymail-com",
		Email:   addr,
		Token:   data.SecretKey,
	}, nil
}

/*
 * TemporarymailGetEmails 读取 temporarymail 收件箱
 * GET /api/?action=checkInbox&value=<secretKey>，响应为邮件数组（空时为 []）
 * 或 map[id]→对象（有信时）。key 无效时返回 {"error":"...","code":500}。
 * 列表主题常为 "[No Subject]"：逐封拉详情后覆盖真实主题，
 * 并逐封从 /view/ 渲染端点抓取全文（覆盖列表无正文的局限）。
 * @param email - 邮箱地址
 * @param token - 建箱返回的 secretKey
 */
func TemporarymailGetEmails(email, token string) ([]NormEmail, error) {
	body, err := temporarymailCheckInbox(token)
	if err != nil {
		return nil, err
	}

	/* 平台响应两种合法形态：空箱 []，有信为 map[id]→对象（2026-09-28 实测） */
	var list []map[string]interface{}
	if err := json.Unmarshal(body, &list); err != nil {
		var obj map[string]map[string]interface{}
		if err2 := json.Unmarshal(body, &obj); err2 != nil {
			return nil, fmt.Errorf("temporarymail: 解析收件箱响应失败（secretKey 可能已失效）: %w", err)
		}
		for _, item := range obj {
			list = append(list, item)
		}
	}

	out := make([]NormEmail, 0, len(list))
	for _, m := range list {
		/* 列表元素无 to 字段，注入收件人地址以归一化 */
		if _, ok := m["to"]; !ok {
			m["to"] = email
		}
		id := temporarymailStr(m["id"])
		if id != "" {
			/* 详情覆盖真实主题（失败不致命：列表元数据兜底） */
			if det, dErr := temporarymailFetchDetail(id); dErr == nil {
				if det.Subject != "" {
					m["subject"] = det.Subject
				}
				if det.From != "" {
					m["from"] = det.From
				}
			}
			/* /view/ 渲染端点全文（失败不致命：列表元数据兜底） */
			if text, vErr := temporarymailFetchView(id); vErr == nil && text != "" {
				m["text"] = text
			}
		}
		out = append(out, NormalizeMap(m, email))
	}
	return out, nil
}

/* temporarymailStr 将任意 JSON 值规整为字符串（nil→空串） */
func temporarymailStr(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

/* temporarymailIssued 平台限流/风控类错误（供上层降级兜底） */
type temporarymailIssued struct {
	code int
	msg  string
}

func (e *temporarymailIssued) Error() string { return e.msg }

/*
 * temporarymailCheckInbox 拉取 checkInbox 响应体。
 * 403 时换备用 UA 重试一次（出口被边缘标记时换指纹形状可绕过）；
 * 429 返回平台限流错误交给 SDK 外层退避；其余非 2xx 携带响应体报错。
 * @param token secretKey
 */
func temporarymailCheckInbox(token string) ([]byte, error) {
	for _, ua := range []string{GetCurrentUA(), temporarymailAltUA} {
		req, err := temporarymailNewAPIRequest(http.MethodGet, "/api/?action=checkInbox&value="+url.QueryEscape(token), ua)
		if err != nil {
			return nil, err
		}
		resp, err := HTTPClient().Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode == 429 {
			ra := resp.Header.Get("Retry-After")
			return nil, fmt.Errorf("temporarymail: 读取收件箱平台限流(429 Retry-After=%s)，请拉大轮询间隔", ra)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, nil
		}
		/* 403/404 疑似 UA 键控风控，换备用 UA 重试一次 */
		if resp.StatusCode != 403 && resp.StatusCode != 404 {
			return nil, fmt.Errorf("temporarymail: 读取收件箱失败 http %d: %s", resp.StatusCode, string(body))
		}
	}
	return nil, fmt.Errorf("temporarymail: 读取收件箱失败 http 403（两次尝试均被拒）")
}

/*
 * temporarymailFetchDetail 拉取单封邮件详情（POST /api/?action=getEmail）
 * 响应为 {id: {...}} 单元素对象；平台对详情有 429/captcha 风控，
 * 该类响应返回 temporarymailIssued 供上层降级（列表元数据兜底）。
 * @param id 邮件 ID
 */
func temporarymailFetchDetail(id string) (*temporarymailDetail, error) {
	req, err := temporarymailNewAPIRequest(http.MethodPost, "/api/?action=getEmail&value="+url.QueryEscape(id), GetCurrentUA())
	if err != nil {
		return nil, err
	}
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 429 {
		return nil, &temporarymailIssued{code: 429, msg: "temporarymail: 详情平台限流(429)"}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("temporarymail: 详情失败 http %d", resp.StatusCode)
	}
	var obj map[string]temporarymailDetail
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("temporarymail: 详情解析失败: %w", err)
	}
	for _, d := range obj {
		det := d
		return &det, nil
	}
	return nil, fmt.Errorf("temporarymail: 详情响应为空")
}

/*
 * temporarymailFetchView 抓取 /view/ 渲染端点全文并还原纯文本。
 * 响应为 platform 已 HTML 化的 text/plain（正文逐行带 <br />），
 * 剥 <br>/<p> 换行为换行符后剔除其余标签，得全信正文。
 * @param id 邮件 ID
 */
func temporarymailFetchView(id string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, temporarymailBase+"/view/?i="+url.QueryEscape(id)+"&width=800", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html, */*")
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Referer", temporarymailBase+"/")
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("temporarymail: view http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return temporarymailViewToText(string(raw)), nil
}

/* temporarymailTagRe 剥除 HTML 标签（含属性） */
var temporarymailTagRe = regexp.MustCompile(`<script[\s\S]*?</script>|<style[\s\S]*?</style>|<[^>]+>`)

/* htmlUnescapeFull 反转义 HTML 实体（复用 stdhtml，二次回转常见命名实体） */
func htmlUnescapeFull(s string) string {
	s = stdhtml.UnescapeString(s)
	return strings.NewReplacer("&quot;", "\"", "&apos;", "'").Replace(s)
}

/* temporarymailViewToText 将 /view/ 响应剥标签还原为纯文本（<br>/<p> 换行保留） */
func temporarymailViewToText(src string) string {
	replacer := strings.NewReplacer(
		"<br />", "\n", "<br/>", "\n", "<br>", "\n",
		"<p>", "\n", "</p>", "\n",
		"&nbsp;", " ", "&gt;", ">", "&lt;", "<", "&amp;", "&", "&quot;", "\"",
	)
	src = replacer.Replace(src)
	src = temporarymailTagRe.ReplaceAllString(src, " ")
	src = htmlUnescapeFull(src)
	lines := make([]string, 0, 8)
	for _, ln := range strings.Split(src, "\n") {
		ln = strings.TrimSpace(ln)
		lines = append(lines, ln)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
