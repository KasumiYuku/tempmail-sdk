package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"regexp"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

/**
 * 24mail.chacuo.net — sid 会话绑定型临时邮箱（平台为 HTTP，非 HTTPS）。
 * 协议与平台前端 24mail.js 对齐：
 *   1. GET / 首页签发会话 Cookie（sid）与内联随机本地名（converts 输入框有效 value）。
 *   2. POST data=<本地名>&type=set&arg=    绑定/设名：成功 data:[name]，被占或无变化 data:[false]。
 *   3. POST data=<本地名>&type=refresh&arg= 收件列表：data:[{stat:...,user:{...},list:[{MID,FROM,TO,SUBJECT,SENDTIME,ISREAD,SIZE}]}]
 *   4. POST data=<本地名>&type=mailinfo&arg=f=<MID> 正文：data:[[{MID,...,RLINK},[{"DATA":["纯文本","正文"],"TYPE":"0"}]]]
 * 会话隔离：generate/getEmails 均使用无 Cookie 罐客户端，请求级显式携带 "Cookie: sid=..."，
 * 避免共享 Cookie 罐导致跨邮箱串会话。
 */

const chacuoBaseURL = "http://24mail.chacuo.net"

/* 平台可选域名（页面 select 两个选项），生成固定使用主域 chacuo.net */
var chacuoDomains = []string{"chacuo.net", "027168.com"}

var (
	/* converts 输入框所在完整标签；页面该标签存在重复 value 属性，浏览器按规范取第一个 */
	chacuoConvertsTagRe = regexp.MustCompile(`(?is)<input[^>]*name="converts"[^>]*>`)
	/* 标签内第一个 value="..." 属性 */
	chacuoValueRe = regexp.MustCompile(`(?i)\bvalue\s*=\s*"([^"]*)"`)
	/* 合法本地名：小写字母与数字，3~32 位（与页面 valid 规则一致） */
	chacuoNameValidRe = regexp.MustCompile(`^[a-z0-9]{3,32}$`)
)

/* chacuoToken 会话票证：sid 会话号 + 已绑定本地名，序列化为 JSON 后存入 SDK 内部 token */
type chacuoToken struct {
	Sid  string `json:"sid"`
	Name string `json:"name"`
}

/* chacuoRandomLocal 生成指定长度的小写字母数字随机本地名 */
func chacuoRandomLocal(length int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, length)
	for i := range buf {
		buf[i] = chars[rand.Intn(len(chars))]
	}
	return string(buf)
}

/* chacuoSetHeaders 设置 24mail.chacuo.net POST 表单请求的通用浏览器头 */
func chacuoSetHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Origin", chacuoBaseURL)
	req.Header.Set("Referer", chacuoBaseURL+"/")
	req.Header.Set("User-Agent", chacuoUA())
	req.Header.Set("x-requested-with", "XMLHttpRequest")
}

/* chacuoUA 返回请求使用的浏览器 UA（全局 UA 不可用时使用固定默认值） */
func chacuoUA() string {
	ua := getCurrentUA()
	if ua == "" {
		ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	}
	return ua
}

/* chacuoHomeName 从首页 HTML 提取平台签发的随机本地名（converts 输入框有效 value，无则 SDK 随机） */
func chacuoHomeName(htmlRaw []byte) string {
	tag := chacuoConvertsTagRe.Find(htmlRaw)
	if tag == nil {
		return ""
	}
	match := chacuoValueRe.FindSubmatch(tag)
	if match == nil {
		return ""
	}
	name := string(match[1])
	if !chacuoNameValidRe.MatchString(name) {
		return ""
	}
	return name
}

/* chacuoDoPost 发送 data/type/arg 表单 POST 并显式携带 sid Cookie，返回响应体 */
func chacuoDoPost(client tls_client.HttpClient, sid, dataVal, typ, arg string) ([]byte, error) {
	body := fmt.Sprintf("data=%s&type=%s&arg=%s", dataVal, typ, arg)
	req, err := http.NewRequest("POST", chacuoBaseURL+"/", strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("24mail-chacuo: 创建 POST 请求失败: %w", err)
	}
	chacuoSetHeaders(req)
	req.Header.Set("Cookie", "sid="+sid)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("24mail-chacuo: POST %s 请求失败: %w", typ, err)
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "24mail-chacuo POST "+typ); err != nil {
		return nil, err
	}
	return io.ReadAll(resp.Body)
}

/* chacuoFetchBody 拉取单封邮件正文：mailinfo → data[0][1][0].DATA（优先 DATA[1]，回退 DATA[0]） */
func chacuoFetchBody(client tls_client.HttpClient, sid, name, mid string) string {
	respBody, err := chacuoDoPost(client, sid, name, "mailinfo", "f="+mid)
	if err != nil {
		return ""
	}
	var infoResp struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &infoResp); err != nil || len(infoResp.Data) == 0 {
		return ""
	}

	/* data[0] = [邮件元信息, [{"DATA":[...]}]，空结果形如 [[]] */
	var inner []json.RawMessage
	if err := json.Unmarshal(infoResp.Data[0], &inner); err != nil || len(inner) < 2 {
		return ""
	}
	var parts []struct {
		DATA json.RawMessage `json:"DATA"`
	}
	if err := json.Unmarshal(inner[1], &parts); err != nil || len(parts) == 0 {
		return ""
	}

	var dataArr []string
	if err := json.Unmarshal(parts[0].DATA, &dataArr); err != nil {
		/* 数据层兜底：DATA 为单个字符串 */
		var single string
		if err := json.Unmarshal(parts[0].DATA, &single); err != nil {
			return ""
		}
		return single
	}
	/* 与前端一致：优先 DATA[1]，为空回退 DATA[0] */
	if len(dataArr) > 1 && dataArr[1] != "" {
		return dataArr[1]
	}
	if len(dataArr) > 0 {
		return dataArr[0]
	}
	return ""
}

/* chacuoIsRead 解析列表项 ISREAD 字段（数字 1 / 字符串 "1" / true 均为已读） */
func chacuoIsRead(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "1" || s == `"1"` || s == "true"
}

/* TwentyfourmailChacuoGenerate 创建 24mail-chacuo 临时邮箱：GET 首页取 sid，POST type=set 绑定名字 */
func TwentyfourmailChacuoGenerate() (*CreatedMailbox, error) {
	/* 使用无 Cookie 罐客户端，避免共享罐中历史 sid 干扰本邮箱会话 */
	client := HTTPClientNoCookieJar()

	/* 步骤1：GET 首页，获取 sid 会话与平台签发的随机本地名 */
	req, err := http.NewRequest("GET", chacuoBaseURL+"/", nil)
	if err != nil {
		return nil, fmt.Errorf("24mail-chacuo: 创建首页请求失败: %w", err)
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("User-Agent", chacuoUA())

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("24mail-chacuo: 首页请求失败: %w", err)
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "24mail-chacuo generate 首页"); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("24mail-chacuo: 读取首页失败: %w", err)
	}

	sid := ""
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "sid" {
			sid = cookie.Value
			break
		}
	}
	if sid == "" {
		/* 备选：Set-Cookie 头可能以非常规形式返回 */
		for _, value := range resp.Header.Values("Set-Cookie") {
			for _, part := range strings.Split(value, ";") {
				part = strings.TrimSpace(part)
				if strings.HasPrefix(part, "sid=") {
					sid = strings.TrimPrefix(part, "sid=")
				}
			}
			if sid != "" {
				break
			}
		}
	}
	if sid == "" {
		return nil, fmt.Errorf("24mail-chacuo: 首页未返回 sid 会话，无法创建邮箱")
	}

	/* 沿用平台签发名，解析失败时 SDK 随机一个 */
	name := chacuoHomeName(raw)
	if name == "" {
		name = chacuoRandomLocal(10)
	}

	/* 步骤2：POST type=set 绑定名字；被占/无变化（data:[false]）时换随机名重试，最多 3 次 */
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			name = chacuoRandomLocal(10)
		}
		respBody, err := chacuoDoPost(client, sid, name, "set", "")
		if err != nil {
			lastErr = err
			continue
		}
		var setResp struct {
			Status int               `json:"status"`
			Info   string            `json:"info"`
			Data   []json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(respBody, &setResp); err != nil {
			lastErr = fmt.Errorf("24mail-chacuo: set 响应解析失败: %w", err)
			continue
		}
		if setResp.Status != 1 {
			lastErr = fmt.Errorf("24mail-chacuo: set 返回 status=%d info=%s", setResp.Status, setResp.Info)
			continue
		}
		/* 成功形如 data:[name]；失败形如 data:[false] 或 data 为空 */
		var bindName string
		if len(setResp.Data) == 0 || json.Unmarshal(setResp.Data[0], &bindName) != nil || bindName != name {
			lastErr = fmt.Errorf("24mail-chacuo: 名字 %s 绑定失败（可能已被占用），响应 %s", name, string(respBody))
			continue
		}

		tokenBytes, err := json.Marshal(chacuoToken{Sid: sid, Name: bindName})
		if err != nil {
			return nil, fmt.Errorf("24mail-chacuo: 序列化会话票证失败: %w", err)
		}
		/* 页面 select 提供 027168.com 备用域，主用 chacuo.net */
		domain := chacuoDomains[0]
		return &CreatedMailbox{
			Channel: "24mail-chacuo",
			Email:   bindName + "@" + domain,
			Token:   string(tokenBytes),
		}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("未知原因")
	}
	return nil, fmt.Errorf("24mail-chacuo: 绑定邮箱名失败（3 次尝试耗尽）: %w", lastErr)
}

/* TwentyfourmailChacuoGetEmails 获取 24mail-chacuo 邮件列表（token 为 sid+本地名 JSON） */
func TwentyfourmailChacuoGetEmails(token, email string) ([]NormEmail, error) {
	if token == "" {
		return nil, fmt.Errorf("24mail-chacuo: token 为空（缺少 sid 会话）")
	}
	var cred chacuoToken
	err := json.Unmarshal([]byte(token), &cred)
	if err != nil || cred.Sid == "" {
		if err == nil {
			err = fmt.Errorf("sid 为空")
		}
		return nil, fmt.Errorf("24mail-chacuo: token 格式无效: %w", err)
	}
	/* 本地名以票证为准（不含 @域），兜底从 email 提取 */
	name := cred.Name
	if name == "" {
		if atIdx := strings.Index(email, "@"); atIdx > 0 {
			name = email[:atIdx]
		} else {
			name = email
		}
	}

	client := HTTPClientNoCookieJar()

	/* 步骤1：type=refresh 拉取列表 */
	respBody, err := chacuoDoPost(client, cred.Sid, name, "refresh", "")
	if err != nil {
		return nil, fmt.Errorf("24mail-chacuo: 获取列表失败: %w", err)
	}
	var listResp struct {
		Status int               `json:"status"`
		Info   string            `json:"info"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(respBody, &listResp); err != nil {
		return nil, fmt.Errorf("24mail-chacuo: 列表响应解析失败: %w", err)
	}
	if listResp.Status != 1 {
		return nil, fmt.Errorf("24mail-chacuo: 列表返回 status=%d info=%s", listResp.Status, listResp.Info)
	}
	if len(listResp.Data) == 0 {
		return []NormEmail{}, nil
	}

	/* 空邮箱或会话未绑定时 data 形如 [false]，按空列表处理 */
	var inbox struct {
		List []struct {
			MID      string          `json:"MID"`
			FROM     string          `json:"FROM"`
			TO       string          `json:"TO"`
			SUBJECT  string          `json:"SUBJECT"`
			SENDTIME string          `json:"SENDTIME"`
			ISREAD   json.RawMessage `json:"ISREAD"`
		} `json:"list"`
	}
	if err := json.Unmarshal(listResp.Data[0], &inbox); err != nil {
		return []NormEmail{}, nil
	}

	emails := make([]NormEmail, 0, len(inbox.List))
	for _, item := range inbox.List {
		if item.MID == "" {
			continue
		}
		/* 步骤2：逐封 type=mailinfo 拉取正文（单独失败不影响其余邮件） */
		body := chacuoFetchBody(client, cred.Sid, name, item.MID)
		flat := map[string]interface{}{
			"id":      item.MID,
			"from":    item.FROM,
			"to":      item.TO,
			"subject": item.SUBJECT,
			"body":    body,
			"date":    item.SENDTIME,
			"isRead":  chacuoIsRead(item.ISREAD),
		}
		emails = append(emails, NormalizeMap(flat, email))
	}
	return emails, nil
}
