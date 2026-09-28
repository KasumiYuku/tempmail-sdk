package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

/*
 * Internxt 渠道实现（internxt.com/temporary-email，Next.js + OpenNext）
 *
 * 调研实证结论（2026-09-28 curl + 浏览器抓包回验）：
 *   - 建箱 GET /api/temp-mail/create-email，读信 GET
 *     /api/temp-mail/get-inbox?email=<e>&token=<t>，详情 GET
 *     /api/temp-mail/get-message?email=<e>&token=<t>&messageId=<id>。
 *     create-email 仅接受 GET（POST 返回 405 Method not allowed）。
 *   - CSRF：首次 GET /temporary-email 响应 Set-Cookie csrfSecret=...
 *     与 XSRF-TOKEN=...（均 HttpOnly）。数据接口校验请求头
 *     csrf-token，其值必须与 Cookie jar 中 XSRF-TOKEN 一致：
 *     头=XSRF-TOKEN 值时 200；头=csrfSecret 值时恒定 500 Internal
 *     Server Error（现实现 0% 成功率即此因）；头非空但与任何 Cookie
 *     都不一致时 500；无 Cookie 时 403 {"message":"Invalid CSRF token"}。
 *   - 建箱响应：{"address":"<前缀>@uberip.com","token":"<十六进制>"}
 *     （uberip.com MX 指向 in.mail.tm）。健壮性提醒：XSRF-TOKEN 为
 *     LongLivedToken（包含 . 分段），每个 API 响应均会刷新
 *     Set-Cookie，故每次读信前都应从 Cookie 罐重取最新值作为 csrf-token
 *     头（实测 csrfSecret 恒定，XSRF-TOKEN 每次刷新）。
 *   - get-inbox 正常返回顶层数组（空箱 []）；错误 token 返回 401
 *     {"message":"Email has expired"}。
 *   - get-message 不存在的 messageId 返回 404
 *     {"message":"Message not found"}。
 *
 * 会话粘性：Generate 先 GET 临时邮箱页（夺取 csrfSecret + XSRF-TOKEN
 * 入全局 Cookie 罐），再带 csrf-token 头（XSRF-TOKEN 值）建箱；
 * token 保存 {address, token} JSON；GetEmails 每次调用前从罐中重取
 * 最新 XSRF-TOKEN 值作为 csrf-token 头，防罐中令牌被刷新后使用旧值。
 */

/* internxtSite internxt 临时邮箱页与 API 基址 */
const (
	internxtSite    = "https://internxt.com"
	internxtRef     = internxtSite + "/temporary-email"
	internxtAPIBase = internxtSite + "/api/temp-mail"
)

/* internxtSession Generate 时打包进 token 的会话凭据 */
type internxtSession struct {
	Address string `json:"address"`
	Token   string `json:"token"`
}

/* internxtPrepare 确保 Cookie 罐持有本域 csrfSecret 与 XSRF-TOKEN
 * （无则 GET 临时邮箱页夺取），并返回当前罐中 XSRF-TOKEN 值（每次
 * API 响应都会刷新该 Cookie，故调用方每次请求前都应重新调用）。
 */
func internxtPrepare() (string, error) {
	client := HTTPClient()
	if xsrf := internxtXSRFFromJar(client); xsrf != "" {
		return xsrf, nil
	}

	req, err := http.NewRequest(http.MethodGet, internxtRef, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if xsrf := internxtXSRFFromJar(client); xsrf != "" {
		return xsrf, nil
	}
	return "", fmt.Errorf("internxt: 未取得 XSRF-TOKEN Cookie")
}

/* internxtXSRFFromJar 从共享客户端 Cookie 罐取当前 XSRF-TOKEN 值 */
func internxtXSRFFromJar(client tls_client.HttpClient) string {
	u, _ := url.Parse(internxtSite)
	for _, c := range client.GetCookies(u) {
		if c.Name == "XSRF-TOKEN" && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

/* internxtGet 带 CSRF 头请求 internxt 数据接口，返回原始响应体
 * csrf-token 头取罐中 XSRF-TOKEN 值（无则通过 internxtPrepare 夺取），
 * 请求后 SSL 不回写头值；下一次请求需罐中 XSRF-TOKEN 新鲜。
 */
func internxtGet(path string, query url.Values) ([]byte, error) {
	csrfToken, err := internxtPrepare()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodGet, internxtAPIBase+path, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	for k, vs := range query {
		for _, val := range vs {
			q.Add(k, val)
		}
	}
	req.URL.RawQuery = q.Encode()

	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", internxtSite)
	req.Header.Set("Referer", internxtRef)
	req.Header.Set("csrf-token", csrfToken)

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
		return nil, fmt.Errorf("internxt: %s 失败 http %d: %s", path, resp.StatusCode, string(body))
	}
	return body, nil
}

/*
 * InternxtGenerate 创建 internxt.com 临时邮箱
 * internxtPrepare 确保罐中 XSRF-TOKEN，再 GET /api/temp-mail/create-email
 * （带 csrf-token 头且值为罐中 XSRF-TOKEN），响应
 * {"address","token"}（token 为收信令牌）。
 */
func InternxtGenerate() (*CreatedMailbox, error) {
	if _, err := internxtPrepare(); err != nil {
		return nil, err
	}

	body, err := internxtGet("/create-email", nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Address string `json:"address"`
		Token   string `json:"token"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("internxt: 解析建箱响应失败: %w", err)
	}
	if data.Address == "" || data.Token == "" || !strings.Contains(data.Address, "@") {
		return nil, fmt.Errorf("internxt: 创建邮箱响应缺少必要字段（address/token）")
	}

	sess, err := json.Marshal(internxtSession{Address: data.Address, Token: data.Token})
	if err != nil {
		return nil, err
	}
	return &CreatedMailbox{
		Channel: "internxt",
		Email:   data.Address,
		Token:   string(sess),
	}, nil
}

/*
 * InternxtGetEmails 获取 internxt.com 收件箱
 * GET /api/temp-mail/get-inbox?email=&token= 返回顶层数组（列表元素含
 * id/from/subject/date/seen 等）；逐条 GET /api/temp-mail/get-message
 * 拉单封全文（响应为单封对象，含 html 渲染全文）。内部请求的
 * csrf-token 头均由罐中最新 XSRF-TOKEN 提供（internxtGet 内部调用
 * internxtPrepare 兜底夺取）。
 * @param email - 邮箱地址
 * @param token - 会话凭据 JSON（address/token）
 */
func InternxtGetEmails(email, token string) ([]NormEmail, error) {
	var sess internxtSession
	if err := json.Unmarshal([]byte(token), &sess); err != nil {
		return nil, fmt.Errorf("internxt: 会话凭据解析失败: %w", err)
	}
	if sess.Address == "" || sess.Token == "" {
		return nil, fmt.Errorf("internxt: 会话凭据缺少必要字段")
	}
	if sess.Address != email {
		return nil, fmt.Errorf("internxt: 会话邮箱与查询邮箱不匹配")
	}

	q := url.Values{}
	q.Add("email", sess.Address)
	q.Add("token", sess.Token)
	body, err := internxtGet("/get-inbox", q)
	if err != nil {
		return nil, err
	}

	/* 收件箱为顶层数组（空箱 []），元素为列表摘要对象 */
	var list []map[string]interface{}
	if len(bytes.TrimSpace(body)) == 0 {
		list = []map[string]interface{}{}
	} else if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("internxt: 解析收件箱响应失败: %w", err)
	}

	out := make([]NormEmail, 0, len(list))
	for _, m := range list {
		if messageIDOf(m) == "" {
			out = append(out, NormalizeMap(m, email))
			continue
		}
		dq := url.Values{}
		dq.Add("email", sess.Address)
		dq.Add("token", sess.Token)
		dq.Add("messageId", messageIDOf(m))
		dbody, derr := internxtGet("/get-message", dq)
		if derr != nil {
			/* 详情失败回退列表摘要 */
			out = append(out, NormalizeMap(m, email))
			continue
		}
		var detail map[string]interface{}
		if err := json.Unmarshal(dbody, &detail); err != nil {
			out = append(out, NormalizeMap(m, email))
			continue
		}
		for k, v := range detail {
			if _, ok := m[k]; !ok {
				m[k] = v
			}
		}
		out = append(out, NormalizeMap(m, email))
	}
	return out, nil
}
