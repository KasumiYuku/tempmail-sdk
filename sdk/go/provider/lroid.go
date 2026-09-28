package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

/*
 * lroid.com — https://lroid.com（Tempail 类临时邮箱）
 * 平台协议（与前端 /js/main.min.js 对齐，2026-09 实测实锤）：
 *   创建邮箱: GET https://lroid.com/ → 首页内联脚本注入 var oturum="<会话令牌>" 与
 *             var tarih="<unix 秒时间戳>"；Set-Cookie 同时下发 PHPSESSID 与 oturum。
 *             自分配邮箱在 <input id="eposta_adres" value="xxx@yevme.com">。
 *   邮件列表: POST https://lroid.com/en/api-kontrol/，表单 oturum=<令牌>&tarih=<首页注入值>；
 *             响应为注入 #epostalar 的 HTML 片段，邮件行是 ul.mailler 下无 class 的 li。
 *             无新邮件时返回 HTTP 304（body 为空），按空列表处理。
 *   邮件正文: POST https://lroid.com/en/api-oku/，表单 oturum=<令牌>&veri[]=<mailID>，
 *             veri 为双元素数组；响应为注入 #eposta_oku 的 HTML 片段。
 * 域名: yevme.com（平台侧该域当前无 MX，属平台故障，SDK 无法修复）。
 */

const lroidBase = "https://lroid.com"

/* lroidKontrolURL lroid 邮件列表接口 */
const lroidKontrolURL = lroidBase + "/en/api-kontrol/"

/* lroidOkuURL lroid 邮件正文接口 */
const lroidOkuURL = lroidBase + "/en/api-oku/"

/* lroidTokPrefix session 令牌前缀，用于区分渠道 token 格式 */
const lroidTokPrefix = "lroid1:"

/* lroidSess lroid 会话信息，序列化后作为 token */
type lroidSess struct {
	/* CookieHdr 合并后的 Cookie 头（含 PHPSESSID 与 oturum） */
	CookieHdr string `json:"c"`
	/* Oturum 首页内联脚本注入的会话令牌 */
	Oturum string `json:"o"`
	/* Tarih 首页内联脚本注入的时间戳（unix 秒） */
	Tarih string `json:"t"`
}

/* lroidMailRow lroid 邮件列表单行 */
type lroidMailRow struct {
	ID      string
	From    string
	Subject string
	Date    string
}

var (
	/* 匹配邮箱地址输入框：<input id="eposta_adres" value="xxx@yevme.com"> */
	lroidEmailRe = regexp.MustCompile(`(?i)<input[^>]+id=["']eposta_adres["'][^>]+value=["']([^"']+)["']`)
	/* 备用：value 在 id 之前的情况 */
	lroidEmailRe2 = regexp.MustCompile(`(?i)<input[^>]+value=["']([^"']+@[^"']+)["'][^>]+id=["']eposta_adres["']`)

	/* 列表 HTML 片段中以 li 为单位（表头 li 无 href 会被过滤） */
	lroidMailLiRe = regexp.MustCompile(`(?is)<li[^>]*>([\s\S]*?)</li>`)
	/* 行内第一个 href，即前端 mail_oku 发送的邮件标识 */
	lroidHrefRe = regexp.MustCompile(`(?i)href\s*=\s*["']\s*([^"'\s][^"']*)["']`)
	/* 行内 div.gonderen = 发件人 */
	lroidFromRe = regexp.MustCompile(`(?is)<div\s+class\s*=\s*["'][^"']*\bgonderen\b[^"']*["'][^>]*>([\s\S]*?)</div>`)
	/* 行内 div.baslik = 主题 */
	lroidSubjectRe = regexp.MustCompile(`(?is)<div\s+class\s*=\s*["'][^"']*\bbaslik\b[^"']*["'][^>]*>([\s\S]*?)</div>`)
	/* 行内 div.zaman = 时间 */
	lroidDateRe = regexp.MustCompile(`(?is)<div\s+class\s*=\s*["'][^"']*\bzaman\b[^"']*["'][^>]*>([\s\S]*?)</div>`)

	/* 剥离 HTML 标签 */
	lroidTagRe = regexp.MustCompile(`<[^>]*>`)
	/* 正文片段中的邮件内容容器（前端的 mail_oku 渲染目标） */
	lroidBodyOpenRe = regexp.MustCompile(`(?is)<div\s+class\s*=\s*["'][^"']*\beposta_acilir\b[^"']*["'][^>]*>`)
	/* 深度计数扫描用：任意 div 开标签与闭标签 */
	lroidDivOpenRe  = regexp.MustCompile(`(?is)<div\b[^>]*>`)
	lroidDivCloseRe = regexp.MustCompile(`(?is)</div\s*>`)
)

/* lroidHTTPClient 获取 lroid 专用 HTTP 客户端（无 cookie jar，cookie 手动管理） */
func lroidHTTPClient() tls_client.HttpClient {
	if HTTPClientNoCookieJar != nil {
		return HTTPClientNoCookieJar()
	}
	return HTTPClient()
}

/* lroidPageHeaders 设置页面/API 请求所需的浏览器模拟请求头 */
func lroidPageHeaders(req *http.Request, referer string, xhr bool) {
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("DNT", "1")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("User-Agent", getCurrentUA())
	if xhr {
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
}

/* lroidMergeCookies 合并已有 cookie 头与响应中的 Set-Cookie，仅取第一个字段（k=v） */
func lroidMergeCookies(prev string, cookies []*http.Cookie) string {
	parts := []string{}
	if prev != "" {
		parts = append(parts, prev)
	}
	for _, c := range cookies {
		if c == nil || c.Name == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return lroidDedupeCookieHeader(strings.Join(parts, "; "))
}

/* lroidDedupeCookieHeader 按 cookie 名去重（Map 无序） */
func lroidDedupeCookieHeader(hdr string) string {
	m := make(map[string]string)
	order := []string{}
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
		if k == "" {
			continue
		}
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = v
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+m[k])
	}
	return strings.Join(out, "; ")
}

/* lroidCookieValue 从 Cookie 头字符串中取出指定 cookie 名的值 */
func lroidCookieValue(hdr, name string) string {
	for _, part := range strings.Split(hdr, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, name+"=") {
			continue
		}
		v := strings.TrimSpace(part[len(name)+1:])
		if v != "" {
			return v
		}
	}
	return ""
}

/* lroidFindJSVar 从页面内联脚本中提取字符串变量：var name="value" */
func lroidFindJSVar(page, name string) string {
	re := regexp.MustCompile(`(?is)var\s+` + regexp.QuoteMeta(name) + `\s*=\s*["']([^"']+)["']`)
	if m := re.FindStringSubmatch(page); len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

/* lroidExtractEmail 从首页 HTML 中提取自分配邮箱地址 */
func lroidExtractEmail(htmlStr string) string {
	if m := lroidEmailRe.FindStringSubmatch(htmlStr); len(m) > 1 {
		return strings.TrimSpace(html.UnescapeString(m[1]))
	}
	if m := lroidEmailRe2.FindStringSubmatch(htmlStr); len(m) > 1 {
		return strings.TrimSpace(html.UnescapeString(m[1]))
	}
	return ""
}

/* lroidStripTags 移除 HTML 标签，返回纯文本（标签位置折叠为空格） */
func lroidStripTags(s string) string {
	return strings.TrimSpace(lroidTagRe.ReplaceAllString(s, " "))
}

/* lroidEncodeSess 将会话信息编码为 token 字符串 */
func lroidEncodeSess(s *lroidSess) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return lroidTokPrefix + base64.StdEncoding.EncodeToString(b), nil
}

/* lroidDecodeSess 从 token 字符串解码会话信息 */
func lroidDecodeSess(tok string) (*lroidSess, error) {
	if !strings.HasPrefix(tok, lroidTokPrefix) {
		return nil, fmt.Errorf("lroid: 无效的会话令牌")
	}
	raw, err := base64.StdEncoding.DecodeString(tok[len(lroidTokPrefix):])
	if err != nil {
		return nil, fmt.Errorf("lroid: 无效的会话令牌")
	}
	var s lroidSess
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("lroid: 无效的会话令牌")
	}
	if s.CookieHdr == "" || s.Oturum == "" {
		return nil, fmt.Errorf("lroid: 会话令牌中缺少必要字段")
	}
	return &s, nil
}

/*
 * LroidGenerate 创建 lroid.com 临时邮箱
 * 流程: GET https://lroid.com/ → 捕获 PHPSESSID/oturum Cookie，提取内联脚本的
 *       oturum/tarih 变量与自分配邮箱地址，全部编码进 token。
 */
func LroidGenerate() (*CreatedMailbox, error) {
	client := lroidHTTPClient()

	req, err := http.NewRequest("GET", lroidBase+"/", nil)
	if err != nil {
		return nil, err
	}
	lroidPageHeaders(req, "", false)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "lroid home"); err != nil {
		return nil, err
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	page := string(raw)

	emailAddr := lroidExtractEmail(page)
	if emailAddr == "" {
		return nil, fmt.Errorf("lroid: 未能从页面中提取邮箱地址")
	}

	cookieHdr := lroidMergeCookies("", resp.Cookies())
	oturum := lroidFindJSVar(page, "oturum")
	if oturum == "" {
		/* 页面脚本未注入会话令牌，尝试从 cookie 中兜底 */
		oturum = lroidCookieValue(cookieHdr, "oturum")
	}
	if oturum == "" {
		return nil, fmt.Errorf("lroid: 未能从页面中提取会话令牌 oturum")
	}

	tarih := lroidFindJSVar(page, "tarih")

	tok, err := lroidEncodeSess(&lroidSess{
		CookieHdr: cookieHdr,
		Oturum:    oturum,
		Tarih:     tarih,
	})
	if err != nil {
		return nil, err
	}

	/* token 中保存完整会话信息，后续获取邮件时需要 */
	return &CreatedMailbox{
		Channel: "lroid",
		Email:   emailAddr,
		Token:   tok,
	}, nil
}

/*
 * LroidGetEmails 获取 lroid.com 邮件列表
 * token: LroidGenerate 产出的会话令牌；email: 创建时分配的邮箱地址。
 * 流程: 先 GET 首页做会话核验（会话页面邮箱与入参邮箱不一致 → 会话被顶掉），
 *       再 POST api-kontrol 拉列表，最后逐封 POST api-oku 拉正文。
 */
func LroidGetEmails(token string, email string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("lroid: 邮箱地址为空")
	}
	if token == "" {
		return nil, fmt.Errorf("lroid: 会话令牌为空")
	}

	sess, err := lroidDecodeSess(token)
	if err != nil {
		return nil, err
	}
	client := lroidHTTPClient()

	/*
	 * 会话核验：GET 首页成功后，页面上的邮箱与入参邮箱不一致说明会话
	 * 已被新的会话顶掉（平台同一浏览器会话仅绑定一个邮箱），提示重建。
	 * 网络失败则跳过核验，继续使用已存参数拉列表。
	 */
	pageEmail, pageTarih, checkErr := lroidCheckSession(client, sess)
	if checkErr == nil && pageEmail != "" && !strings.EqualFold(pageEmail, email) {
		return nil, fmt.Errorf("lroid: 会话邮箱不匹配（页面=%s 入参=%s），会话可能已被顶掉，请重新创建邮箱", pageEmail, email)
	}
	if pageTarih != "" {
		sess.Tarih = pageTarih
	}

	/* POST api-kontrol 获取邮件列表 HTML 片段 */
	fragment, status, err := lroidPostKontrol(client, sess)
	if err != nil {
		return nil, err
	}
	/* HTTP 304 表示无新邮件，按空列表处理 */
	if status == 304 {
		return []NormEmail{}, nil
	}
	if fragment == "" {
		return []NormEmail{}, nil
	}

	rows := lroidParseMailRows(fragment)
	if len(rows) == 0 {
		return []NormEmail{}, nil
	}

	emails := make([]NormEmail, 0, len(rows))
	for _, row := range rows {
		/* 逐封拉取正文，失败不阻断（保留列表字段） */
		htmlBody, textBody := lroidFetchMailBody(client, sess, row.ID)

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
		emails = append(emails, NormalizeMap(flat, email))
	}
	return emails, nil
}

/* lroidCheckSession 访问首页核验会话，返回当前会话绑定的邮箱与最新 tarih */
func lroidCheckSession(client tls_client.HttpClient, sess *lroidSess) (string, string, error) {
	req, err := http.NewRequest("GET", lroidBase+"/", nil)
	if err != nil {
		return "", "", err
	}
	lroidPageHeaders(req, "", false)
	req.Header.Set("Cookie", sess.CookieHdr)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("lroid: 会话核验请求失败 %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", err
	}
	page := string(raw)

	pageEmail := lroidExtractEmail(page)
	pageTarih := lroidFindJSVar(page, "tarih")
	/* 若平台轮换会话语义，刷新 cookie 头 */
	if merged := lroidMergeCookies(sess.CookieHdr, resp.Cookies()); merged != "" {
		sess.CookieHdr = merged
	}
	return pageEmail, pageTarih, nil
}

/* lroidPostKontrol POST api-kontrol 拉取邮件列表 HTML 片段 */
func lroidPostKontrol(client tls_client.HttpClient, sess *lroidSess) (string, int, error) {
	form := make(url.Values)
	form["oturum"] = []string{sess.Oturum}
	form["tarih"] = []string{sess.Tarih}
	return lroidPostForm(client, lroidKontrolURL, sess, form)
}

/* lroidPostForm 发送 API 表单 POST 并返回响应体与状态码 */
func lroidPostForm(client tls_client.HttpClient, apiURL string, sess *lroidSess, form url.Values) (string, int, error) {
	body := form.Encode()
	req, err := http.NewRequest("POST", apiURL, strings.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	lroidPageHeaders(req, lroidBase+"/", true)
	req.Header.Set("Cookie", sess.CookieHdr)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Origin", lroidBase)

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

/* lroidParseMailRows 从 api-kontrol 的 HTML 片段中提取邮件行 */
func lroidParseMailRows(fragment string) []lroidMailRow {
	rows := make([]lroidMailRow, 0, 4)
	items := lroidMailLiRe.FindAllStringSubmatch(fragment, -1)
	for _, item := range items {
		if len(item) < 2 {
			continue
		}
		content := item[1]

		/* 行内第一个 href 即为该邮件的 mailID */
		row := lroidMailRow{}
		if m := lroidHrefRe.FindStringSubmatch(content); len(m) >= 2 {
			row.ID = strings.TrimSpace(html.UnescapeString(m[1]))
		}
		if row.ID == "" {
			continue
		}

		if m := lroidFromRe.FindStringSubmatch(content); len(m) >= 2 {
			row.From = lroidStripTags(m[1])
		}
		if m := lroidSubjectRe.FindStringSubmatch(content); len(m) >= 2 {
			row.Subject = lroidStripTags(m[1])
		}
		if m := lroidDateRe.FindStringSubmatch(content); len(m) >= 2 {
			row.Date = lroidStripTags(m[1])
		}
		rows = append(rows, row)
	}
	return rows
}

/* lroidFetchMailBody 通过 api-oku 拉取单封邮件正文，返回 HTML 与纯文本 */
func lroidFetchMailBody(client tls_client.HttpClient, sess *lroidSess, mailID string) (string, string) {
	if mailID == "" {
		return "", ""
	}

	form := make(url.Values)
	form["oturum"] = []string{sess.Oturum}
	/* 前端 mail_oku 以双元素数组传参：veri[] = [mailID, mailID] */
	form["veri[]"] = []string{mailID, mailID}

	fragment, status, err := lroidPostForm(client, lroidOkuURL, sess, form)
	if err != nil {
		return "", ""
	}
	if status != 200 || fragment == "" {
		return "", ""
	}

	htmlBody := lroidExtractBodyFromPage(fragment)
	return htmlBody, lroidStripTags(htmlBody)
}

/* lroidExtractBodyFromPage 从 api-oku 响应片段中提取邮件正文 HTML */
func lroidExtractBodyFromPage(fragment string) string {
	/* 优先采用 .eposta_acilir 容器（前端 mail_oku 的渲染结构），按 div 深度剥离其子内容 */
	if idx := lroidBodyOpenRe.FindStringIndex(fragment); idx != nil {
		pos, depth := idx[1], 1
		for depth > 0 {
			openIdx := lroidDivOpenRe.FindStringIndex(fragment[pos:])
			closeIdx := lroidDivCloseRe.FindStringIndex(fragment[pos:])
			if closeIdx == nil {
				break
			}
			if openIdx != nil && openIdx[0] < closeIdx[0] {
				depth++
				pos += openIdx[1]
				continue
			}
			depth--
			if depth == 0 {
				inner := strings.TrimSpace(fragment[idx[1] : pos+closeIdx[0]])
				if inner != "" {
					return inner
				}
				break
			}
			pos += closeIdx[1]
		}
	}

	/* 回退：常见的邮件正文容器 class */
	fallbacks := []string{
		"eposta_metin", "eposta_menzil", "mail_icerik", "icerik",
		"mail-content", "message-body", "email-body",
	}
	for _, cls := range fallbacks {
		re := regexp.MustCompile(`(?is)<div[^>]+class\s*=\s*["'][^"']*` + regexp.QuoteMeta(cls) + `[^"']*["'][^>]*>([\s\S]*?)</div>`)
		if m := re.FindStringSubmatch(fragment); len(m) >= 2 {
			if inner := strings.TrimSpace(m[1]); inner != "" {
				return inner
			}
		}
	}

	/* 剥离整页骨架，返回原始片段 */
	return strings.TrimSpace(fragment)
}
