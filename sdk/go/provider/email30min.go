package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * 30minemail 渠道实现（30minemail.com）
 * 官网邮箱由服务端 16 位 hex 本地名识别：
 * - 建箱 GET /?generate 返回完整 HTML 页面（含 <地址>@30minemail.com，见 id=temp-email），
 *   本地名无效邮箱访问 messages.php 时返回 ok:false/expired:true，故必须经服务端建箱。
 * - 读信 GET /messages.php?email=<完整地址>&_=<unix毫秒>，
 *   响应 {"ok":true,"expired":false,"count":0,"emails":[],"expires_in":1733,
 *   "expiry_text":"29 minutes remaining","server_time":1790513860}。
 * emails 元素字段实测：{id,from,to,subject,date,html}（收件页 view.php 以
 * $("<div>").html(m.html) 渲染正文，相对时间取 data-timestamp=item["date"]，
 * 与站点探针抓包一致）。from/to/subject/date 均为纯文本，html 为完整正文。
 * 无认证、无 Cookie、无 CSRF。
 */

const thirtyMinEmailBase = "https://30minemail.com"
const thirtyMinEmailDomain = "30minemail.com"

/* thirtyMinEmailInboxResponse 读信响应 */
type thirtyMinEmailInboxResponse struct {
	OK        bool                     `json:"ok"`
	Expired   bool                     `json:"expired"`
	Count     int                      `json:"count"`
	Emails    []map[string]interface{} `json:"emails"`
	ExpiresIn int                      `json:"expires_in"`
}

/*
 * ThirtyMinEmailGenerate 创建 30minemail.com 临时邮箱
 * GET /?generate 返回 HTML 页面，从其中解析 16 位 hex 本地名地址；
 * token 复用完整地址（服务端以地址定位收件箱）。
 */
func ThirtyMinEmailGenerate() (*CreatedMailbox, error) {
	req, err := http.NewRequest(http.MethodGet, thirtyMinEmailBase+"/?generate", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	req.Header.Set("User-Agent", GetCurrentUA())

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
		return nil, fmt.Errorf("30minemail: 创建邮箱失败 http %d", resp.StatusCode)
	}

	page := string(body)
	idx := strings.Index(page, "@"+thirtyMinEmailDomain)
	if idx < 0 {
		return nil, fmt.Errorf("30minemail: 创建页面未找到邮箱地址")
	}
	/* 向前查找本地名起点：空白或 > 之后 */
	start := idx
	for start > 0 {
		c := page[start-1]
		if c == ' ' || c == '\n' || c == '\t' || c == '>' || c == '"' {
			break
		}
		start--
	}
	local := strings.TrimSpace(page[start:idx])
	if len(local) < 8 {
		return nil, fmt.Errorf("30minemail: 创建页面解析地址异常: %s", page[start:idx+len(thirtyMinEmailDomain)+1])
	}
	addr := local + "@" + thirtyMinEmailDomain

	return &CreatedMailbox{
		Channel: "30minemail",
		Email:   addr,
		Token:   addr,
	}, nil
}

/*
 * ThirtyMinEmailGetEmails 读取 30minemail.com 收件箱
 * GET /messages.php?email=<完整地址>&_=<unix毫秒>，模拟官方轮询参数。
 * @param email - 完整邮箱地址
 * @param token - 复用完整地址（服务端以地址定位收件箱）
 */
func ThirtyMinEmailGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("30minemail: 邮箱地址为空")
	}

	u := thirtyMinEmailBase + "/messages.php?email=" + url.QueryEscape(email) + "&_=" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())

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
		return nil, fmt.Errorf("30minemail: 读取收件箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data thirtyMinEmailInboxResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("30minemail: 解析收件箱响应失败: %w", err)
	}
	if !data.OK || data.Expired {
		return nil, fmt.Errorf("30minemail: 收件箱不可用或已过期: %s", string(body))
	}

	out := make([]NormEmail, 0, len(data.Emails))
	for _, m := range data.Emails {
		/* 列表元素无 to 字段时注入收件人地址 */
		if _, ok := m["to"]; !ok {
			m["to"] = email
		}
		out = append(out, NormalizeMap(m, email))
	}
	return out, nil
}
