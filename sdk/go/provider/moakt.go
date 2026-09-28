package provider

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	gohtml "golang.org/x/net/html"
)

// moakt.com：凭证为 tm_session 等 Cookie；先 GET 语言首页再 GET 收件箱解析 #email-address；
// 邮件列表解析 /{locale}/email/{uuid} 链接，详情 GET .../html 解析 .message-container。

const moaktOrigin = "https://www.moakt.com"

type moaktSess struct {
	Locale    string `json:"l"`
	CookieHdr string `json:"c"`
}

const moaktTokPrefix = "mok1:"

var (
	moaktEmailDivRe   = regexp.MustCompile(`(?is)<div\s+id="email-address"\s*>([^<]+)</div>`)
	moaktDomainOptRe  = regexp.MustCompile(`(?is)<option\s+value="([^"]+)">\s*@[^<]+</option>`)
	moaktMailDomainRe = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
	moaktHrefEmailRe  = regexp.MustCompile(
		`href="(/[^"]+/email/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})"`,
	)
	moaktTitleRe    = regexp.MustCompile(`(?is)<li\s+class="title"\s*>([^<]*)</li>`)
	moaktDateRe     = regexp.MustCompile(`(?is)<li\s+class="date"[^>]*>[\s\S]*?<span[^>]*>([^<]+)</span>`)
	moaktSenderRe   = regexp.MustCompile(`(?is)<li\s+class="sender"[^>]*>[\s\S]*?<span[^>]*>([\s\S]*?)</span>\s*</li>`)
	moaktFromAddrRe = regexp.MustCompile(`<([a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,})>`)
	// /plain 视图：正文位于 div.email-body>pre 中，内容为完整的 HTML 转义文本，
	// pre 的子元素可能是转义后的文本或已解析的标签，需整体渲染后反转义
	moaktPlainBodyRe = regexp.MustCompile(`(?is)<div\s+class="email-body"[^>]*>\s*<pre[^>]*>`)
	moaktClosePreRe  = regexp.MustCompile(`</pre>`)
	// /html 视图的附件区块：每条附件依次包含 attach_name/attach_type/attach_size
	// 与 /{locale}/email/{id}/attachment/{uuid} 下载链接
	moaktAttachGroupRe = regexp.MustCompile(`(?is)<ul[^>]*>((?:\s|<li)[\s\S]*?)</ul>`)
	moaktAttachLiRe    = regexp.MustCompile(`(?is)<li[^>]*>([\s\S]*?)</li>`)
	moaktAttachNameRe  = regexp.MustCompile(`(?is)<(?:span|div)[^>]*class="[^"]*\battach_name\b[^"]*"[^>]*>([\s\S]*?)</(?:span|div)>`)
	moaktAttachTypeRe  = regexp.MustCompile(`(?is)<(?:span|div)[^>]*class="[^"]*\battach_type\b[^"]*"[^>]*>([\s\S]*?)</(?:span|div)>`)
	moaktAttachSizeRe  = regexp.MustCompile(`(?is)<(?:span|div)[^>]*class="[^"]*\battach_size\b[^"]*"[^>]*>([\s\S]*?)</(?:span|div)>`)
	moaktAttachLinkRe  = regexp.MustCompile(`href="(/[^"]+/attachment/[0-9a-f-]{36})"`)
)

/*
 * moaktExtractBodyText 在 /plain 视图中定位 div.email-body>pre 中的正文。
 * 平台将正文整体 HTML 转义后放入 pre（如 &lt;br /&gt;），此处直接截取
 * pre 内容并 UnescapeString 还原为原文；转义保证了正文中的 </pre> 等
 * 字面量不会干扰结束定位。
 */
func moaktExtractBodyText(page string) string {
	m := moaktPlainBodyRe.FindStringIndex(page)
	if m == nil {
		return ""
	}
	rest := page[m[1]:]
	if end := moaktClosePreRe.FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	return strings.TrimSpace(html.UnescapeString(rest))
}

/*
 * moaktExtractBodyHTML 在 /html 视图中提取 email-body div 的完整内部 HTML，
 * 避免非贪婪正则在嵌套 div 时截断正文。该视图的正文容器可能为空，
 * 需与 /plain 视图提取的纯文本互为回退。
 */
func moaktExtractBodyHTML(page string) string {
	doc, err := gohtml.Parse(strings.NewReader(page))
	if err != nil {
		return ""
	}
	var result string
	var walk func(*gohtml.Node)
	walk = func(n *gohtml.Node) {
		if result != "" {
			return
		}
		if n.Type == gohtml.ElementNode && n.Data == "div" {
			for _, a := range n.Attr {
				if a.Key == "class" && strings.Contains(a.Val, "email-body") {
					var sb strings.Builder
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						gohtml.Render(&sb, c)
					}
					result = strings.TrimSpace(sb.String())
					return
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return result
}

func moaktRequestParts(domain *string) (string, string) {
	if domain == nil {
		return "zh", ""
	}
	s := strings.TrimSpace(*domain)
	if s == "" {
		return "zh", ""
	}
	if strings.ContainsAny(s, "/?#\\") {
		return "zh", ""
	}
	if moaktMailDomainRe.MatchString(s) {
		return "zh", strings.TrimPrefix(strings.ToLower(s), "@")
	}
	return s, ""
}

func moaktParseServerDomains(page string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, m := range moaktDomainOptRe.FindAllStringSubmatch(page, -1) {
		if len(m) < 2 {
			continue
		}
		d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(m[1])), "@")
		if d != "" {
			out[d] = struct{}{}
		}
	}
	return out
}

func moaktRandomLocal(n int) (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = chars[int(b)%len(chars)]
	}
	return string(out), nil
}

func moaktEmailDomain(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(email[at+1:]))
}

func moaktCookieMap(hdr string) map[string]string {
	m := make(map[string]string)
	for _, part := range strings.Split(hdr, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.Index(part, "=")
		if i <= 0 || i >= len(part)-1 {
			continue
		}
		k := strings.TrimSpace(part[:i])
		v := strings.TrimSpace(part[i+1:])
		if k != "" {
			m[k] = v
		}
	}
	return m
}

func moaktMergeCookies(prev string, cookies []*http.Cookie) string {
	m := moaktCookieMap(prev)
	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}
		m[c.Name] = c.Value
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, "; ")
}

func moaktEncodeSess(s *moaktSess) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return moaktTokPrefix + base64.StdEncoding.EncodeToString(b), nil
}

func moaktDecodeSess(tok string) (*moaktSess, error) {
	if !strings.HasPrefix(tok, moaktTokPrefix) {
		return nil, fmt.Errorf("moakt: invalid session token")
	}
	raw, err := base64.StdEncoding.DecodeString(tok[len(moaktTokPrefix):])
	if err != nil {
		return nil, fmt.Errorf("moakt: invalid session token")
	}
	var s moaktSess
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("moakt: invalid session token")
	}
	if s.CookieHdr == "" || s.Locale == "" {
		return nil, fmt.Errorf("moakt: invalid session token")
	}
	return &s, nil
}

func moaktHTTPClient() tls_client.HttpClient {
	if HTTPClientNoCookieJar != nil {
		return HTTPClientNoCookieJar()
	}
	return HTTPClient()
}

func moaktSetPageHeaders(req *http.Request, referer string) {
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("DNT", "1")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", referer)
	req.Header.Set("Upgrade-Insecure-Requests", "1")
}

func moaktParseInboxEmail(htmlStr string) (string, error) {
	m := moaktEmailDivRe.FindStringSubmatch(htmlStr)
	if len(m) < 2 {
		return "", fmt.Errorf("moakt: email-address not found")
	}
	addr := strings.TrimSpace(html.UnescapeString(m[1]))
	if addr == "" {
		return "", fmt.Errorf("moakt: empty email-address")
	}
	return addr, nil
}

func moaktStripTags(s string) string {
	re := regexp.MustCompile(`<[^>]+>`)
	return strings.TrimSpace(re.ReplaceAllString(s, " "))
}

func moaktListMailIDs(htmlStr string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, m := range moaktHrefEmailRe.FindAllStringSubmatch(htmlStr, -1) {
		if len(m) < 2 {
			continue
		}
		path := m[1]
		if strings.Contains(path, "/delete") {
			continue
		}
		id := path[strings.LastIndex(path, "/")+1:]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

/*
 * moaktExtractAttachments 解析详情页 message-attachments 区块中的附件列表。
 * 每条附件携带 attach_name/attach_type/attach_size 元数据与
 * /{locale}/email/{id}/attachment/{uuid} 下载链接，返回
 * normalizeAttachments 接受的 raw["attachments"] 数组形式
 * （元素含 filename/size/contentType/url）。
 */
func moaktExtractAttachments(page string, origin string, locale string) []interface{} {
	idx := strings.Index(page, "message-attachments")
	if idx < 0 {
		return nil
	}
	block := page[idx:]
	/* 附件区块固定以 <ul> 列表呈现，避免松散 li 匹配扩散到区块外 */
	ul := moaktAttachGroupRe.FindStringSubmatch(block)
	if len(ul) < 2 {
		return nil
	}
	var atts []interface{}
	for _, li := range moaktAttachLiRe.FindAllStringSubmatch(ul[1], -1) {
		name := moaktStripTags(html.UnescapeString(moaktFirstMatch(moaktAttachNameRe, li[1])))
		if name == "" {
			continue
		}
		att := map[string]interface{}{"filename": name}
		if ct := moaktStripTags(strings.TrimSpace(moaktFirstMatch(moaktAttachTypeRe, li[1]))); ct != "" {
			att["contentType"] = ct
		}
		if sizeStr := strings.TrimSpace(moaktFirstMatch(moaktAttachSizeRe, li[1])); sizeStr != "" {
			att["size"] = moaktAttachSizeBytes(moaktStripTags(sizeStr))
		}
		if lk := moaktAttachLinkRe.FindStringSubmatch(li[1]); len(lk) >= 2 {
			target := lk[1]
			if !strings.HasPrefix(target, "/") {
				target = "/" + target
			}
			att["url"] = origin + target
		} else if idURL := moaktAttachMailID(li[1]); idURL != "" {
			att["url"] = idURL
		}
		atts = append(atts, att)
	}
	return atts
}

/* moaktFirstMatch 返回首个分组捕获结果，无匹配时返回空串 */
func moaktFirstMatch(re *regexp.Regexp, src string) string {
	m := re.FindStringSubmatch(src)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

/*
 * moaktAttachSizeBytes 将 attach_size 的人类可读文本（如 "0.00 MB"、"128 KB"）
 * 换算为字节（float64，与 normalizeAttachments 的 size 提取约定一致）。
 */
func moaktAttachSizeBytes(s string) float64 {
	fields := strings.Fields(s)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(fields[1]) {
	case "KB":
		return v * 1024
	case "MB":
		return v * 1024 * 1024
	case "GB":
		return v * 1024 * 1024 * 1024
	}
	return 0
}

/*
 * moaktAttachMailID 从 /html 视图或 /plain 视图附件链接的兜底匹配中提取
 * 邮件 ID，用于仅拿到下载 uuid 的场景（按 moakt.com 的链接格式降级）。
 */
func moaktAttachMailID(li string) string {
	m := moaktHrefEmailRe.FindStringSubmatch(li)
	if len(m) < 2 {
		return ""
	}
	if !strings.Contains(li, "/attachment/") {
		return ""
	}
	return moaktOrigin + m[1]
}

/*
 * moaktFetchPage 以同一 Cookie 会话请求 moakt 页面，返回响应体字符串；
 * HTTP 状态非 2xx 或读取失败时返回错误。
 */
func moaktFetchPage(client tls_client.HttpClient, sess *moaktSess, detailURL string, referer string) (string, error) {
	req, err := http.NewRequest("GET", detailURL, nil)
	if err != nil {
		return "", err
	}
	moaktSetPageHeaders(req, referer)
	req.Header.Set("Cookie", sess.CookieHdr)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "moakt mail"); err != nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", err
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

/*
 * moaktAppendMail 解析某封邮件的详情页并追加一条归一化结果骨架。
 * /html 视图仅当正文容器非空时作为 HTML 候选，/plain 视图的纯文本
 * 连同附件区块一并解析，Text 恒由预归一化的纯文本填充。
 */
func moaktAppendMail(out *[]NormEmail, client tls_client.HttpClient, sess *moaktSess, id string, email string, inclHTML bool) bool {
	origin := moaktOrigin
	locEsc := url.PathEscape(sess.Locale)
	base := origin + "/" + locEsc

	plainPage, err := moaktFetchPage(client, sess, base+"/email/"+url.PathEscape(id)+"/plain", base+"/email/"+url.PathEscape(id))
	if err != nil {
		return false
	}
	raw := moaktParseMessageHTML(plainPage, id, email)
	text := moaktExtractBodyText(plainPage)
	if text != "" {
		raw["text"] = text
	}

	/* 附件区块仅存在于 /html 视图，HTML 候选亦一并获取 */
	htmlPage := ""
	if inclHTML {
		if hp, err := moaktFetchPage(client, sess, base+"/email/"+url.PathEscape(id)+"/html", base+"/email/"+url.PathEscape(id)); err == nil {
			htmlPage = hp
			if body := moaktExtractBodyHTML(hp); body != "" {
				raw["html"] = body
			}
		}
	}
	attPage := htmlPage
	if attPage == "" {
		attPage = plainPage
	}
	if atts := moaktExtractAttachments(attPage, origin, locEsc); len(atts) > 0 {
		raw["attachments"] = atts
	}

	/* /html 视图字段脚注缺失时从 /plain 视图兜底解析 */
	outRaw := raw
	if htmlPage != "" {
		for _, key := range []string{"subject", "date", "from"} {
			if v, ok := raw[key].(string); !ok || v == "" {
				if alt := moaktParseMessageHTML(htmlPage, id, email); alt[key] != nil {
					outRaw[key] = alt[key]
				}
			}
		}
	}

	*out = append(*out, NormalizeMap(outRaw, email))
	return true
}

func moaktParseMessageHTML(page string, id string, recipient string) map[string]interface{} {
	raw := map[string]interface{}{"id": id, "to": recipient}
	if sm := moaktTitleRe.FindStringSubmatch(page); len(sm) >= 2 {
		raw["subject"] = strings.TrimSpace(html.UnescapeString(sm[1]))
	}
	if sm := moaktDateRe.FindStringSubmatch(page); len(sm) >= 2 {
		raw["date"] = strings.TrimSpace(html.UnescapeString(sm[1]))
	}
	if sm := moaktSenderRe.FindStringSubmatch(page); len(sm) >= 2 {
		inner := html.UnescapeString(sm[1])
		raw["from"] = strings.TrimSpace(moaktStripTags(inner))
		if em := moaktFromAddrRe.FindStringSubmatch(inner); len(em) >= 2 {
			raw["from"] = strings.TrimSpace(em[1])
		}
	}
	return raw
}

// MoaktGenerate GET /{locale} 再 POST /{locale}/inbox（random=1，不跟重定向取 tm_session）
// 再 GET /{locale}/inbox 解析 #email-address；token 内为 Cookie 快照。
// opts.Domain 为语言路径（如 zh、en），默认 zh。
func MoaktGenerate(domain *string) (*CreatedMailbox, error) {
	loc, mailDomain := moaktRequestParts(domain)
	base := moaktOrigin + "/" + url.PathEscape(loc)
	inbox := base + "/inbox"
	client := moaktHTTPClient()

	req, err := http.NewRequest("GET", base, nil)
	if err != nil {
		return nil, err
	}
	moaktSetPageHeaders(req, base)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "moakt home"); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	cookieHdr := moaktMergeCookies("", resp.Cookies())

	form := url.Values{}
	if mailDomain != "" {
		domains := moaktParseServerDomains(string(body))
		if _, ok := domains[mailDomain]; !ok {
			return nil, fmt.Errorf("moakt: unsupported domain %s", mailDomain)
		}
		local, err := moaktRandomLocal(12)
		if err != nil {
			return nil, fmt.Errorf("moakt: random local failed: %w", err)
		}
		form.Set("setemail", "")
		form.Set("username", local)
		form.Set("domain", mailDomain)
		form.Set("preferred_domain", "")
	} else {
		form.Set("random", "1")
	}

	// POST /{locale}/inbox with random=1，不跟随重定向以获取 tm_session cookie
	noRedirectClient := HTTPClientNoRedirect()
	req2, err := http.NewRequest("POST", inbox, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	moaktSetPageHeaders(req2, base)
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("Cookie", cookieHdr)
	resp2, err := noRedirectClient.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	_, _ = io.Copy(io.Discard, resp2.Body)
	cookieHdr = moaktMergeCookies(cookieHdr, resp2.Cookies())

	if moaktCookieMap(cookieHdr)["tm_session"] == "" {
		return nil, fmt.Errorf("moakt: missing tm_session cookie")
	}

	// GET /{locale}/inbox 解析邮箱地址
	req3, err := http.NewRequest("GET", inbox, nil)
	if err != nil {
		return nil, err
	}
	moaktSetPageHeaders(req3, base)
	req3.Header.Set("Cookie", cookieHdr)
	resp3, err := client.Do(req3)
	if err != nil {
		return nil, err
	}
	defer resp3.Body.Close()
	if err := CheckHTTPStatus(resp3, "moakt inbox"); err != nil {
		return nil, err
	}
	body, err = io.ReadAll(resp3.Body)
	if err != nil {
		return nil, err
	}
	cookieHdr = moaktMergeCookies(cookieHdr, resp3.Cookies())
	htmlStr := string(body)
	emailAddr, err := moaktParseInboxEmail(htmlStr)
	if err != nil {
		return nil, err
	}
	if mailDomain != "" && moaktEmailDomain(emailAddr) != mailDomain {
		return nil, fmt.Errorf("moakt: domain mismatch expected=%s actual=%s", mailDomain, moaktEmailDomain(emailAddr))
	}

	tok, err := moaktEncodeSess(&moaktSess{Locale: loc, CookieHdr: cookieHdr})
	if err != nil {
		return nil, err
	}
	return &CreatedMailbox{
		Channel: "moakt",
		Email:   emailAddr,
		Token:   tok,
	}, nil
}

// MoaktGetEmails 拉取收件箱链接后逐封请求 .../email/{id}/html 与 .../email/{id}/plain 详情，
// 正文与附件解析规则见 moaktAppendMail。
func MoaktGetEmails(email, token string) ([]NormEmail, error) {
	sess, err := moaktDecodeSess(token)
	if err != nil {
		return nil, err
	}
	loc := sess.Locale
	inbox := moaktOrigin + "/" + url.PathEscape(loc) + "/inbox"
	client := moaktHTTPClient()

	req, err := http.NewRequest("GET", inbox, nil)
	if err != nil {
		return nil, err
	}
	moaktSetPageHeaders(req, moaktOrigin+"/"+url.PathEscape(loc))
	req.Header.Set("Cookie", sess.CookieHdr)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "moakt inbox"); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	ids := moaktListMailIDs(string(body))
	out := make([]NormEmail, 0, len(ids))
	for _, id := range ids {
		moaktAppendMail(&out, client, sess, id, email, true)
	}
	return out, nil
}
