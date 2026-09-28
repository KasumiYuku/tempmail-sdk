package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

/*
 * Shadowmail 渠道实现（shadowmail.win）
 *
 * 完整接入契约（前端 JS 抓取 + curl 实测，2026-09-27）：
 *   - 注册：POST /api/register {"email":"<随机前缀>@gmail.com","password":"Abcd1234!"}
 *     → 200 {"message":"Successfully Registered"}。平台只收 shadowmail.win 域
 *     （注册邮箱为平台外通信地址 + 账号主体）。
 *   - 登录：POST /api/login（同 body）→ 200 {"message":"Successfull Login"}
 *     并 Set-Cookie: sessionId=<uuid>（HttpOnly; Secure; Path=/; Max-Age 3600）。
 *   - 建箱：POST /api/new-address → 200 {"message":"Successfully created new email
 *     for user","address":"<id>@shadowmail.win","id":<id>}；每账号 12 个槽位。
 *   - 读信：POST /api/get-emails {"address":"<地址>"} → 200 {"message":"Emails read",
 *     "mails":[...]}；mails 元素字段 id/address_id/sender/subject/body/created_at。
 *   - 边界：地址必须是账号名下地址，否则 "Must specify an address to read emails
 *     from"；未登录各端点返回 404；云代理出口风控时 get-emails 会 429
 *     {"message":"Invalid Email Format"}（surface 端可重试）。
 *
 * 会话隔离：全域使用显式 Cookie 头（sessionId=<uuid>）逐请求携带，凭据串
 *   token 持久化注册邮箱/密码/会话 id，便于会话过期后自动重生。
 */

const (
	shadowmailBase = "https://shadowmail.win"
	// shadowmailPw 固定注册密码（平台无自选密码入口，注册即固定）
	shadowmailPw = "Abcd1234!"
	// shadowmailDomain 平台唯一收信域
	shadowmailDomain = "shadowmail.win"
	// shadowmailTokenPrefix 本渠道凭据串前缀
	shadowmailTokenPrefix = "shadowmail|"
)

/* shadowmailAccountResponse 注册/登录响应 */
type shadowmailAccountResponse struct {
	Message string `json:"message"`
}

/* shadowmailNewAddressResponse 建箱响应 */
type shadowmailNewAddressResponse struct {
	Message string `json:"message"`
	Address string `json:"address"`
	ID      int    `json:"id"`
}

/* shadowmailGetEmailsResponse 读信响应 */
type shadowmailGetEmailsResponse struct {
	Message string                   `json:"message"`
	Mails   []map[string]interface{} `json:"mails"`
}

/* shadowmailRandomAccount 生成随机注册邮箱前缀（sdk+8 位小写字母） */
func shadowmailRandomAccount() string {
	const chars = "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return "sdk" + string(b)
}

/* shadowmailDo 携带显式 Cookie 的 JSON POST 请求，返回响应体与状态码 */
func shadowmailDo(client tls_client.HttpClient, path string, body interface{}, cookie string) ([]byte, int, string, error) {
	if client == nil {
		client = HTTPClient()
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, shadowmailBase+path, bytes.NewReader(raw))
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	respRaw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, "", err
	}
	status := resp.StatusCode
	scs := resp.Header.Values("Set-Cookie")
	return respRaw, status, strings.Join(scs, "; "), nil
}

/* shadowmailSessionFromSetCookie 从 Set-Cookie 中提取 sessionId 值（纯 uuid，不含键名） */
func shadowmailSessionFromSetCookie(scs string) string {
	if scs == "" {
		return ""
	}
	for _, sc := range strings.Split(scs, "; ") {
		if strings.HasPrefix(sc, "sessionId=") {
			return strings.TrimPrefix(sc, "sessionId=")
		}
	}
	return ""
}

/* shadowmailRegisterLogin 注册或登录（POST /api/register、/api/login），返回会话 Cookie */
func shadowmailRegisterLogin(client tls_client.HttpClient, account, password string, isLogin bool) (string, error) {
	path := "/api/register"
	if isLogin {
		path = "/api/login"
	}
	body := map[string]string{"email": account, "password": password}
	raw, status, scs, err := shadowmailDo(client, path, body, "")
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("shadowmail %s: http %d", path, status)
	}
	var data shadowmailAccountResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", err
	}
	if isLogin && data.Message != "Successfull Login" {
		return "", fmt.Errorf("shadowmail login: %s", data.Message)
	}
	/* 重复注册（幂等）：消息为 Email already in use 时视为账号已存在，继续走登录 */
	if !isLogin && data.Message != "Successfully Registered" && data.Message != "Email already in use" {
		return "", fmt.Errorf("shadowmail register: %s", data.Message)
	}
	session := shadowmailSessionFromSetCookie(scs)
	if isLogin && session == "" {
		return "", fmt.Errorf("shadowmail login: 未下发 sessionId Cookie")
	}
	return session, nil
}

/*
 * ShadowmailGenerate 注册账号并创建临时邮箱地址
 * @return token 凭据串格式："<account>|<password>|<sessionId>"，读信凭此重建会话
 */
func ShadowmailGenerate() (*CreatedMailbox, error) {
	// 关键：平台会话回源校验 IP，且 Set-Cookie sessionId 会被以原样提取；
	// 同一 Cookie 罐客户端在 login 后已自动携带 sessionId（curl 实测可通）。
	// 但若无罐导致新请求不带 Cookie，会话即失效（平台返回 401 Invalid Session），
	// 故此处显式将 login 下发的 sessionId 作为 Cookie 头串透传。
	client := HTTPClientNoCookieJar()
	account := shadowmailRandomAccount() + "@gmail.com"

	// 1) 注册（幂等：已存在同名账号则跳过）
	_, err := shadowmailRegisterLogin(client, account, shadowmailPw, false)
	if err != nil {
		return nil, err
	}
	// 2) 登录取得 sessionId（纯 uuid，不合成键值对）
	session, err := shadowmailRegisterLogin(client, account, shadowmailPw, true)
	if err != nil {
		return nil, err
	}
	// 3) 创建地址（每账号 12 槽）：SDK 层显式 Cookie 头传 sessionId
	raw, status, _, err := shadowmailDo(client, "/api/new-address", map[string]interface{}{}, "sessionId="+session)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("shadowmail new-address: http %d", status)
	}
	var data shadowmailNewAddressResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.Address == "" || !strings.HasSuffix(data.Address, "@"+shadowmailDomain) {
		return nil, fmt.Errorf("shadowmail new-address: 响应缺少有效地址")
	}

	// Token 持久化：account|password|sessionId（sessionId 为 uuid，无分隔符冲突）
	token := shadowmailTokenPrefix + account + "|" + shadowmailPw + "|" + session
	return &CreatedMailbox{
		Channel: "shadowmail",
		Email:   strings.ToLower(strings.TrimSpace(data.Address)),
		Token:   token,
	}, nil
}

/* shadowmailParseToken 解析凭据串为 account/password/sessionId 三元组 */
func shadowmailParseToken(token string) (account, password, session string, err error) {
	if !strings.HasPrefix(token, shadowmailTokenPrefix) {
		return "", "", "", fmt.Errorf("shadowmail: token 格式错误")
	}
	parts := strings.Split(strings.TrimPrefix(token, shadowmailTokenPrefix), "|")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("shadowmail: token 字段缺失")
	}
	account, password, session = parts[0], parts[1], parts[2]
	if account == "" || password == "" || session == "" {
		return "", "", "", fmt.Errorf("shadowmail: token 凭据字段为空")
	}
	return
}

/*
 * ShadowmailGetEmails 读取收件箱
 * @param email 平台新地址（<id>@shadowmail.win）
 * @param token 建箱下发的凭据串
 * 会话失效时自动以凭据内 account/password 重新登录换新 sessionId。
 */
func ShadowmailGetEmails(email, token string) ([]NormEmail, error) {
	account, password, session, err := shadowmailParseToken(token)
	if err != nil {
		return nil, err
	}
	client := HTTPClientNoCookieJar()

	do := func() ([]byte, int, error) {
		body := map[string]string{"address": email}
		rawBody, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPost, shadowmailBase+"/api/get-emails", bytes.NewReader(rawBody))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", GetCurrentUA())
		req.Header.Set("Cookie", "sessionId="+session)
		resp, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, 0, err
		}
		return raw, resp.StatusCode, nil
	}

	raw, status, err := do()
	if err != nil {
		return nil, err
	}
	// sessionId 最长 1 小时（Max-Age 3600），过期后重登录重试一次
	if status == 401 || status == 404 {
		if s, lerr := shadowmailRegisterLogin(client, account, password, true); lerr == nil {
			session = s
			raw, status, err = do()
		}
	}
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("shadowmail get-emails: http %d", status)
	}
	var data shadowmailGetEmailsResponse
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.Message != "Emails read" {
		return nil, fmt.Errorf("shadowmail get-emails: %s", data.Message)
	}

	out := make([]NormEmail, 0, len(data.Mails))
	for _, m := range data.Mails {
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		flat["from"] = m["sender"]
		flat["to"] = email
		flat["date"] = m["created_at"]
		// 平台无 text/html 区分，body 为正文（默认按纯文本处理，普通化可按需互转）
		flat["text"] = m["body"]
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
