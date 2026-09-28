package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"mime/quotedprintable"
	"regexp"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

/*
 * TempmailEE 渠道实现（tempmail.ee）
 *
 * 出口风控实证结论（2026-09-27 实测）：
 *   平台读信端 /api/mails 的 403 Access denied 不是 TLS 指纹、请求头顺序
 *   或浏览器真实性校验（curl、tls-client 全指纹均可通过），而是
 *   「会话 Cookie 绑定校验」：
 *   - change（换箱）返回的 Set-Cookie 中 temp_mail_session
 *     （形如 tmapi_sess_xxx）与 temp_email 共同构成读信凭据；
 *   - 同会话查别的邮箱、只带 session 不带 temp_email、只带 visitor
 *     追踪 cookie，均 403；带齐 temp_email + temp_mail_session 即 200。
 *   因此只要在同一个「事务」内完成 change → 提取 Set-Cookie → 读信，
 *   纯云端 SDK 即可真实读到邮件，无需真实浏览器。
 *
 * 会话隔离：建箱与读信全部使用 HTTPClientNoCookieJar（无 Cookie 罐），
 *   会话凭据由本渠道以显式 Cookie 请求头逐请求携带，杜绝与全局
 *   Cookie 罐残留会话串池、也避免同行并发相互污染。
 *
 * 已验证通行配方（fhttp tls-client Chrome 131/144/146 指纹均实测 200）：
 *   - POST /api/mails 无 sec-ch-ua 头也能通过（有正确会话 Cookie 时），
 *     但为贴近真实浏览器 fetch，仍带上完整 sec-ch-ua 三件套，
 *     UA 使用固定 Chrome 154（与其 sec-ch-ua 品牌版本一致）。
 *   - POST /api/mailbox/change 若不带 sec-ch-ua 头会被平台拒绝
 *     （403 Browser request required），带齐即 200，并下发会话 Cookie。
 */

const tempmailEEBase = "https://tempmail.ee"

/* 与 sec-ch-ua 品牌版本一致的固定 UA（Chrome 154 / Linux） */
const tempmailEEUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

/* Token 前缀，用于识别本渠道会话凭据串 */
const tempmailEETokenPrefix = "tempmail-ee|"

/* tempmailEECurrentResponse 建箱/当前邮箱响应 */
type tempmailEECurrentResponse struct {
	Success   bool   `json:"success"`
	Email     string `json:"email"`
	ExpiresAt string `json:"expiresAt"`
	Reason    string `json:"reason"`
}

/* tempmailEEMailsResponse 读信响应 */
type tempmailEEMailsResponse struct {
	Timestamp int64                    `json:"timestamp"`
	Mails     []map[string]interface{} `json:"mails"`
}

/* tempmailEEChangeResponse 建箱（change）响应 */
type tempmailEEChangeResponse struct {
	Success           bool   `json:"success"`
	NewEmail          string `json:"newEmail"`
	ExpiresAt         string `json:"expiresAt"`
	Reason            string `json:"reason"`
	Error             string `json:"error"`
	ChallengeRequired bool   `json:"challengeRequired"`
}

/* tempmailEEBrowserIntegrity 建箱提交的浏览器指纹（与官方前端一致） */
func tempmailEEBrowserIntegrity() map[string]interface{} {
	return map[string]interface{}{
		"webdriver": false, "languagesMissing": false, "languageMissing": false,
		"pluginsMissing": false, "pluginsUndefined": false, "outerSizeMissing": false,
		"innerSizeMissing": false, "screenMissing": false, "screenDepthMissing": false,
		"timezoneMissing": false, "timezoneOffsetMissing": false,
		"userAgentDataPresent": true, "userAgentMissing": false, "platformClass": "Linux",
		"mobile": false, "collectionFailed": false,
	}
}

/*
 * tempmailEESetBrowserHeaders 设置浏览器特征安全头（同站 fetch 全套）
 * @param req      请求
 * @param withSecCH 是否携带 sec-ch-ua 三件套（change 必须，平台据此放行）
 */
func tempmailEESetBrowserHeaders(req *fhttp.Request, withSecCH bool) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", tempmailEEBase)
	req.Header.Set("Referer", tempmailEEBase+"/")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", tempmailEEUA)
	if withSecCH {
		req.Header.Set("Sec-Ch-Ua", `"Chromium";v="154", "Google Chrome";v="154", "Not.A/Brand";v="99"`)
		req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
		req.Header.Set("Sec-Ch-Ua-Platform", `"Linux"`)
	}
}

/* tempmailEECookieFromResponse 从 change 响应提取会话 Cookie 键值对（无 Cookie 罐模式下手动接管会话） */
func tempmailEECookieFromResponse(resp *fhttp.Response) (email, session string) {
	for _, sc := range resp.Header.Values("Set-Cookie") {
		kv := sc
		if i := strings.IndexByte(sc, ';'); i > 0 {
			kv = sc[:i]
		}
		switch {
		case strings.HasPrefix(kv, "temp_email="):
			email = strings.TrimPrefix(kv, "temp_email=")
		case strings.HasPrefix(kv, "temp_mail_session="):
			session = strings.TrimPrefix(kv, "temp_mail_session=")
		}
	}
	return email, session
}

/* tempmailEETokenBuild 由邮箱与 temp_mail_session 组装渠道内部凭据串 */
func tempmailEETokenBuild(email, session string) string {
	return tempmailEETokenPrefix + "temp_email=" + email + "; temp_mail_session=" + session
}

/*
 * tempmailEEParseToken 解析读信凭据，返回 (session, ok)
 * @param token   Generate 时下发的渠道凭据串
 * @param email   请求读取的邮箱（防御 token 与 email 不一致，以请求为准）
 */
func tempmailEEParseToken(token, email string) (string, bool) {
	if !strings.HasPrefix(token, tempmailEETokenPrefix) {
		return "", false
	}
	cred := strings.TrimPrefix(token, tempmailEETokenPrefix)
	session := ""
	for _, part := range strings.Split(cred, ";") {
		kv := strings.TrimSpace(part)
		if strings.HasPrefix(kv, "temp_mail_session=") {
			session = strings.TrimPrefix(kv, "temp_mail_session=")
		}
	}
	if session == "" {
		return "", false
	}
	/* 会话绑定邮箱：以请求邮箱为准重拼 cookie，防止凭据与邮箱错配 */
	return "temp_email=" + email + "; temp_mail_session=" + session, true
}

/*
 * TempmailEEGenerate 创建 tempmail.ee 临时邮箱
 * 事务会话：GET / 面熟 → POST /api/mailbox/change（sec-ch-ua 全套头）换新邮箱
 * → 提取 Set-Cookie 中的 temp_mail_session，随 EmailInfo.token 透传给读信。
 */
func TempmailEEGenerate() (*CreatedMailbox, error) {
	client := HTTPClientNoCookieJar()

	/* 步骤 1：GET / 建立 Cookie 会话（面熟首访） */
	bootReq, err := fhttp.NewRequest("GET", tempmailEEBase, nil)
	if err != nil {
		return nil, err
	}
	bootReq.Header.Set("User-Agent", tempmailEEUA)
	bootReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	bootResp, err := client.Do(bootReq)
	if err != nil {
		return nil, fmt.Errorf("tempmail-ee: 建立会话失败: %w", err)
	}
	io.Copy(io.Discard, bootResp.Body)
	bootResp.Body.Close()

	/* 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua） */
	body, _ := json.Marshal(map[string]interface{}{
		"turnstileToken":   nil,
		"browserIntegrity": tempmailEEBrowserIntegrity(),
	})
	chResp, err := tempmailEEPost(client, "/api/mailbox/change", body, true, "")
	if err != nil {
		return nil, err
	}
	defer chResp.Body.Close()
	chRaw, _ := io.ReadAll(chResp.Body)

	var chg tempmailEEChangeResponse
	if err := json.Unmarshal(chRaw, &chg); err != nil {
		return nil, err
	}
	if !chg.Success || chg.NewEmail == "" {
		return nil, fmt.Errorf("tempmail-ee: 建箱失败: %s（status %d）", strings.TrimSpace(string(chRaw)), chResp.StatusCode)
	}

	/* 步骤 3：从 Set-Cookie 接管会话凭据，供读信校验使用 */
	cookieEmail, session := tempmailEECookieFromResponse(chResp)
	email := chg.NewEmail
	if cookieEmail != "" && cookieEmail != email {
		/* 以防万一以响应体为准，Cookie 仅取 session */
		email = cookieEmail
	}
	if session == "" {
		return nil, fmt.Errorf("tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信")
	}

	expires := chg.ExpiresAt
	if expires == "" {
		expires = time.Now().Add(60 * time.Minute).UTC().Format(time.RFC3339)
	}

	return &CreatedMailbox{
		Channel:   "tempmail-ee",
		Email:     email,
		Token:     tempmailEETokenBuild(email, session),
		ExpiresAt: expires,
	}, nil
}

/*
 * tempmailEEPost 发起带浏览器特征头的 POST（fhttp 客户端）
 * @param client   无 Cookie 罐的 TLS 客户端
 * @param path     请求路径
 * @param body     请求体
 * @param withSecCH 是否携带 sec-ch-ua 头
 * @param cookie   显式 Cookie 请求头（会话凭据串，空则不携带）
 * @returns 响应
 */
func tempmailEEPost(client interface {
	Do(*fhttp.Request) (*fhttp.Response, error)
}, path string, body []byte, withSecCH bool, cookie string) (*fhttp.Response, error) {
	req, err := fhttp.NewRequest("POST", tempmailEEBase+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	tempmailEESetBrowserHeaders(req, withSecCH)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return client.Do(req)
}

/*
 * TempmailEEGetEmails 读取 tempmail.ee 收件箱
 * 凭据来自 Generate 时从 change 响应提取的 temp_mail_session，
 * 逐请求以显式 Cookie 头携带（temp_email + temp_mail_session 缺一不可）。
 */
func TempmailEEGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("tempmail-ee: 邮箱为空")
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("tempmail-ee: token 为空")
	}

	/* 解析会话凭据；无有效 temp_mail_session 无法通过平台会话校验 */
	cookie, ok := tempmailEEParseToken(token, email)
	if !ok {
		return nil, fmt.Errorf("tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱")
	}

	body, _ := json.Marshal(map[string]string{"email": email})
	resp, err := tempmailEEPost(HTTPClientNoCookieJar(), "/api/mails", body, true, cookie)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 403 {
		return nil, fmt.Errorf("tempmail-ee inbox: http 403（会话 Cookie 校验失败，邮箱可能已过期，请重新 Generate）")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tempmail-ee inbox: http %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var data tempmailEEMailsResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}

	/*
	 * 2026-09-27 实测：/api/mails 列表只含元数据
	 * （id/fromAddress/toAddress/subject/createdAt/isRead，无正文），
	 * 正文必须逐一请求 GET /api/mails/{id}，响应的 content 字段为
	 * multipart（RFC822 MIME 原始+顶部边界头，HTML 实体已转义），
	 * 需本函数解码拆出 text / html。从根包得知客户端仅规范为
	 * GET/POST/PUT，ID 由平台下发，属路径的一部分，安全可行。
	 */
	out := make([]NormEmail, 0, len(data.Mails))
	for _, m := range data.Mails {
		ne := tempmailEERowToNorm(m, email)
		if ne == nil {
			continue
		}
		if err := tempmailEEFetchDetail(HTTPClientNoCookieJar(), ne, cookie); err != nil {
			/* 单封详情拉取失败不阻塞列表其余邮件（详情偶发 4xx/网络抖动） */
			continue
		}
		out = append(out, *ne)
	}
	return out, nil
}

/*
 * tempmailEERowToNorm 将 /api/mails 列表行（仅元数据）转换为 NormEmail 骨架
 * @param row   列表元素 map（id/fromAddress/toAddress/subject/createdAt/isRead）
 * @param email 当前邮箱（toAddress 缺失时的回退）
 * @returns 归一化邮件骨架；id 缺失视为无效行返回 nil
 */
func tempmailEERowToNorm(row map[string]interface{}, email string) *NormEmail {
	id := getStrFromMap(row, "id")
	if id == "" {
		return nil
	}
	to := getStrFromMap(row, "toAddress", "to")
	if to == "" {
		to = email
	}
	created := getStrFromMap(row, "createdAt", "date", "receivedAt")
	if created == "" {
		created = time.Now().UTC().Format(time.RFC3339)
	}
	return &NormEmail{
		ID:      id,
		From:    getStrFromMap(row, "fromAddress", "from", "sender"),
		To:      to,
		Subject: getStrFromMap(row, "subject"),
		Date:    created,
		IsRead:  getBoolFromMap(row, "isRead"),
	}
}

/* getStrFromMap 按候选键顺序从 map 提取字符串值（数值转十进制字符串） */
func getStrFromMap(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		v, ok := m[key]
		if !ok || v == nil {
			continue
		}
		switch val := v.(type) {
		case string:
			return strings.TrimSpace(val)
		case float64:
			return strconv.FormatInt(int64(val), 10)
		case json.Number:
			return val.String()
		default:
			return strings.TrimSpace(fmt.Sprintf("%v", val))
		}
	}
	return ""
}

/* getBoolFromMap 从 map 提取布尔值（兼容 bool / 0|1 数值与字符串） */
func getBoolFromMap(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		v, ok := m[key]
		if !ok || v == nil {
			continue
		}
		switch val := v.(type) {
		case bool:
			return val
		case float64:
			return val != 0
		case string:
			return val == "1" || strings.EqualFold(val, "true")
		}
	}
	return false
}

/*
 * tempmailEEFetchDetail 拉取单封详情并填充正文
 * GET /api/mails/{id}（带显式会话 Cookie），content 为平台包装后的
 * MIME multipart 原文（HTML 实体转义版），解析后写入 ne.Text / ne.HTML。
 */
func tempmailEEFetchDetail(client interface {
	Do(*fhttp.Request) (*fhttp.Response, error)
}, ne *NormEmail, cookie string) error {
	req, err := fhttp.NewRequest("GET", tempmailEEBase+"/api/mails/"+ne.ID, nil)
	if err != nil {
		return err
	}
	tempmailEESetBrowserHeaders(req, false)
	req.Header.Set("Accept", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("tempmail-ee detail: http %d", resp.StatusCode)
	}
	var detail map[string]interface{}
	if err := json.Unmarshal(raw, &detail); err != nil {
		return err
	}
	content := getStrFromMap(detail, "content")
	if content == "" {
		/*
		 * content 缺失时回退兜底候选键，避免平台字段演进后正文丢失
		 * （实测老版前端仅用 content）。
		 */
		content = getStrFromMap(detail, "text", "body", "html")
		if content == "" {
			return fmt.Errorf("tempmail-ee detail: 无正文字段")
		}
	}
	text, htmlStr := tempmailEEParseContent(content)
	if ne.Text == "" {
		ne.Text = text
	}
	if ne.HTML == "" {
		ne.HTML = htmlStr
	}
	/* 详情缺失 from/subject 时补全（列表行已带则不动） */
	if ne.From == "" {
		ne.From = getStrFromMap(detail, "fromAddress", "from")
	}
	if ne.Subject == "" {
		ne.Subject = getStrFromMap(detail, "subject")
	}
	return nil
}

var teeScriptRe = regexp.MustCompile(`(?is)<script[\s\S]*?</script>`)
var teeTagRe = regexp.MustCompile(`(?s)<[^>]+>`)

/* teeHTMLToText 将 HTML 转为纯文本（源自根包逻辑的本地版本） */
func teeHTMLToText(src string) string {
	cleaned := teeScriptRe.ReplaceAllString(src, " ")
	cleaned = teeTagRe.ReplaceAllString(cleaned, " ")
	return strings.Join(strings.Fields(stdhtml.UnescapeString(cleaned)), " ")
}

/*
 * tempmailEEParseContent 解析详情 content：
 * 1) 平台已做 HTML 实体转义（= 写成 &#61;、+ 写成 &#43;、/ 写成 &#47;），
 *    先反转义；之后补充标准 HTML 反转义（&lt; &gt; &amp; &quot; 等）。
 * 2) content 是 MIME multipart 原文（含顶部边界装饰头），按首个边界行
 *    切块，各 part 依据 Content-Transfer-Encoding 做 base64 /
 *    quoted-printable 解码，text part 入文本、html part 入 HTML。
 * @param raw 原始 content 字符串
 * @returns (纯文本正文, HTML 正文)，两者之一为空时互为兜底合成
 */
func tempmailEEParseContent(raw string) (string, string) {
	payload := stdhtml.UnescapeString(raw)
	replacer := strings.NewReplacer("&#61;", "=", "&#43;", "+", "&#47;", "/")
	payload = replacer.Replace(payload)
	payload = strings.ReplaceAll(payload, "\r\n", "\n")

	lines := strings.Split(payload, "\n")
	boundary := teeBoundaryOf(lines)
	text, htmlStr := "", ""
	if boundary != "" {
		text, htmlStr = teeParts(lines, boundary)
	}
	if boundary == "" || (text == "" && htmlStr == "") {
		/*
		 * 无有效 multipart 结构（如邮件 body 仅单个 part）：
		 * 整个 content 去壳后作为正文，避免丢信。
		 */
		text, htmlStr = teeSingleton(payload)
	}
	if text == "" && htmlStr != "" {
		text = teeHTMLToText(htmlStr)
	}
	if htmlStr == "" && text != "" {
		htmlStr = "<html><body><pre>" + stdhtml.EscapeString(text) + "</pre></body></html>"
	}
	return text, htmlStr
}

/*
 * teeSingleton 单 part（无边界或拆不出内容）降级解析：
 * 依次按「头部区隔（首个空行之后）→ 整体」取内容，
 * 并通过关键字识别 Content-Transfer-Encoding 做解码。
 */
func teeSingleton(payload string) (string, string) {
	body := strings.TrimSpace(payload)
	if !strings.Contains(body, "\n") {
		return body, ""
	}
	lines := strings.Split(payload, "\n")
	// 头部以 RFC822 形式出现（首行含冒号键值）时，正文从首个空行后开始
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			body = strings.Join(lines[i+1:], "\n")
			break
		}
		if i > 40 || (i >= 3 && !strings.Contains(ln, ":")) {
			break
		}
	}
	lower := strings.ToLower(payload)
	cte := ""
	for _, enc := range []string{"base64", "quoted-printable"} {
		if strings.Contains(lower, enc) {
			cte = enc
		}
	}
	return teeDecodePart(body, cte), ""
}

/* teeBoundaryOf 扫描首块寻找边界行（默认 multipart 边界行处于块首） */
func teeBoundaryOf(lines []string) string {
	for i := 0; i < len(lines) && i < 120; i++ {
		if strings.HasPrefix(lines[i], "--") && len(lines[i]) > 2 {
			return strings.TrimPrefix(strings.TrimSuffix(lines[i], "\r"), "--")
		}
	}
	return ""
}

/* teeParts 按 boundary 拆分 multipart 并解码归并 text/html 两个 part */
func teeParts(lines []string, boundary string) (string, string) {
	var text, htmlStr string
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "--"+boundary) {
			continue
		}
		if strings.HasPrefix(lines[i], "--"+boundary+"--") {
			break
		}
		headers := map[string]string{}
		j := i + 1
		/* part 头部：直到首个空行（RFC 空行在 Content-* 头之后） */
		for j < len(lines) && lines[j] != "" && !strings.HasPrefix(lines[j], "--"+boundary) {
			ln := lines[j]
			if k := strings.IndexByte(ln, ':'); k > 0 {
				headers[strings.ToLower(strings.TrimSpace(ln[:k]))] = strings.TrimSpace(ln[k+1:])
			}
			j++
		}
		if j < len(lines) && lines[j] == "" {
			j++
		}
		/* part 正文：到下一个边界行为止，内部空行属于正文内容 */
		body := []string{}
		for j < len(lines) && !strings.HasPrefix(lines[j], "--"+boundary) {
			body = append(body, lines[j])
			j++
		}
		text, htmlStr = teeMergePart(body, headers, text, htmlStr)
		i = j - 1
	}
	return text, htmlStr
}

/* teeMergePart 单个 part 的解码归并：按 content-type 归类，缺省回退 text 槽 */
func teeMergePart(body []string, headers map[string]string, text, htmlStr string) (string, string) {
	ct := strings.ToLower(headers["content-type"])
	if i := strings.IndexByte(ct, ';'); i > 0 {
		ct = ct[:i]
	}
	cte := strings.ToLower(headers["content-transfer-encoding"])
	content := strings.Join(body, "\n")
	if strings.Contains(ct, "text/plain") {
		if text == "" {
			text = teeDecodePart(content, cte)
		}
		return text, htmlStr
	}
	if strings.Contains(ct, "text/html") {
		if htmlStr == "" {
			htmlStr = teeDecodePart(content, cte)
		}
		return text, htmlStr
	}
	if text == "" {
		text = teeDecodePart(content, cte)
	}
	return text, htmlStr
}

/* teeDecodePart 按 Content-Transfer-Encoding 解码 part 内容 */
func teeDecodePart(data, cte string) string {
	data = strings.TrimSpace(data)
	switch strings.ToLower(cte) {
	case "base64":
		joined := strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
				return -1
			}
			return r
		}, data)
		if b, err := base64.StdEncoding.DecodeString(joined); err == nil {
			return strings.TrimSpace(string(b))
		}
	case "quoted-printable":
		if b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(data))); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return data
}
