package provider

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	gohtml "golang.org/x/net/html"
)

/*
 * TempMailGG 渠道实现（temp-mail.gg，Laravel + Livewire v3）
 *
 * 调研实证结论（2026-09-27 浏览器 + curl 全链路抓包回验）：
 *   纯 HTTP 可实现，全站无验证码（Turnstile 仅 Cloudflare 侧，Livewire
 *   update 不校验 turnstileToken）。
 *   - GET https://temp-mail.gg/ 返回 data-csrf 属性令牌、XSRF-TOKEN /
 *     tempmail_session cookies 与初始 wire:snapshot（JSON；data.email
 *     初始为空，邮箱必须由 Livewire generateEmail 显式生成）。
 *   - POST https://temp-mail.gg/livewire/update 是唯一边界（同站 fetch
 *     协议）：JSON body 顶层 _token（=data-csrf）+ components[0]：
 *     {snapshot, updates:{}, calls:[{path:"",method:"<call>",
 *     params:[...]}]}。generateEmail 生成邮箱并回传新 snapshot，响应
 *     components[0].effects.html 含收件箱整块 UI（列表条目：
 *     <div wire:click="selectEmail(<数字id>)">…h3=发件人邮箱 …
 *     p=主题 …p=正文预览 …span="N seconds ago"）。200 且 calls 为空的
 *     update 即平台 20 秒轮询刷信形态（首次调用后响应带
 *     Set-Cookie: temp_email=…，粘住邮箱）。
 *   - 详情：update calls=[{method:"selectEmail",params:[<数字id>]}]，
 *     响应 effects.html 的邮件模态框含 From / 时间 / Plain Text 全文
 *     （x-show="activeTab === 'text'" 区块）。
 *   - Livewire 会话轮换：Laravel 每次 livewire/update 都轮换会话
 *     Cookie；同值重放会 419。curl 复刻必须以响应 Set-Cookie 覆写
 *     后续请求——Go 端 tls-client Cookie 罐按浏览器语义自动完成。
 *   - 域名池（统一 MX mailser.temp-mail.gg）：fibmail.com /
 *     gensmail.com / gimbmail.com / bont.edu.gr / topa.edu.pl。
 *
 * 会话粘性：Generate 时响应 Set-Cookie temp_email + tempmail_session
 * 落全局 Cookie 罐；token 内保存 {email, snapshot} 凭据串。GetEmails
 * 复用 token 中的快照轮询/点开详情，并以响应快照 data.email 断言会话
 * 仍指向本邮箱（防全局 Cookie 罐被并行会话覆盖后串箱）。
 */

const tempMailGGOrigin = "https://temp-mail.gg"

/* tempMailGGTokenPrefix 渠道凭据串前缀，用于识别会话接管 */
const tempMailGGTokenPrefix = "temp-mail-gg|"

/*
 * tempMailGGSnapshot Livewire v3 组件快照结构（data 为响应状态，
 * memo/checksum 原样透传，不做字段级解释）。
 */
type tempMailGGSnapshot struct {
	Data     map[string]interface{} `json:"data"`
	Memo     map[string]interface{} `json:"memo"`
	Checksum string                 `json:"checksum"`
}

/* tempMailGGSession Generate 时打包进 token 的会话凭据 */
type tempMailGGSession struct {
	Email    string `json:"email"`
	CSRF     string `json:"csrf"`
	Snapshot string `json:"snapshot"`
}

/* tempMailGGUpdateResponse livewire/update 响应外壳 */
type tempMailGGUpdateResponse struct {
	Components []struct {
		Snapshot string `json:"snapshot"`
		Effects  struct {
			Returns []interface{} `json:"returns"`
			HTML    string        `json:"html"`
		} `json:"effects"`
	} `json:"components"`
}

/* tempMailGGListRow Inbox 列表条目（从 effects.html 解析） */
type tempMailGGListRow struct {
	ID      string
	From    string
	Subject string
	Preview string
	When    string
}

var (
	ggSnapshotRe = regexp.MustCompile(`wire:snapshot="([^"]*)"`)
	ggDataCsrfRe = regexp.MustCompile(`data-csrf="([^"]*)"`)
	/* 生成区块中呈现当前邮箱的 div（class 含 font-mono text-sm） */
	ggEmailDivRe = regexp.MustCompile(`(?s)<div[^>]*class="[^"]*font-mono[^"]*text-sm[^"]*"[^>]*>\s*([^<]+)\s*</div>`)
	ggRelativeRe = regexp.MustCompile(`(?i)^\s*(\d+)\s+(second|minute|hour|day)s?\s+ago\s*$`)
)

/* tempMailGGHTMLToText 将 HTML 片段转为纯文本（去 script/style/标签 + 反转义） */
func tempMailGGHTMLToText(src string) string {
	s := regexp.MustCompile(`(?is)<(script|style)[\s\S]*?</\1>`).ReplaceAllString(src, " ")
	s = regexp.MustCompile(`<?s><[^>]+>`).ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\x00", "")
	return strings.TrimSpace(html.UnescapeString(s))
}

/* tempMailGGSetPostHeaders 设置 livewire/update 同步请求头（同站 fetch 全套） */
func tempMailGGSetPostHeaders(req *http.Request) {
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Accept", "text/html, application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Livewire", "")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", tempMailGGOrigin)
	req.Header.Set("Referer", tempMailGGOrigin+"/")
}

/*
 * tempMailGGUpdate 调用 livewire/update（Cookie 罐自动吞新会话 Cookie）
 * @param snapshot 当前组件快照 JSON 串
 * @param csrf     data-csrf 令牌（顶层 _token）
 * @param method   组件方法（"" 表示纯轮询，即平台 20s 自动刷形态）
 * @param params   方法参数
 */
func tempMailGGUpdate(snapshot, csrf, method string, params []interface{}) (*tempMailGGUpdateResponse, error) {
	var calls []map[string]interface{}
	if method != "" {
		calls = []map[string]interface{}{{"path": "", "method": method, "params": params}}
	}
	payloadBytes, err := json.Marshal(map[string]interface{}{
		"_token": csrf,
		"components": []map[string]interface{}{{
			"snapshot": snapshot,
			"updates":  map[string]interface{}{},
			"calls":    calls,
		}},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", tempMailGGOrigin+"/livewire/update", strings.NewReader(string(payloadBytes)))
	if err != nil {
		return nil, err
	}
	tempMailGGSetPostHeaders(req)

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 419 {
		return nil, fmt.Errorf("temp-mail-gg: livewire 会话过期（419），请重新 Generate")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("temp-mail-gg: livewire/update http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out tempMailGGUpdateResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("temp-mail-gg: 解析 update 响应失败: %w", err)
	}
	return &out, nil
}

/* tempMailGGParseSnapshot 解析快照 JSON 串（memo 不解释，原样透传用） */
func tempMailGGParseSnapshot(raw string) (*tempMailGGSnapshot, error) {
	var snap tempMailGGSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return nil, fmt.Errorf("temp-mail-gg: 解析快照失败: %w", err)
	}
	if snap.Data == nil {
		snap.Data = map[string]interface{}{}
	}
	if snap.Memo == nil {
		snap.Memo = map[string]interface{}{}
	}
	return &snap, nil
}

/* tempMailGGSnapString 快照结构序列化回 JSON 串（各字段保序不变量，与原串一致） */
func tempMailGGSnapString(snap *tempMailGGSnapshot) string {
	b, _ := json.Marshal(snap)
	return string(b)
}

/*
 * TempMailGGGenerate 创建 temp-mail.gg 临时邮箱
 * @param name 未使用（该渠道无自定义前缀，固定随机生成）
 * GET 首页取 CSRF + 初始快照 -> update calls=generateEmail ->
 * 从响应快照取 data.email，并把 {email, csrf, snapshot} 打成凭据串。
 */
func TempMailGGGenerate(name string) (*CreatedMailbox, error) {
	req, err := http.NewRequest("GET", tempMailGGOrigin, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("temp-mail-gg: 建立会话失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("temp-mail-gg: 首页 http %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	page := string(body)
	mCSRF := ggDataCsrfRe.FindStringSubmatch(page)
	mSnap := ggSnapshotRe.FindStringSubmatch(page)
	if mCSRF == nil || mSnap == nil {
		return nil, fmt.Errorf("temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱")
	}
	csrf := mCSRF[1]
	/* HTML 属性内的快照 JSON 是双转义态（&quot;），逐层反转义为原始 JSON */
	snapRaw := html.UnescapeString(mSnap[1])

	/* generateEmail: 平台免费额度为免登录每时段 5 个，耗尽时响应无 email */
	updateResp, err := tempMailGGUpdate(snapRaw, csrf, "generateEmail", nil)
	if err != nil {
		return nil, err
	}
	if len(updateResp.Components) == 0 || updateResp.Components[0].Snapshot == "" {
		return nil, fmt.Errorf("temp-mail-gg: generateEmail 响应异常（components 缺失）")
	}
	snap2, err := tempMailGGParseSnapshot(updateResp.Components[0].Snapshot)
	if err != nil {
		return nil, err
	}
	email, _ := snap2.Data["email"].(string)
	if email = strings.TrimSpace(email); email == "" {
		return nil, fmt.Errorf("temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录配额（每时段 5 个）")
	}

	tokenBytes, err := json.Marshal(tempMailGGSession{
		Email:    email,
		CSRF:     csrf,
		Snapshot: tempMailGGSnapString(snap2),
	})
	if err != nil {
		return nil, err
	}

	return &CreatedMailbox{
		Channel:   "temp-mail-gg",
		Email:     email,
		Token:     tempMailGGTokenPrefix + string(tokenBytes),
		ExpiresAt: time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
	}, nil
}

/* tempMailGGDecodeSession 解析凭据串 -> 会话（email + csrf + 快照） */
func tempMailGGDecodeSession(token string) (*tempMailGGSession, error) {
	if !strings.HasPrefix(token, tempMailGGTokenPrefix) {
		return nil, fmt.Errorf("temp-mail-gg: 凭据串前缀不符，请重新 Generate")
	}
	var sess tempMailGGSession
	if err := json.Unmarshal([]byte(strings.TrimPrefix(token, tempMailGGTokenPrefix)), &sess); err != nil {
		return nil, fmt.Errorf("temp-mail-gg: 解析凭据串失败: %w", err)
	}
	if sess.Email == "" || sess.Snapshot == "" {
		return nil, fmt.Errorf("temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate")
	}
	return &sess, nil
}

/* tempMailGGAttrsOf 取节点属性 map（无则空） */
func tempMailGGAttrsOf(n *gohtml.Node) map[string]string {
	m := map[string]string{}
	for _, a := range n.Attr {
		if a.Namespace == "" {
			m[a.Key] = a.Val
		}
	}
	return m
}

/* tempMailGGHasClass 判断节点 class 属性是否包含指定类名 */
func tempMailGGHasClass(attrs map[string]string, class string) bool {
	for _, cls := range strings.Fields(attrs["class"]) {
		if cls == class {
			return true
		}
	}
	return false
}

/* tempMailGGTextOf 提取节点下所有文本（br 转为换行） */
func tempMailGGTextOf(n *gohtml.Node) string {
	var b strings.Builder
	var walk func(*gohtml.Node)
	walk = func(x *gohtml.Node) {
		if x.Type == gohtml.TextNode {
			b.WriteString(x.Data)
			return
		}
		if x.Type == gohtml.ElementNode && x.Data == "br" {
			b.WriteString("\n")
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

/*
 * tempMailGGParseList 用 DOM 解析轮询响应 effects.html 的 Inbox 条目。
 * 条目容器为 <div wire:click="selectEmail(<数字id>)">，
 * 其内 h3=发件人邮箱、p(text-zinc-300)=主题、p(line-clamp-2)=正文预览、
 * span(text-xs)=相对时间。
 */
func tempMailGGParseList(htmlBlock string) []tempMailGGListRow {
	doc, err := gohtml.Parse(strings.NewReader(htmlBlock))
	if err != nil {
		return nil
	}
	rows := make([]tempMailGGListRow, 0)
	var walk func(*gohtml.Node)
	walk = func(n *gohtml.Node) {
		if n.Type == gohtml.ElementNode {
			attrs := tempMailGGAttrsOf(n)
			if v, ok := attrs["wire:click"]; ok && strings.HasPrefix(v, "selectEmail(") {
				row := tempMailGGListRow{ID: strings.Trim(strings.TrimPrefix(strings.TrimSuffix(v, ")"), "selectEmail("), " ")}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type != gohtml.ElementNode {
						continue
					}
					cAttrs := tempMailGGAttrsOf(c)
					switch {
					case c.Data == "h3" && tempMailGGHasClass(cAttrs, "font-semibold"):
						if row.From == "" {
							row.From = tempMailGGTextOf(c)
						}
					case c.Data == "p" && tempMailGGHasClass(cAttrs, "text-zinc-300"):
						if row.Subject == "" {
							row.Subject = tempMailGGTextOf(c)
						}
					case c.Data == "p" && tempMailGGHasClass(cAttrs, "line-clamp-2"):
						if row.Preview == "" {
							row.Preview = tempMailGGTextOf(c)
						}
					case c.Data == "span" && tempMailGGHasClass(cAttrs, "text-xs"):
						if row.When == "" {
							row.When = tempMailGGTextOf(c)
						}
					}
				}
				if row.ID != "" {
					rows = append(rows, row)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return rows
}

/* tempMailGGParseDetail 解析 selectEmail 详情视图（模态框）的正文/发件人 */
func tempMailGGParseDetail(htmlBlock string) (from, subject, text, htmlBody string, ok bool) {
	doc, err := gohtml.Parse(strings.NewReader(htmlBlock))
	if err != nil {
		return "", "", "", "", false
	}
	var walk func(*gohtml.Node)
	walk = func(n *gohtml.Node) {
		if n.Type != gohtml.ElementNode {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			return
		}
		attrs := tempMailGGAttrsOf(n)
		switch {
		case n.Data == "h3" && tempMailGGHasClass(attrs, "text-xl"):
			subject = tempMailGGTextOf(n)
		case n.Data == "span" && strings.HasPrefix(tempMailGGTextOf(n), "From:"):
			from = strings.TrimSpace(strings.TrimPrefix(tempMailGGTextOf(n), "From:"))
		case n.Data == "div" && strings.Contains(attrs["x-show"], "activeTab === 'text'"):
			text = tempMailGGTextOf(n)
		case n.Data == "div" && strings.Contains(attrs["x-show"], "activeTab === 'html'"):
			/* HTML 页签容器由组件下放富文本；无内容时为空字符串 */
			inner := strings.TrimSpace(tempMailGGTextOf(n))
			if inner != "" {
				htmlBody = n.Data
				_ = inner
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if subject == "" && text == "" && from == "" {
		return "", "", "", "", false
	}
	return from, subject, text, htmlBody, true
}

/* tempMailGGParseRelative 解析相对时间（"N seconds/minutes/hours/days ago"）为 UTC RFC3339 */
func tempMailGGParseRelative(s string, now time.Time) string {
	m := ggRelativeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return now.UTC().Format(time.RFC3339)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return now.UTC().Format(time.RFC3339)
	}
	var d time.Duration
	switch strings.ToLower(m[2]) {
	case "second":
		d = time.Second
	case "minute":
		d = time.Minute
	case "hour":
		d = time.Hour
	case "day":
		d = 24 * time.Hour
	}
	return now.Add(-time.Duration(n) * d).UTC().Format(time.RFC3339)
}

/*
 * TempMailGGGetEmails 读取 temp-mail.gg 收件箱
 * @param email 邮箱地址（与 token 内会话邮箱一致才继续）
 * @param token 凭据串（prefix | {email,csrf,snapshot}）
 * 流程：轮询 update(无 calls) 取 Inbox 列表 -> 对每封 selectEmail
 * 提取详情正文。轮询响应快照中的 data.email 与请求邮箱不一致时返回
 * 错误（防全局 Cookie 罐被并发会话覆盖后串箱）。
 */
func TempMailGGGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("temp-mail-gg: 邮箱为空，请重新 Generate")
	}
	sess, err := tempMailGGDecodeSession(token)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(sess.Email, email) {
		return nil, fmt.Errorf("temp-mail-gg: 邮箱与凭据不匹配（%s != %s），请重新 Generate", sess.Email, email)
	}

	/* 轮询刷新（calls 为空 = 平台 20s 自动刷新形态） */
	respPoll, err := tempMailGGUpdate(sess.Snapshot, sess.CSRF, "", nil)
	if err != nil {
		return nil, err
	}
	if len(respPoll.Components) == 0 {
		return nil, fmt.Errorf("temp-mail-gg: 轮询响应异常（components 缺失）")
	}
	c0 := respPoll.Components[0]
	if c0.Snapshot != "" {
		if snap, e := tempMailGGParseSnapshot(c0.Snapshot); e == nil {
			if current, _ := snap.Data["email"].(string); strings.TrimSpace(current) != "" && !strings.EqualFold(strings.TrimSpace(current), email) {
				return nil, fmt.Errorf("temp-mail-gg: 会话已被切换至 %s（与请求邮箱 %s 不一致），请重新 Generate", strings.TrimSpace(current), email)
			}
		}
	}
	htmlBlock := c0.Effects.HTML
	if strings.TrimSpace(htmlBlock) == "" {
		return nil, fmt.Errorf("temp-mail-gg: 轮询响应无 effects.html，会话可能已失效")
	}

	listRows := tempMailGGParseList(htmlBlock)
	if len(listRows) == 0 {
		return nil, nil
	}

	/* 逐封拉详情正文；详情失败回退列表字段（不中断整批） */
	latestSnap := sess.Snapshot
	if c0.Snapshot != "" {
		latestSnap = c0.Snapshot
	}
	now := time.Now().UTC()
	out := make([]NormEmail, 0, len(listRows))
	for _, row := range listRows {
		ne := tempMailGGListToNorm(row, email, now)
		if ne == nil {
			continue
		}
		idNum, convErr := strconv.ParseInt(row.ID, 10, 64)
		if convErr == nil {
			if respDetail, e := tempMailGGUpdate(latestSnap, sess.CSRF, "selectEmail", []interface{}{idNum}); e == nil && len(respDetail.Components) > 0 {
				if respDetail.Components[0].Snapshot != "" {
					latestSnap = respDetail.Components[0].Snapshot
				}
				if from, subject, text, _, ok := tempMailGGParseDetail(respDetail.Components[0].Effects.HTML); ok {
					if from != "" {
						ne.From = from
					}
					if subject != "" {
						ne.Subject = subject
					}
					if text != "" {
						ne.Text = text
					}
				}
			}
		}
		if ne.Text == "" {
			ne.Text = ne.Subject
		}
		if ne.HTML == "" {
			ne.HTML = "<html><body><pre>" + html.EscapeString(ne.Text) + "</pre></body></html>"
		}
		out = append(out, *ne)
	}
	return out, nil
}

/* tempMailGGListToNorm 列表行转 NormEmail（详情前的基础归一） */
func tempMailGGListToNorm(row tempMailGGListRow, email string, now time.Time) *NormEmail {
	if row.ID == "" {
		return nil
	}
	date := tempMailGGParseRelative(row.When, now)
	ne := &NormEmail{
		ID:      row.ID,
		From:    row.From,
		To:      email,
		Subject: row.Subject,
		Date:    date,
		Text:    row.Preview,
	}
	return ne
}
