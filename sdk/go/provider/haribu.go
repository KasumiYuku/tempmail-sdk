package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	gohtml "golang.org/x/net/html"
)

/*
 * haribu.net — https://haribu.net（Tempail 类临时邮箱）
 * 平台协议（与前端 /js/main.min.js 对齐，2026-09 实测实锤）：
 *   创建邮箱: GET https://haribu.net/en/ → 首页内联脚本注入 var oturum="<会话令牌>" 与
 *             var tarih="<unix 秒时间戳>"；Set-Cookie 同时下发 PHPSESSID（oturum 的
 *             Set-Cookie 弹在 tempemail.eu 域上，页面内联值是主要来源）。
 *             自分配邮箱在 <input id="eposta_adres" value="xxx@yevme.com">。
 *   邮件列表: POST https://haribu.net/en/api-kontrol/，表单 oturum=<令牌>&tarih=<首页注入值>；
 *             响应为注入 #epostalar 的 HTML 片段，邮件行是 ul.mailler 下的 li（带
 *              id="mail_<id>" 与 href）。无新邮件时返回 HTTP 304（body 为空），按空列表处理。
 *             注意：对 api-kontrol 直接 GET 会被 302 到首页，SDK 旧实现即错在此处。
 *   邮件正文: POST https://haribu.net/en/api-oku/，表单 oturum=<令牌>&veri[]=<发件人>
 *             &veri[]=<邮件ID>（前端 mail_oku(t,a) 的双元素数组 [a,t]）；
 *             响应为注入 #eposta_oku 的 HTML 片段。
 * 域名: yevme.com（平台侧该域当前无 MX，属平台故障，SDK 无法修复；协议本身已按前端对齐）。
 */

const haribuBase = "https://haribu.net"

/* haribuKontrolURL haribu 邮件列表接口 */
const haribuKontrolURL = haribuBase + "/en/api-kontrol/"

/* haribuOkuURL haribu 邮件正文接口 */
const haribuOkuURL = haribuBase + "/en/api-oku/"

/* haribu session 令牌前缀，用于区分不同渠道的 token 格式 */
const haribuTokPrefix = "haribu1:"

/* haribuSess 存储 haribu 会话信息，序列化后作为 token */
type haribuSess struct {
	/* CookieHdr 合并后的 Cookie 头（含 PHPSESSID，可能含 oturum） */
	CookieHdr string `json:"c"`
	/* Oturum 首页内联脚本注入的会话令牌 */
	Oturum string `json:"o"`
	/* Tarih 首页内联脚本注入的时间戳（unix 秒，平台为会话固定注入值） */
	Tarih string `json:"t"`
}

/* haribuMailRow haribu 邮件列表单行 */
type haribuMailRow struct {
	ID      string
	From    string
	Subject string
	Date    string
}

var (
	/* 匹配邮箱地址输入框：<input id="eposta_adres" value="xxx@yyy.com"> */
	haribuEmailInputRe = regexp.MustCompile(`(?i)<input[^>]+id\s*=\s*["']eposta_adres["'][^>]+value\s*=\s*["']([^"']+)["']`)
	/* 备选匹配顺序（value 在 id 前面的情况） */
	haribuEmailInputRe2 = regexp.MustCompile(`(?i)<input[^>]+value\s*=\s*["']([^"']+@[^"']+)["'][^>]+id\s*=\s*["']eposta_adres["']`)

	/* 列表 HTML 片段中以 li 为单位（表头 li 无 href 会被过滤） */
	haribuMailLiRe = regexp.MustCompile(`(?is)<li[^>]*>([\s\S]*?)</li>`)
	/* 行内第一个 href，即前端 mail_oku 发送的邮件标识 */
	haribuHrefRe = regexp.MustCompile(`(?i)href\s*=\s*["']\s*([^"'\s][^"']*)["']`)
	/* 行内 div.gonderen = 发件人 */
	haribuFromRe = regexp.MustCompile(`(?is)<div\s+class\s*=\s*["'][^"']*\bgonderen\b[^"']*["'][^>]*>([\s\S]*?)</div>`)
	/* 行内 div.baslik = 主题 */
	haribuSubjectRe = regexp.MustCompile(`(?is)<div\s+class\s*=\s*["'][^"']*\bbaslik\b[^"']*["'][^>]*>([\s\S]*?)</div>`)
	/* 行内 div.zaman = 时间 */
	haribuDateRe = regexp.MustCompile(`(?is)<div\s+class\s*=\s*["'][^"']*\bzaman\b[^"']*["'][^>]*>([\s\S]*?)</div>`)

	/* 剥离 HTML 标签 */
	haribuTagRe = regexp.MustCompile(`<[^>]*>`)
	/* oku 响应片段中外层容器开闭标签（解析器兜底提取时定位） */
	haribuBodyOpenRe  = regexp.MustCompile(`(?is)<(div|section)\s+class\s*=\s*["'][^"']*\b(eposta_acilir|eposta_oku|eposta_metin)\b[^"']*["'][^>]*>`)
	haribuBodyCloseRe = regexp.MustCompile(`(?is)</(div|section)>`)
)

/*
 * haribuExtractBodyHTML 使用 HTML 解析器提取邮件正文 div 的完整内部 HTML，
 * 避免非贪婪正则在嵌套 div 时截断正文。支持多种 class/id 名称。
 */
func haribuExtractBodyHTML(page string) string {
	doc, err := gohtml.Parse(strings.NewReader(page))
	if err != nil {
		return ""
	}
	targets := []string{"mail_icerik", "mail_eposta", "icerik", "mail-content", "message-body", "email-body"}
	var result string
	var walk func(*gohtml.Node)
	walk = func(n *gohtml.Node) {
		if result != "" {
			return
		}
		if n.Type == gohtml.ElementNode && n.Data == "div" {
			for _, a := range n.Attr {
				if a.Key == "class" || a.Key == "id" {
					for _, target := range targets {
						if strings.Contains(a.Val, target) {
							var sb strings.Builder
							for c := n.FirstChild; c != nil; c = c.NextSibling {
								gohtml.Render(&sb, c)
							}
							result = strings.TrimSpace(sb.String())
							return
						}
					}
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

/* haribuHTTPClient 获取 haribu 专用 HTTP 客户端（无 cookie jar，手动管理 cookie） */
func haribuHTTPClient() tls_client.HttpClient {
	if HTTPClientNoCookieJar != nil {
		return HTTPClientNoCookieJar()
	}
	return HTTPClient()
}

/* haribuPageHeaders 设置页面/API 请求所需的浏览器模拟请求头 */
func haribuPageHeaders(req *http.Request, referer string, xhr bool) {
	req.Header.Set("User-Agent", getCurrentUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("DNT", "1")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	if xhr {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

/* haribuCookieMap 解析 Cookie 头字符串为 map */
func haribuCookieMap(hdr string) map[string]string {
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

/* haribuMergeCookies 合并已有 cookie 和响应中的新 cookie（按名覆盖去重） */
func haribuMergeCookies(prev string, cookies []*http.Cookie) string {
	m := haribuCookieMap(prev)
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

/* haribuCookieValue 从 Cookie 头字符串中取出指定 cookie 名的值 */
func haribuCookieValue(hdr, name string) string {
	m := haribuCookieMap(hdr)
	return m[name]
}

/* haribuFindJSVar 从页面内联脚本中提取字符串变量：var name="value" */
func haribuFindJSVar(page, name string) string {
	re := regexp.MustCompile(`(?is)var\s+` + regexp.QuoteMeta(name) + `\s*=\s*["']([^"']+)["']`)
	if m := re.FindStringSubmatch(page); len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

/* haribuEncodeSess 将会话信息编码为 token 字符串 */
func haribuEncodeSess(s *haribuSess) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return haribuTokPrefix + base64.StdEncoding.EncodeToString(b), nil
}

/* haribuDecodeSess 从 token 字符串解码会话信息 */
func haribuDecodeSess(tok string) (*haribuSess, error) {
	if !strings.HasPrefix(tok, haribuTokPrefix) {
		return nil, fmt.Errorf("haribu: 无效的会话令牌")
	}
	raw, err := base64.StdEncoding.DecodeString(tok[len(haribuTokPrefix):])
	if err != nil {
		return nil, fmt.Errorf("haribu: 无效的会话令牌")
	}
	var s haribuSess
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("haribu: 无效的会话令牌")
	}
	if s.CookieHdr == "" || s.Oturum == "" {
		return nil, fmt.Errorf("haribu: 会话令牌中缺少必要字段")
	}
	return &s, nil
}

/* haribuExtractEmail 从 HTML 中提取邮箱地址（匹配 id="eposta_adres" 的 input 元素），失败返回空串 */
func haribuExtractEmail(htmlStr string) string {
	/* 尝试标准顺序：id 在 value 前 */
	if m := haribuEmailInputRe.FindStringSubmatch(htmlStr); len(m) >= 2 {
		addr := strings.TrimSpace(html.UnescapeString(m[1]))
		if addr != "" && strings.Contains(addr, "@") {
			return addr
		}
	}
	/* 备选顺序：value 在 id 前 */
	if m := haribuEmailInputRe2.FindStringSubmatch(htmlStr); len(m) >= 2 {
		addr := strings.TrimSpace(html.UnescapeString(m[1]))
		if addr != "" && strings.Contains(addr, "@") {
			return addr
		}
	}
	return ""
}

/* haribuStripTags 移除 HTML 标签，返回纯文本（标签位置折叠为空格） */
func haribuStripTags(s string) string {
	return strings.TrimSpace(haribuTagRe.ReplaceAllString(s, " "))
}

/*
 * HaribuGenerate 创建 haribu 临时邮箱
 * 流程: GET https://haribu.net/en/ → 捕获 PHPSESSID 等 Cookie，提取内联脚本的
 *       oturum/tarih 变量与自分配邮箱地址，全部编码进 token。
 */
func HaribuGenerate() (*CreatedMailbox, error) {
	client := haribuHTTPClient()

	/* 第一步：GET 首页获取 session cookie、内联变量与邮箱地址 */
	req, err := http.NewRequest("GET", haribuBase+"/en/", nil)
	if err != nil {
		return nil, err
	}
	haribuPageHeaders(req, "", false)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "haribu home"); err != nil {
		return nil, err
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	page := string(body)

	emailAddr := haribuExtractEmail(page)
	if emailAddr == "" {
		return nil, fmt.Errorf("haribu: 未能从页面中提取邮箱地址")
	}

	cookieHdr := haribuMergeCookies("", resp.Cookies())
	oturum := haribuFindJSVar(page, "oturum")
	if oturum == "" {
		/* 页面脚本未注入会话令牌，尝试从 cookie 中兜底 */
		oturum = haribuCookieValue(cookieHdr, "oturum")
	}
	if oturum == "" {
		return nil, fmt.Errorf("haribu: 未能从页面中提取会话令牌 oturum")
	}

	tarih := haribuFindJSVar(page, "tarih")

	/* 编码会话信息为 token */
	tok, err := haribuEncodeSess(&haribuSess{
		CookieHdr: cookieHdr,
		Oturum:    oturum,
		Tarih:     tarih,
	})
	if err != nil {
		return nil, err
	}

	return &CreatedMailbox{
		Channel: "haribu",
		Email:   emailAddr,
		Token:   tok,
	}, nil
}

/*
 * HaribuGetEmails 获取 haribu 邮件列表
 * 流程: GET 首页做会话核验并刷新 tarih → POST api-kontrol 拉邮件列表 HTML 片段
 *       （304/空 按空列表处理）→ 逐封 POST api-oku 拉正文。
 */
func HaribuGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("haribu: 邮箱地址为空")
	}
	if token == "" {
		return nil, fmt.Errorf("haribu: 会话令牌为空")
	}

	sess, err := haribuDecodeSess(token)
	if err != nil {
		return nil, err
	}
	client := haribuHTTPClient()

	/*
	 * 会话核验：GET 首页成功后，页面上的邮箱与入参邮箱不一致说明会话
	 * 已被新的会话顶掉（平台同一 cookie 会话仅绑定一个邮箱），提示重建。
	 * 网络失败则跳过核验，继续使用已存参数拉列表。
	 */
	pageEmail, pageTarih, checkErr := haribuCheckSession(client, sess)
	if checkErr == nil && pageEmail != "" && !strings.EqualFold(pageEmail, email) {
		return nil, fmt.Errorf("haribu: 会话邮箱不匹配（页面=%s 入参=%s），会话可能已被顶掉，请重新创建邮箱", pageEmail, email)
	}
	if pageTarih != "" {
		sess.Tarih = pageTarih
	}

	/* POST api-kontrol 获取邮件列表 HTML 片段 */
	fragment, status, err := haribuPostKontrol(client, sess)
	if err != nil {
		return nil, err
	}
	/* HTTP 304 或空 body 均表示无新邮件 */
	if status == 304 || fragment == "" {
		return []NormEmail{}, nil
	}

	rows := haribuParseMailRows(fragment)
	if len(rows) == 0 {
		return []NormEmail{}, nil
	}

	out := make([]NormEmail, 0, len(rows))
	for _, row := range rows {
		/* 逐封拉取正文，失败不阻断（保留列表字段） */
		htmlBody, textBody := haribuFetchMailBody(client, sess, row)

		flat := map[string]interface{}{
			"id":      row.ID,
			"from":    row.From,
			"to":      email,
			"subject": row.Subject,
			"date":    row.Date,
			"html":    htmlBody,
			"text":    textBody,
			"isRead":  false,
		}
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}

/* haribuCheckSession 访问首页核验会话，返回当前会话绑定的邮箱与最新 tarih */
func haribuCheckSession(client tls_client.HttpClient, sess *haribuSess) (string, string, error) {
	req, err := http.NewRequest("GET", haribuBase+"/en/", nil)
	if err != nil {
		return "", "", err
	}
	haribuPageHeaders(req, "", false)
	req.Header.Set("Cookie", sess.CookieHdr)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("haribu: 会话核验请求失败 %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	page := string(raw)

	pageEmail := haribuExtractEmail(page)
	pageTarih := haribuFindJSVar(page, "tarih")
	/* 若平台轮换会话 cookie，刷新 cookie 头 */
	if merged := haribuMergeCookies(sess.CookieHdr, resp.Cookies()); merged != "" {
		sess.CookieHdr = merged
	}
	return pageEmail, pageTarih, nil
}

/* haribuPostKontrol POST api-kontrol 拉取邮件列表 HTML 片段 */
func haribuPostKontrol(client tls_client.HttpClient, sess *haribuSess) (string, int, error) {
	form := make(url.Values)
	form["oturum"] = []string{sess.Oturum}
	form["tarih"] = []string{sess.Tarih}
	return haribuPostForm(client, haribuKontrolURL, sess, form)
}

/* haribuPostForm 发送 API 表单 POST 并返回响应体与状态码 */
func haribuPostForm(client tls_client.HttpClient, apiURL string, sess *haribuSess, form url.Values) (string, int, error) {
	req, err := http.NewRequest("POST", apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	haribuPageHeaders(req, haribuBase+"/en/", true)
	req.Header.Set("Cookie", sess.CookieHdr)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Origin", haribuBase)

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, err
	}
	return string(raw), resp.StatusCode, nil
}

/* haribuParseMailRows 从 api-kontrol 的 HTML 片段中提取邮件行 */
func haribuParseMailRows(fragment string) []haribuMailRow {
	rows := make([]haribuMailRow, 0, 4)
	items := haribuMailLiRe.FindAllStringSubmatch(fragment, -1)
	for _, item := range items {
		if len(item) < 2 {
			continue
		}
		content := item[1]

		/* 行内第一个 href 即为该邮件的 mailID */
		row := haribuMailRow{}
		if m := haribuHrefRe.FindStringSubmatch(content); len(m) >= 2 {
			row.ID = strings.TrimSpace(html.UnescapeString(m[1]))
		}
		if row.ID == "" {
			continue
		}

		if m := haribuFromRe.FindStringSubmatch(content); len(m) >= 2 {
			row.From = strings.TrimSpace(html.UnescapeString(haribuStripTags(m[1])))
		}
		if m := haribuSubjectRe.FindStringSubmatch(content); len(m) >= 2 {
			row.Subject = strings.TrimSpace(html.UnescapeString(haribuStripTags(m[1])))
		}
		if m := haribuDateRe.FindStringSubmatch(content); len(m) >= 2 {
			row.Date = strings.TrimSpace(html.UnescapeString(haribuStripTags(m[1])))
		}
		rows = append(rows, row)
	}
	return rows
}

/* haribuFetchMailBody 通过 api-oku 拉取单封邮件正文，返回 HTML 与纯文本 */
func haribuFetchMailBody(client tls_client.HttpClient, sess *haribuSess, row haribuMailRow) (string, string) {
	if row.ID == "" {
		return "", ""
	}

	/* 前端 mail_oku(t, a) 以双元素数组传参：veri[] = [发件人, 邮件ID] */
	form := make(url.Values)
	form["oturum"] = []string{sess.Oturum}
	form["veri[]"] = []string{row.From, row.ID}

	fragment, status, err := haribuPostForm(client, haribuOkuURL, sess, form)
	if err != nil {
		return "", ""
	}
	if status != 200 || fragment == "" {
		return "", ""
	}

	htmlBody := haribuExtractMailHTML(fragment)
	if htmlBody == "" {
		return "", ""
	}
	return htmlBody, haribuStripTags(htmlBody)
}

/* haribuExtractMailHTML 从 api-oku 响应片段中提取邮件正文 HTML */
func haribuExtractMailHTML(fragment string) string {
	/* 优先使用 HTML 解析器按正文容器 class 提取完整内部 HTML */
	if body := haribuExtractBodyHTML(fragment); body != "" {
		return body
	}

	/* 解析器未命中：改为剥离外层渲染容器，取 .eposta_acilir / #eposta_oku 内的内容 */
	if idx := haribuBodyOpenRe.FindStringIndex(fragment); idx != nil {
		start := idx[1]
		if closeIdx := haribuBodyCloseRe.FindStringIndex(fragment[start:]); closeIdx != nil {
			end := start + closeIdx[0]
			if inner := strings.TrimSpace(fragment[start:end]); inner != "" {
				return inner
			}
		}
	}

	/* 兜底：直接返回原始片段（NormalizeMap 侧仍会保留可见内容） */
	return strings.TrimSpace(fragment)
}
