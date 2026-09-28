package provider

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * GeneratorEmail 渠道实现（generator.email，PHP SSR 网页型 + WebSocket
 * 推送；gasmurl 引擎同源形态）
 *
 * 调研实证结论（2026-09-28 连续三轮 curl + 真实收信取证）：
 *   - 无独立建箱 API：首页 SSR 渲染随机邮箱并内联 window.SITE_DATA
 *     （cur_user/cur_domain），同响应 Set-Cookie inbox_ctx=<domain>/<user>/
 *     绑定该会话；再次访问首页若不换 Cookie 罐会另发新邮箱快照，
 *     但带罐访问 /<domain>/<user>/ 或直接带罐请求 /inbox4/ 时服务端
 *     仍按 inbox_ctx 渲染绑定邮箱（实测三轮 GET /inbox4/ 持续渲染
 *     生成时的原邮箱）。
 *   - 读信同为 SSR：GET /inbox4/ 带 inbox_ctx 返回该邮箱渲染页。
 *     列表具体结构（实测含信页面原文，2026-09-28 17:2x）：
 *       #mail-summary-head：div.g8r.list-group-item2.list-group-item-info，
 *         三要素 from_div_45g45gg / subj_div_45g45gg / time_div_45g45gg
 *       #mail-summary-body：div.g8r.list-group-item2.list-item2-flat，
 *         正文原文在 div.mess_bodiyy（注意平台把 body 拼作 bodiyy），
 *         详情头部另有 mailsrc[data-mid] 与 To/From/Subject/Received 行
 *   - 判定语义：列表行正则必须匹配 list-group-item2；正文侧仅 head
 *     与 body 两个 div 的行会被三个 from/subj/time 正则扫到空串，须过滤，
 *     否则产生空行条目。
 */

/* generatorEmailSite 站点基址与收信页路径 */
const (
	generatorEmailSite  = "https://generator.email"
	generatorEmailInbox = generatorEmailSite + "/inbox4/"
)

/* generatorEmailSession token 中保存的邮箱上下文快照 */
type generatorEmailSession struct {
	Email     string `json:"email"`
	Domain    string `json:"domain"`
	User      string `json:"user"`
	InboxCtx  string `json:"inboxCtx,omitempty"`
	InboxName string `json:"inboxName,omitempty"`
}

/* generatorEmailListRow 列表条目三要素 */
type generatorEmailListRow struct {
	From    string
	Subject string
	Time    string
}

var (
	/* SITE_DATA 快照提取 */
	generatorEmailUserRE   = regexp.MustCompile(`cur_user:"([^"]*)"`)
	generatorEmailDomainRE = regexp.MustCompile(`cur_domain:"([^"]*)"`)
	/*
	 * 列表行级结构（实测 DOM）：每封信相邻两行——
	 * head 行（list-group-item2 list-group-item-info，from/subj/time 三要素）
	 * + body 行（list-group-item2 list-item2-flat，mess_bodiyy 正文原文），
	 * 全局按 (head,body) 相邻配对解析。
	 */
	generatorEmailRowRE  = regexp.MustCompile(`(?s)<div[^>]*class="[^"]*list-group-item2[^"]*"[^>]*>(.*?)(?:</div>\s*){3}`)
	generatorEmailFromRE = regexp.MustCompile(`(?s)class="[^"]*from_div_45g45gg[^"]*"[^>]*>(.*?)</div>`)
	generatorEmailSubjRE = regexp.MustCompile(`(?s)class="[^"]*subj_div_45g45gg[^"]*"[^>]*>(.*?)</div>`)
	generatorEmailTimeRE = regexp.MustCompile(`(?s)class="[^"]*time_div_45g45gg[^"]*"[^>]*>(.*?)</div>`)
	generatorEmailBodyRE = regexp.MustCompile(`(?s)<div[^>]*class="[^"]*mess_bodiyy[^"]*"[^>]*>(.*?)</div>`)
	/*
	 * B 形态行：懒加载行开标签锚点（onclick 携带 <domain>/<user>/<msgId>），
	 * 行内容改为区间切片解耦（GetEmails 内按锚点切分再取三要素），
	 * 避免连续 </div> 计数的正则回溯行为在 Go RE2 下与 Python 不一致。
	 */
	generatorEmailLazyRowOpenRE = regexp.MustCompile(`<div[^>]*onclick="loadInboxClientSide\('([^']*)'\)"[^>]*>`)
	generatorEmailMsgIDRE       = regexp.MustCompile(`loadInboxClientSide\('[^']*/([a-f0-9]{32})'\)`)
	/*
	 * script/style 块剔除：Go RE2 不支持 \1 反向引用，
	 * 开标签捕获后按命中标签二选一闭合正则显式匹配（语义等价展开）。
	 */
	generatorEmailTagOpenRE  = regexp.MustCompile(`(?is)<(script|style)[\s>]`)
	generatorEmailScriptClRE = regexp.MustCompile(`(?is)</script\s*>`)
	generatorEmailStyleClRE  = regexp.MustCompile(`(?is)</style\s*>`)
)

/* generatorEmailStripScriptStyle 剔除 script/style 块（无闭合标签时保留原文） */
func generatorEmailStripScriptStyle(s string) string {
	var b strings.Builder
	pos := 0
	for pos < len(s) {
		m := generatorEmailTagOpenRE.FindStringSubmatchIndex(s[pos:])
		if m == nil {
			b.WriteString(s[pos:])
			break
		}
		openEnd := pos + m[1]
		b.WriteString(s[pos:openEnd])
		b.WriteString(" ")
		closeRE := generatorEmailScriptClRE
		if s[pos+m[2]:pos+m[3]] == "style" {
			closeRE = generatorEmailStyleClRE
		}
		cm := closeRE.FindStringIndex(s[openEnd:])
		if cm == nil {
			b.WriteString(s[openEnd:])
			pos = len(s)
			break
		}
		b.WriteString(s[openEnd : openEnd+cm[1]])
		pos = openEnd + cm[1]
	}
	return b.String()
}

/* generatorEmailStripTags 去标签与脚本/样式块，反转义 */
func generatorEmailStripTags(s string) string {
	s = generatorEmailStripScriptStyle(s)
	s = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

/* generatorEmailUA 请求头 UA：gsess 实验（2026-09-28）已证实此字面 UA
 * 与 fhttp Chrome 指纹组合可稳定维持会话（GetCurrentUA 的随机化 UA 会
 * 被服务端按 UA 分流导致会话丢失） */
const generatorEmailUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

/*
 * generatorEmailFetch 请求收件箱渲染页并返回 HTML
 * 附带浏览器级头部与会话 Cookie；服务端依据 inbox_ctx 渲染绑定邮箱。
 * @param sess - 会话快照（Generate 时传 nil 走首页，GetEmails 时携带会话）
 */
func generatorEmailFetch(sess *generatorEmailSession) ([]byte, error) {
	url := generatorEmailSite + "/"
	if sess != nil {
		/* 读信走 inbox4；Cookie 由共享客户端罐自动携带（同 gsess 验证链路） */
		url = generatorEmailInbox
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", generatorEmailUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", generatorEmailSite+"/")
	/* 不手动设置 Cookie 头：fhttp 对显式 Cookie 头存在转义差异，
	 * 依赖共享客户端 CookieJar 自动附加（gsess 实验 r0 即命中实证） */

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
		return nil, fmt.Errorf("generator-email: 请求失败 http %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

/* generatorEmailInboxCtxValue 把 domain/user 编码成平台 Cookie 观察值形态 */
func generatorEmailInboxCtxValue(domain, user string) string {
	return fmt.Sprintf("%s%%2F%s%%2F", domain, user)
}

/* generatorEmailMatchFirst 取第一个捕获组，空则回退字符串 */
func generatorEmailMatchFirst(re *regexp.Regexp, src string) string {
	m := re.FindStringSubmatch(src)
	if len(m) > 1 {
		return m[1]
	}
	return ""
}

/*
 * GeneratorEmailGenerate 创建 generator.email 临时邮箱
 * 解析首页 SITE_DATA 快照（cur_user/cur_domain）得到邮箱地址，
 * 并回写 inbox_ctx 格式凭据供读信携带。
 * 关键修正（2026-09-28 实锤）：MTA 侧会通过邮箱别名重新生成域名
 * （quangvps.com 信到 tempm.com），首页快照域可能收不到信；
 * 建箱后带回写路由进行 Cookie 罐级会话绑定，读信使用与建箱
 * 完全相同的 Cookie 罐（同一客户端），确保渲染邮箱与快照一致。
 * 同时邮箱快照可能被平台漂移，故 token 保存三类：快照域 user、
 * 以及页面内嵌的 inbox_ctx 值。
 */
func GeneratorEmailGenerate() (*CreatedMailbox, error) {
	/*
	 * 域池注记（2026-09-28 终验）：平台首页快照域 MX 混杂
	 * generator.email / email-fake.com / emailfake.com / tempm.com 等；
	 * 实测 email-fake.com 也可正常收信（gamerai.space 三哨兵命中），
	 * 按 MX 白名单过滤会把可用域误拒并制造假 gen-failed，
	 * 故建箱不做域筛选，收信成败交由平台会话自然轮换。
	 */
	body, err := generatorEmailFetch(nil)
	if err != nil {
		return nil, err
	}
	src := string(body)
	user := generatorEmailMatchFirst(generatorEmailUserRE, src)
	domain := generatorEmailMatchFirst(generatorEmailDomainRE, src)
	if user == "" || domain == "" {
		return nil, fmt.Errorf("generator-email: 首页未携带邮箱快照（cur_user/cur_domain）")
	}
	email := user + "@" + domain

	sess, err := json.Marshal(generatorEmailSession{
		Email:    email,
		Domain:   domain,
		User:     user,
		InboxCtx: generatorEmailInboxCtxValue(domain, user),
	})
	if err != nil {
		return nil, err
	}
	return &CreatedMailbox{
		Channel: "generator-email",
		Email:   email,
		Token:   string(sess),
	}, nil
}

/*
 * GeneratorEmailGetEmails 获取 generator.email 收件箱
 * 解析收件箱渲染页：mail-summary-head（from/subject/time）与
 * mail-summary-body（mess_bodiyy 正文原文）。
 * @param email - 邮箱地址
 * @param token - 会话凭据 JSON（含 inbox_ctx Cookie 值）
 */
func GeneratorEmailGetEmails(email, token string) ([]NormEmail, error) {
	var sess generatorEmailSession
	if err := json.Unmarshal([]byte(token), &sess); err != nil {
		return nil, fmt.Errorf("generator-email: 会话凭据解析失败: %w", err)
	}
	if sess.Email != email {
		return nil, fmt.Errorf("generator-email: 会话邮箱与查询邮箱不匹配")
	}
	/* 兼容旧 token（无 InboxCtx 时按 Snapshot 一致性校验接管） */
	if sess.InboxCtx == "" {
		sess.InboxCtx = generatorEmailInboxCtxValue(sess.Domain, sess.User)
	}

	body, err := generatorEmailFetch(&sess)
	if err != nil {
		return nil, err
	}
	src := string(body)

	/* 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换 */
	if got := generatorEmailMatchFirst(generatorEmailDomainRE, src); got != sess.Domain {
		return nil, fmt.Errorf("generator-email: 会话域名已切换（token %s，服务端 %s）", sess.Domain, got)
	}

	/*
	 * 列表页信件行为两种形态（2026-09-28 桌页取证）：
	 *   - A：行 class list-group-item2（head+body 相邻双行，同页内联正文）
	 *   - B：行 class list-group-item[-success]（onclick loadInboxClientSide
	 *     指向 <domain>/<user>/<msgId>；正文在详情页 SSR）
	 * SDK 双形态适配：A 形态直接内联提取；B 形态解析 onclick 里的
	 * msgId 并拉详情页提取 mess_bodiyy 原文。
	 */
	out := make([]NormEmail, 0, 4)

	/* A 形态：list-group-item2 相邻行配对 */
	rows := generatorEmailRowRE.FindAllStringSubmatch(src, -1)
	for i := 0; i+1 < len(rows); i += 2 {
		headRaw := rows[i][1]
		bodyRaw := rows[i+1][1]
		from := generatorEmailStripTags(generatorEmailMatchFirst(generatorEmailFromRE, headRaw))
		subject := generatorEmailStripTags(generatorEmailMatchFirst(generatorEmailSubjRE, headRaw))
		when := generatorEmailStripTags(generatorEmailMatchFirst(generatorEmailTimeRE, headRaw))
		if from == "" || subject == "" {
			continue
		}
		text := html.UnescapeString(generatorEmailStripTags(generatorEmailMatchFirst(generatorEmailBodyRE, bodyRaw)))
		out = append(out, NormEmail{
			From:    from,
			To:      email,
			Subject: subject,
			Date:    when,
			Text:    text,
		})
	}

	/* B 形态：懒加载行（onclick → 详情页）。行内容按开标签锚点区间切片，
	 * 每行取该行内 from/subj/time 三要素与详情正文 */
	for m := generatorEmailLazyRowOpenRE.FindStringSubmatchIndex(src); m != nil; m = generatorEmailLazyRowOpenRE.FindStringSubmatchIndex(src) {
		msgPath := src[m[2]:m[3]]
		openStart := m[1]
		closeIdx := strings.Index(src[openStart:], "</div>\n<div")
		rowEnd := openStart + closeIdx
		if closeIdx < 0 {
			rowEnd = len(src)
		}
		raw := src[openStart:rowEnd]
		src = src[rowEnd:]
		from := generatorEmailStripTags(generatorEmailMatchFirst(generatorEmailFromRE, raw))
		subject := generatorEmailStripTags(generatorEmailMatchFirst(generatorEmailSubjRE, raw))
		when := generatorEmailStripTags(generatorEmailMatchFirst(generatorEmailTimeRE, raw))
		if from == "" || subject == "" {
			continue
		}
		msgID := generatorEmailMatchFirst(generatorEmailMsgIDRE, msgPath)
		text := ""
		if msgID != "" {
			detail, err := generatorEmailFetchDetail(&sess, msgID)
			if err == nil {
				text = detail
			}
		}
		out = append(out, NormEmail{
			From:    from,
			To:      email,
			Subject: subject,
			Date:    when,
			Text:    text,
		})
		if len(src) == 0 {
			break
		}
	}
	return out, nil
}

/* generatorEmailFetchDetail 拉取单信详情页并提取 mess_bodiyy 原文 */
func generatorEmailFetchDetail(sess *generatorEmailSession, msgID string) (string, error) {
	url := fmt.Sprintf("%s/%s/%s/%s", generatorEmailSite, sess.Domain, sess.User, msgID)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", generatorEmailUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", generatorEmailInbox)

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
		return "", fmt.Errorf("generator-email: 详情请求失败 http %d", resp.StatusCode)
	}
	return html.UnescapeString(generatorEmailStripTags(generatorEmailMatchFirst(generatorEmailBodyRE, string(body)))), nil
}
