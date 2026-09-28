package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net/url"
	"regexp"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

/*
 * NoxenDe5Net 渠道实现（UniMail-Bot 公共实例 tempmail.noxen.de5.net）
 * 完整接入契约（curl 实测 + 上游源码 review，2026-09-27 至 09-28）：
 *   - 登录：POST /api/login {"username":"guest","password":"123456"}
 *     → 200 {"success":true,"role":"guest"} 并 Set-Cookie: iding-session=<JWT>
 *     （HttpOnly; Secure; SameSite=Strict，Max-Age 86400）；会话校验 /api/session 返回
 *     {"authenticated":true,"role":"guest","username":"guest","strictAdmin":false}。
 *   - 建箱：GET /api/generate → 200 {"email":"随机@域名","expires":毫秒时间戳}，
 *     域名池来自 GET /api/domains（无鉴权也可读）。
 *   - 读信：GET /api/emails?mailbox=<地址>&limit=20 → 200 邮件数组（无邮件为 []），
 *     字段 id/sender/subject/received_at/is_read/preview/verification_code；
 *     详情 GET /api/email/{id} → {..., content, html_content, to_addrs,
 *     r2_bucket, r2_object_key, download}。
 *   - 正文存储（2026-09-28 实测）：详情响应的 content/html_content 恒为空，
 *     平台将原始 EML 存 Cloudflare R2（r2_bucket/r2_object_key），
 *     download 字段指向 GET /api/email/{id}/download，返回
 *     200 message/rfc822 + Content-Disposition: attachment 的原始报文
 *     （上游 apiHandlers.js handleApiRequest 下载处理器 + 实测 3880 号邮件）。
 *   - 鉴权边界：读信不带会话 Cookie 返回 401；访客邮箱只能查自己的 mailbox；
 *     邮箱用户仅能查看近 24 小时邮件（访客子会话无该限制）。
 *
 * 会话隔离：先 GET /api/session 兜底校验 cookie，未通过则重新 login；
 *   凭据串只由会话 Cookie 与同源地址构成，读信时逐请求显式携带。
 */

const (
	noxenDe5NetBase = "https://tempmail.noxen.de5.net"
	noxenDe5NetUser = "guest"
	noxenDe5NetPass = "123456"
	// noxenDe5NetTokenPrefix 本渠道凭据串前缀（"基址|cookie=值"）
	noxenDe5NetTokenPrefix = "noxen-de5-net|"
)

/* noxenDe5NetLoginResponse 登录响应 */
type noxenDe5NetLoginResponse struct {
	Success bool   `json:"success"`
	Role    string `json:"role"`
	Error   string `json:"error"`
}

/* noxenDe5NetGenerateResponse 建箱响应 */
type noxenDe5NetGenerateResponse struct {
	Email   string `json:"email"`
	Expires int64  `json:"expires"`
}

/*
 * noxenDe5NetSessionCookie 登录取得会话 Cookie（iding-session=JWT）
 * @param domain 可选首选域名（来自 opts.Domain）
 * @param client 无 Cookie 罐的客户端（会话由显式 Cookie 头接管，避免全局 Cookie 罐串池）
 */
func noxenDe5NetSessionCookie(client tls_client.HttpClient) (string, error) {
	if client == nil {
		client = HTTPClient()
	}
	body, _ := json.Marshal(map[string]string{"username": noxenDe5NetUser, "password": noxenDe5NetPass})
	req, err := fhttp.NewRequest("POST", noxenDe5NetBase+"/api/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("noxen-de5-net login: http %d", resp.StatusCode)
	}
	var data noxenDe5NetLoginResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", err
	}
	if !data.Success {
		return "", fmt.Errorf("noxen-de5-net login: 登录失败")
	}
	var session string
	for _, sc := range resp.Header.Values("Set-Cookie") {
		kv := sc
		if i := strings.IndexByte(sc, ';'); i > 0 {
			kv = sc[:i]
		}
		if strings.HasPrefix(kv, "iding-session=") {
			session = kv
			break
		}
	}
	if session == "" {
		return "", fmt.Errorf("noxen-de5-net login: 未下发会话 Cookie")
	}
	return session, nil
}

/*
 * noxenDe5NetCookieStillValid 校验会话 Cookie 是否仍有效（GET /api/session）
 */
func noxenDe5NetCookieStillValid(client tls_client.HttpClient, cookie string) bool {
	req, err := fhttp.NewRequest("GET", noxenDe5NetBase+"/api/session", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Cookie", cookie)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var data struct {
		Authenticated bool `json:"authenticated"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return false
	}
	return resp.StatusCode == 200 && data.Authenticated
}

/*
 * NoxenDe5NetGenerate 登录并创建临时邮箱
 * @param domain 可选首选域名（nil 时平台自动选域）
 * token 凭据串格式："noxen-de5-net|iding-session=<JWT>|base=<基址>"，读信据此拼 Cookie
 */
func NoxenDe5NetGenerate(domain *string) (*CreatedMailbox, error) {
	client := HTTPClientNoCookieJar()

	wantDomain := ""
	if domain != nil {
		wantDomain = strings.TrimSpace(*domain)
		if wantDomain != "" && !noxenDe5NetDomainInPool(client, wantDomain) {
			return nil, fmt.Errorf("noxen-de5-net generate: 域名 %s 不在平台域名池", wantDomain)
		}
	}

	session, err := noxenDe5NetSessionCookie(client)
	if err != nil {
		return nil, err
	}

	// 建箱：GET /api/generate（访客会话）
	req, err := fhttp.NewRequest("GET", noxenDe5NetBase+"/api/generate", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Cookie", session)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("noxen-de5-net generate: http %d", resp.StatusCode)
	}
	var data noxenDe5NetGenerateResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.Email == "" {
		return nil, fmt.Errorf("noxen-de5-net generate: 响应缺少 email")
	}

	token := noxenDe5NetTokenPrefix + url.QueryEscape(session) + "|base=" + noxenDe5NetBase
	expiresAt := ""
	if data.Expires > 0 {
		expiresAt = time.UnixMilli(data.Expires).UTC().Format(time.RFC3339)
	}
	return &CreatedMailbox{
		Channel:   "noxen-de5-net",
		Email:     data.Email,
		Token:     token,
		ExpiresAt: expiresAt,
	}, nil
}

/* noxenDe5NetDomainInPool 校验域名是否在平台域名池内 */
func noxenDe5NetDomainInPool(client tls_client.HttpClient, domain string) bool {
	req, err := fhttp.NewRequest("GET", noxenDe5NetBase+"/api/domains", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var pool []string
	if json.Unmarshal(raw, &pool) != nil {
		return false
	}
	for _, d := range pool {
		if strings.EqualFold(d, domain) {
			return true
		}
	}
	return false
}

/*
 * NoxenDe5NetGetEmails 读取收件箱
 * @param email 信箱地址（与凭据同源）
 * @param token 建箱下发的凭据串
 * 列表只含预览（preview 为正文前约 120 字符）。正文获取优先级：
 * 1) 详情（r2_bucket/r2_object_key/download 定位）+ 下载端点拉取原始 EML，
 *    本地拆分 text/plain 与 text/html（上游 parseEmailBody 同构逻辑）；
 * 2) 详情/EML 链路不可得时，用 verification_code 置顶 + preview 合成占位正文，
 *    保证验证码与 preview 覆盖范围内的哨兵可提取（无可达全文时如实的上限）。
 */
func NoxenDe5NetGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if !strings.HasPrefix(token, noxenDe5NetTokenPrefix) {
		return nil, fmt.Errorf("noxen-de5-net: token 格式错误")
	}
	cookie, err := url.QueryUnescape(strings.TrimPrefix(token, noxenDe5NetTokenPrefix))
	if err != nil {
		return nil, fmt.Errorf("noxen-de5-net: token 解码失败")
	}
	cookie = strings.TrimSuffix(cookie, "|base="+noxenDe5NetBase)
	client := HTTPClientNoCookieJar()
	if !noxenDe5NetCookieStillValid(client, cookie) {
		session, err := noxenDe5NetSessionCookie(client)
		if err != nil {
			return nil, err
		}
		cookie = session
	}

	// 列表：GET /api/emails?mailbox=<地址>&limit=20
	listURL := noxenDe5NetBase + "/api/emails?mailbox=" + url.QueryEscape(email) + "&limit=20"
	req, err := fhttp.NewRequest("GET", listURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Cookie", cookie)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 401 {
			return nil, fmt.Errorf("noxen-de5-net 读信: http 401（会话失效或非本会话邮箱）")
		}
		return nil, fmt.Errorf("noxen-de5-net 读信: http %d", resp.StatusCode)
	}

	var list []map[string]interface{}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}

	out := make([]NormEmail, 0, len(list))
	for _, m := range list {
		id := fmt.Sprintf("%v", m["id"])
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		flat["from"] = m["sender"]
		flat["to"] = email
		flat["date"] = m["received_at"]
		flat["text"] = m["preview"]
		flat["isRead"] = m["is_read"]
		full := false
		if id != "" && id != "0" {
			// 详情：取 r2/download 定位 + 兜底 content/html_content（平台恒空）
			if detail, dErr := noxenDe5NetFetchDetail(client, cookie, id); dErr == nil {
				flat["content"] = detail.content
				flat["html_content"] = detail.htmlContent
				flat["to_addrs"] = detail.toAddrs
				flat["r2_bucket"] = detail.r2Bucket
				flat["r2_object_key"] = detail.r2ObjectKey
				// 全文：download 端点原始 EML → 本地拆分 text/html
				if detail.download != "" {
					eml, eErr := noxenDe5NetFetchEML(client, cookie, detail.download)
					if eErr == nil && len(eml) > 0 {
						if text, htmlBody := noxenDe5NetParseEML(eml); text != "" || htmlBody != "" {
							flat["text"] = text
							flat["html_content"] = htmlBody
							full = true
						}
					}
				}
			}
			if !full {
				flat["text"] = noxenDe5NetComposePlaceholder(m)
			}
		}
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}

/* noxenDe5NetDetail 邮件详情（字段与上游 /api/email/{id} 对齐） */
type noxenDe5NetDetail struct {
	content     string
	htmlContent string
	toAddrs     string
	download    string
	r2Bucket    string
	r2ObjectKey string
}

/* noxenDe5NetFetchDetail 拉取单封邮件详情 */
func noxenDe5NetFetchDetail(client tls_client.HttpClient, cookie, id string) (*noxenDe5NetDetail, error) {
	req, err := fhttp.NewRequest("GET", noxenDe5NetBase+"/api/email/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Cookie", cookie)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("noxen-de5-net detail: http %d", resp.StatusCode)
	}
	var data struct {
		Content     string `json:"content"`
		HTMLContent string `json:"html_content"`
		ToAddrs     string `json:"to_addrs"`
		Download    string `json:"download"`
		R2Bucket    string `json:"r2_bucket"`
		R2ObjectKey string `json:"r2_object_key"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return &noxenDe5NetDetail{
		content:     data.Content,
		htmlContent: data.HTMLContent,
		toAddrs:     data.ToAddrs,
		download:    data.Download,
		r2Bucket:    data.R2Bucket,
		r2ObjectKey: data.R2ObjectKey,
	}, nil
}

/*
 * noxenDe5NetFetchEML 拉取原始 EML 报文（详情 download 字段指向的下载端点）
 * @param client 请求客户端（与详情同会话）
 * @param cookie 会话凭据
 * @param dlPath 详情下发的下载相对路径（如 /api/email/3880/download）
 * 实测：GET /api/email/{id}/download → 200 message/rfc822，
 *   Content-Disposition: attachment（原始存储的 .eml，含 multipart 边界）。
 */
func noxenDe5NetFetchEML(client tls_client.HttpClient, cookie, dlPath string) ([]byte, error) {
	u := dlPath
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = noxenDe5NetBase + u
	}
	req, err := fhttp.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "message/rfc822, */*")
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Cookie", cookie)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("noxen-de5-net download: http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

/*
 * noxenDe5NetComposePlaceholder 全文不可得时的合成占位正文：
 * verification_code 置顶（"验证码: xxx"），preview 附后为正文近况；
 * 二者均空时返回空串。哨兵提取以 preview 实际覆盖范围为如实上限。
 * @param m 列表元素 map（verification_code/preview）
 */
func noxenDe5NetComposePlaceholder(m map[string]interface{}) string {
	code := ""
	if v, ok := m["verification_code"]; ok && v != nil {
		code = strings.TrimSpace(fmt.Sprintf("%v", v))
	}
	preview := ""
	if v, ok := m["preview"]; ok && v != nil {
		preview = strings.TrimSpace(fmt.Sprintf("%v", v))
	}
	parts := make([]string, 0, 2)
	if code != "" {
		parts = append(parts, "验证码: "+code)
	}
	if preview != "" {
		parts = append(parts, preview)
	}
	return strings.Join(parts, "\n\n")
}

/* noxenDe5NetBoundaryRe 提取 Content-Type 中的 multipart boundary（引号可选） */
var noxenDe5NetBoundaryRe = regexp.MustCompile(`(?i)boundary="?([^";\s]+)"?`)

/* noxenDe5NetBoundary 从原始 Content-Type 头值提取 boundary（区分大小写，与上游一致） */
func noxenDe5NetBoundary(ctRaw string) string {
	m := noxenDe5NetBoundaryRe.FindStringSubmatch(ctRaw)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

/*
 * noxenDe5NetSplitEML 将原始报文切分为首部 map 与正文块
 * @param payload 已做 CRLF→LF 归一化的原始报文
 * @param offset 起始行号（外包 "#participant"+part 或 mbox "From " 首行时置 1）
 */
func noxenDe5NetSplitEML(payload string, offset int) (map[string]string, string) {
	lines := strings.Split(payload, "\n")
	headers := make(map[string]string)
	i := offset
	if i < len(lines) && strings.HasPrefix(lines[i], "From ") {
		i++
	}
	var curKey string
	for ; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			i++
			break
		}
		// 折行续行（RFC 5322）：以空白开头且已有当前头键时拼接到上一条
		if (line[0] == ' ' || line[0] == '\t') && curKey != "" {
			headers[curKey] += " " + strings.TrimSpace(line)
			continue
		}
		if k := strings.IndexByte(line, ':'); k > 0 {
			curKey = strings.ToLower(strings.TrimSpace(line[:k]))
			headers[curKey] = strings.TrimSpace(line[k+1:])
		}
	}
	if i > len(lines) {
		i = len(lines)
	}
	return headers, strings.Join(lines[i:], "\n")
}

/*
 * noxenDe5NetSplitMultipart 按 boundary 切出各 part（含各自首部行）
 * 外部以 "#participant"+part 形式喂入 splitEML 可正确拆分 part 头/体。
 */
func noxenDe5NetSplitMultipart(body, boundary string) []string {
	segments := strings.Split(body, "--"+boundary)
	parts := make([]string, 0, len(segments))
	for _, seg := range segments {
		seg = strings.TrimPrefix(seg, "\n")
		seg = strings.TrimSuffix(seg, "--\n")
		seg = strings.TrimSuffix(seg, "--")
		if strings.TrimSpace(seg) != "" {
			parts = append(parts, seg)
		}
	}
	return parts
}

/*
 * noxenDe5NetParseEML 解析 EML 原始报文 → (纯文本正文, HTML 正文)
 * 与上游 noxenys/temp-mail emailParser.parseEmailBody 同构：
 * 顶层头 + multipart 递归（嵌套 multipart / message/rfc822 / 跳过 rfc822-headers），
 * 无 HTML part 时兜底在整体原文中抓取 <html>…</html> 片段。
 */
func noxenDe5NetParseEML(raw []byte) (string, string) {
	payload := strings.ReplaceAll(string(raw), "\r\n", "\n")
	payload = strings.ReplaceAll(payload, "\r", "")
	topHeaders, topBody := noxenDe5NetSplitEML(payload, 0)
	return noxenDe5NetParseEntity(topHeaders, topBody)
}

/*
 * noxenDe5NetParseEntity 递归解析单个 MIME 实体（上游 parseEntity 同构）
 * @param headers 实体首部
 * @param body    实体正文块（原始字节流，含嵌套边界）
 */
func noxenDe5NetParseEntity(headers map[string]string, body string) (string, string) {
	ctRaw := headers["content-type"]
	ct := strings.ToLower(ctRaw)
	cte := strings.ToLower(headers["content-transfer-encoding"])

	// 单体：text/html 或 text/plain（含无 Content-Type 时按纯文本处理）
	if !strings.HasPrefix(ct, "multipart/") {
		decoded := noxenDe5NetDecodePart(body, cte)
		if strings.Contains(ct, "text/html") {
			return "", decoded
		}
		return decoded, ""
	}

	// 复合：递归拆分，text 槽与 html 槽各自取第一个非空命中
	text, htmlStr := "", ""
	boundary := noxenDe5NetBoundary(ctRaw)
	if boundary != "" {
		for _, part := range noxenDe5NetSplitMultipart(body, boundary) {
			ph, pb := noxenDe5NetSplitEML("#participant\n"+part, 1)
			pct := strings.ToLower(ph["content-type"])
			switch {
			case strings.HasPrefix(pct, "multipart/"):
				t, h := noxenDe5NetParseEntity(ph, pb)
				if text == "" {
					text = t
				}
				if htmlStr == "" {
					htmlStr = h
				}
			case strings.HasPrefix(pct, "message/rfc822"):
				// 转发的原始邮件整体作为 part：递归整封解析
				nh, nb := noxenDe5NetSplitEML(pb, 0)
				t, h := noxenDe5NetParseEntity(nh, nb)
				if text == "" {
					text = t
				}
				if htmlStr == "" {
					htmlStr = h
				}
			case strings.Contains(pct, "rfc822-headers"):
				// 纯头部 part 跳过，正文在后续 part 中抓取
				continue
			default:
				t, h := noxenDe5NetParseEntity(ph, pb)
				if text == "" {
					text = t
				}
				if htmlStr == "" {
					htmlStr = h
				}
			}
			if text != "" && htmlStr != "" {
				break
			}
		}
	}
	// 无 HTML 命中时从整体原文兜底抓取 HTML 片段（上游 guessHtmlFromRaw 同构）
	if htmlStr == "" {
		htmlStr = noxenDe5NetGuessHTML(body)
	}
	return text, htmlStr
}

/*
 * noxenDe5NetDecodePart 按 Content-Transfer-Encoding 解码 part 内容。
 * 非 UTF-8 字符集（gbk 等）不做转码：字节原样输出，
 * ASCII 哨兵/验证码不受影响，中文可能乱码——与上游 TextDecoder(fatal:false)
 * 行为同理，属本渠道可达上限（SDK 不引第三方编码表）。
 */
func noxenDe5NetDecodePart(data, cte string) string {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		joined := strings.Map(func(r rune) rune {
			switch r {
			case '\n', '\r', '\t', ' ':
				return -1
			}
			return r
		}, data)
		if b, err := base64.StdEncoding.DecodeString(joined); err == nil {
			return strings.TrimSpace(string(b))
		}
		return data
	case "quoted-printable":
		if b, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(data))); err == nil {
			return strings.TrimSpace(string(b))
		}
		return data
	default:
		// 7bit/8bit/binary：原样返回
		return strings.TrimSpace(data)
	}
}

/*
 * noxenDe5NetGuessHTML 从整体原文中抓取 <html>…</html> 片段（上游 guessHtmlFromRaw 同构）
 * 覆盖邮件未正确声明 Content-Type、HTML 正文裸置的情况。
 */
func noxenDe5NetGuessHTML(body string) string {
	if body == "" {
		return ""
	}
	lower := strings.ToLower(body)
	hs := strings.Index(lower, "<html")
	if hs == -1 {
		hs = strings.Index(lower, "<!doctype html")
	}
	if hs == -1 {
		return ""
	}
	he := strings.LastIndex(lower, "</html>")
	if he == -1 || he < hs {
		return ""
	}
	return body[hs : he+7]
}
