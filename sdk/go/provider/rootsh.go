package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

/* rootsh.com（BccTo.CC）临时邮箱服务商
 * 流程：GET / 获取 session cookie → POST /applymail 申请/绑定邮箱
 *       POST /getmail 获取邮件列表 → GET /win/{编码邮箱}/{fid} 获取正文
 * 会话绑定：服务端要求 post.mail 与 mail cookie 绑定的地址一致，且共享客户端可能
 * 随 UA 随机化重建（cookie 罐丢失），故 GetEmails 每轮先幂等 POST /applymail
 * 复活/绑定本次会话，再查询（已存在地址返回同一地址+time，不重置邮箱）。
 * 时间游标：applymail 返回的 time 是 Unix 秒时间戳（申请时刻），不能用于增量查询；
 * getmail 表单的 time 是分页游标，首轮必须传 0，之后每轮用 getmail 响应中的 time
 * 持久化为下一轮游标。由于 Go 侧 GetEmails 签名无法回传新 token，游标在包内按邮箱
 * 持久化（rootshCursors），Generate 初始化 0。
 */

const rootshBaseURL = "https://rootsh.com"
const rootshDefaultDomain = "bccto.cc"

/* rootsh 渠道共享单一 cookie 罐，且 applymail 会改写服务端 post.mail 绑定，
 * 并用互斥锁串行化全部网络流程，避免并发收发时会话互相覆盖导致 403。 */
var rootshSessionMu sync.Mutex

/* rootsh 分页游标表：邮箱 -> getmail 协议分页游标 time（服务端口径，首轮为 "0"） */
var (
	rootshCursorMu sync.Mutex
	rootshCursors  = map[string]string{}
)

/* rootshSetXHRHeaders 设置 AJAX 请求所需的通用请求头 */
func rootshSetXHRHeaders(req *http.Request) {
	req.Header.Set("User-Agent", getCurrentUA())
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Referer", rootshBaseURL+"/")
	req.Header.Set("Origin", rootshBaseURL)
}

/* rootshRandomLocal 随机生成 10 位字母数字字符串作为邮箱本地部分 */
func rootshRandomLocal() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

/* rootshLoadCursor 读取邮箱当前的分页游标
 * 依次尝试：绑定后的权威地址、入参邮箱、SDK 传入 token，全部缺失时回退 "0"。 */
func rootshLoadCursor(boundEmail, email, token string) string {
	rootshCursorMu.Lock()
	defer rootshCursorMu.Unlock()
	if v, ok := rootshCursors[boundEmail]; ok && v != "" {
		return v
	}
	if v, ok := rootshCursors[email]; ok && v != "" {
		return v
	}
	if v := strings.TrimSpace(token); v != "" {
		return v
	}
	return "0"
}

/* rootshSaveCursor 持久化服务端回传的分页游标
 * 仅在响应 time 非空且非 "0" 时覆盖，避免游标回退导致邮件重复拉取。 */
func rootshSaveCursor(boundEmail string, t string) {
	if t = strings.TrimSpace(t); t != "" && t != "0" {
		rootshCursorMu.Lock()
		rootshCursors[boundEmail] = t
		rootshCursorMu.Unlock()
	}
}

/* RootshGenerate 创建 rootsh 临时邮箱
 * 1. GET / 获取 session cookie
 * 2. POST /applymail 申请邮箱地址
 * token 语义：getmail 分页游标的初始值 "0"（applymail 返回的 time 为 Unix 秒
 * 申请时刻，不是分页游标，不能作为 token 传给 getmail）。
 */
func RootshGenerate() (*CreatedMailbox, error) {
	rootshSessionMu.Lock()
	defer rootshSessionMu.Unlock()

	client := HTTPClient()

	/* 步骤 1：GET / 获取 session cookie */
	req, err := http.NewRequest("GET", rootshBaseURL+"/", nil)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 创建首页请求失败: %w", err)
	}
	req.Header.Set("User-Agent", getCurrentUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 获取首页失败: %w", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	if err := CheckHTTPStatus(resp, "rootsh home"); err != nil {
		return nil, err
	}

	/* 步骤 2：POST /applymail 创建邮箱 */
	local := rootshRandomLocal()
	emailAddr := fmt.Sprintf("%s@%s", local, rootshDefaultDomain)

	form := url.Values{}
	form.Set("mail", emailAddr)

	req2, err := http.NewRequest("POST", rootshBaseURL+"/applymail", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("rootsh: 创建申请请求失败: %w", err)
	}
	rootshSetXHRHeaders(req2)
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp2, err := client.Do(req2)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 申请邮箱失败: %w", err)
	}
	defer resp2.Body.Close()

	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 读取申请响应失败: %w", err)
	}

	if err := CheckHTTPStatus(resp2, "rootsh applymail"); err != nil {
		return nil, err
	}

	var applyResp struct {
		Success string      `json:"success"`
		User    string      `json:"user"`
		Time    json.Number `json:"time"`
		Tips    string      `json:"tips"`
	}
	if err := json.Unmarshal(body, &applyResp); err != nil {
		return nil, fmt.Errorf("rootsh: 解析申请响应失败: %w", err)
	}

	if applyResp.Success != "true" {
		return nil, fmt.Errorf("rootsh: 申请邮箱失败, tips=%s", applyResp.Tips)
	}

	/* 使用服务端返回的邮箱地址（可能与请求的不同） */
	actualEmail := strings.TrimSpace(applyResp.User)
	if actualEmail == "" {
		actualEmail = emailAddr
	}

	/* token 为 getmail 分页游标初始值 "0"，并在游标表中登记；applymail 的 time 是
	 * Unix 秒申请时刻，仅作参考，不再用作增量查询游标。 */
	tokenTime := "0"
	rootshCursorMu.Lock()
	rootshCursors[actualEmail] = tokenTime
	rootshCursorMu.Unlock()

	return &CreatedMailbox{
		Channel: "rootsh",
		Email:   actualEmail,
		Token:   tokenTime,
	}, nil
}

/* RootshGetEmails 获取 rootsh 邮件列表
 * 1. GET / 确保当前客户端会话 cookie 就绪
 * 2. POST /applymail 幂等复活/绑定本次会话（mail 必须等于 cookie 绑定地址，否则 getmail 403）
 * 3. POST /getmail 增量获取邮件列表（time 为上一轮响应回传的分页游标，首轮 0）
 * 4. 对每封邮件 GET /win/{编码邮箱}/{fid} 获取 HTML 正文（失败仅留空正文，不判渠道失败）
 */
func RootshGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("rootsh: 邮箱地址为空")
	}

	rootshSessionMu.Lock()
	defer rootshSessionMu.Unlock()

	client := HTTPClient()

	/* 步骤 1：GET / 确保会话 cookie 就绪（共享客户端可能因 UA 随机化刚重建，cookie 罐为空） */
	reqHome, err := http.NewRequest("GET", rootshBaseURL+"/", nil)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 创建首页请求失败: %w", err)
	}
	reqHome.Header.Set("User-Agent", getCurrentUA())
	reqHome.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	reqHome.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	respHome, err := client.Do(reqHome)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 获取首页失败: %w", err)
	}
	defer respHome.Body.Close()
	io.ReadAll(respHome.Body)

	if err := CheckHTTPStatus(respHome, "rootsh home"); err != nil {
		return nil, err
	}

	/* 步骤 2：POST /applymail 幂等复活/绑定会话
	 * 服务端校验 post.mail 与 mail cookie 绑定的地址一致，GetEmails 与 Generate
	 * 可能不在同一 cookie 罐（共享客户端重建），必须先重新绑定；对已存在地址
	 * 服务端返回同一地址+time，不重置邮箱，属于幂等操作。 */
	bindForm := url.Values{}
	bindForm.Set("mail", email)

	reqBind, err := http.NewRequest("POST", rootshBaseURL+"/applymail", strings.NewReader(bindForm.Encode()))
	if err != nil {
		return nil, fmt.Errorf("rootsh: 创建会话绑定请求失败: %w", err)
	}
	rootshSetXHRHeaders(reqBind)
	reqBind.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	respBind, err := client.Do(reqBind)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 会话绑定失败: %w", err)
	}
	defer respBind.Body.Close()

	bodyBind, err := io.ReadAll(respBind.Body)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 读取会话绑定响应失败: %w", err)
	}

	if err := CheckHTTPStatus(respBind, "rootsh applymail bind"); err != nil {
		return nil, err
	}

	var bindResp struct {
		Success string      `json:"success"`
		User    string      `json:"user"`
		Time    json.Number `json:"time"`
		Tips    string      `json:"tips"`
	}
	if err := json.Unmarshal(bodyBind, &bindResp); err != nil {
		return nil, fmt.Errorf("rootsh: 解析会话绑定响应失败: %w", err)
	}

	if bindResp.Success != "true" {
		return nil, fmt.Errorf("rootsh: 会话绑定失败, tips=%s", bindResp.Tips)
	}

	/* 若服务端下发了权威地址（邮箱过期后 applymail 可能换发新地址），以服务端为准 */
	boundEmail := strings.TrimSpace(bindResp.User)
	if boundEmail == "" {
		boundEmail = email
	}

	/* 步骤 3：POST /getmail 获取邮件列表（time 为上一轮持久化的分页游标，首轮 0） */
	lastCheckTime := rootshLoadCursor(boundEmail, email, token)

	now := fmt.Sprintf("%d", time.Now().UnixMilli())
	form := url.Values{}
	form.Set("mail", boundEmail)
	form.Set("time", lastCheckTime)
	form.Set("_", now)

	req, err := http.NewRequest("POST", rootshBaseURL+"/getmail", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("rootsh: 创建获取邮件请求失败: %w", err)
	}
	rootshSetXHRHeaders(req)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 获取邮件列表失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rootsh: 读取邮件列表响应失败: %w", err)
	}

	if err := CheckHTTPStatus(resp, "rootsh getmail"); err != nil {
		return nil, err
	}

	/* 解析邮件列表响应
	 * mail 数组中每个元素: [displayName, fromEmail, subject, dateStr, fid, receivedTime]
	 * time 为服务端回传的下一轮分页游标，本轮即刻持久化。 */
	var mailResp struct {
		Success string          `json:"success"`
		To      string          `json:"to"`
		Time    json.Number     `json:"time"`
		Mail    [][]interface{} `json:"mail"`
	}
	if err := json.Unmarshal(body, &mailResp); err != nil {
		return nil, fmt.Errorf("rootsh: 解析邮件列表失败: %w", err)
	}

	if mailResp.Success != "true" {
		return nil, fmt.Errorf("rootsh: 获取邮件列表失败")
	}

	rootshSaveCursor(boundEmail, mailResp.Time.String())

	if len(mailResp.Mail) == 0 {
		return []NormEmail{}, nil
	}

	emails := make([]NormEmail, 0, len(mailResp.Mail))
	for _, item := range mailResp.Mail {
		if len(item) < 6 {
			continue
		}

		/* 提取字段：[displayName, fromEmail, subject, dateStr, fid, receivedTime] */
		fromEmail := rootshToString(item[1])
		displayName := rootshToString(item[0])
		subject := rootshToString(item[2])
		dateStr := rootshToString(item[3])
		fid := rootshToString(item[4])

		/* 构造发件人地址：如果有 displayName 则格式化为 "name <email>" */
		fromAddr := fromEmail
		if displayName != "" && displayName != fromEmail {
			fromAddr = fmt.Sprintf("%s <%s>", displayName, fromEmail)
		}

		/* GET /win/{编码邮箱}/{fid} 获取邮件正文；平台正文链路已断（详见
		 * rootshFetchMailBody 注释），失败时正文如实留空，列表字段照常返回。 */
		htmlBody := rootshFetchMailBody(client, fid, boundEmail)

		flat := map[string]interface{}{
			"id":      fid,
			"from":    fromAddr,
			"to":      boundEmail,
			"subject": subject,
			"date":    dateStr,
			"html":    htmlBody,
		}
		emails = append(emails, NormalizeMap(flat, boundEmail))
	}

	return emails, nil
}

/* rootshFetchMailBody 获取单封邮件的 HTML 正文
 * 站点已下线 POST /viewmail（404），新版路由为 GET /win/{编码邮箱}/{fid}：
 *   - 邮箱编码规则：先 replace("@", "!)!)^")，再 replace(".", "+--=_")；
 *   - fid 为 getmail 返回的邮件标识（含 .eml 后缀），作为路径段转义后拼接。
 * 实测平台对新路由的任意 fid 均返回 "Mail does not exist"（正文链路在服务端已断），
 * 当前正文必然落空。SDK 策略：正文缺失/404/不存在一律留空返回，绝不升级为渠道整体失败，
 * 列表级字段（subject/from/date）判定保留。
 * 兼容性：若平台修复后恢复 JSON 信封（{"success":"true","mail":"..."}）则取 mail 字段；
 * 若直接回传正文 HTML 页面且不含错误文案，则整页作为正文。 */
func rootshFetchMailBody(client interface {
	Do(*http.Request) (*http.Response, error)
}, fid, email string) string {
	if fid == "" {
		return ""
	}

	/* 邮箱编码：@ -> !)!)^，. -> +--=_（编码后仅剩路径合法字符，直接拼接） */
	encoded := strings.ReplaceAll(email, "@", "!)!)^")
	encoded = strings.ReplaceAll(encoded, ".", "+--=_")

	req, err := http.NewRequest("GET", rootshBaseURL+"/win/"+encoded+"/"+url.PathEscape(fid), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", getCurrentUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", rootshBaseURL+"/")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}

	/* 优先按旧接口的 JSON 信封解析（平台修复后可能恢复）：success=true 且 mail 非空才采用 */
	var viewResp struct {
		Success    string `json:"success"`
		Mail       string `json:"mail"`
		Attachment string `json:"attachment"`
	}
	if err := json.Unmarshal(body, &viewResp); err == nil {
		if viewResp.Success == "true" && strings.TrimSpace(viewResp.Mail) != "" {
			return viewResp.Mail
		}
		/* success!=true 或无正文，视为正文缺失（含 "Mail does not exist" 场景） */
		return ""
	}

	/* 非 JSON 响应：错误文案视为无正文；否则视为平台直接回传的正文 HTML 页面 */
	text := strings.TrimSpace(string(body))
	if text == "" || strings.Contains(text, "Mail does not exist") {
		return ""
	}
	return string(body)
}

/* rootshToString 将 interface{} 安全转换为字符串 */
func rootshToString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return fmt.Sprintf("%.0f", val)
	case json.Number:
		return val.String()
	default:
		return fmt.Sprintf("%v", val)
	}
}
