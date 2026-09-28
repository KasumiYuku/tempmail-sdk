package provider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

/*
 * Nukemail 渠道实现（nukemail.app）
 *
 * 完整接入契约（抓前端 JS + curl/PoW 全流程实测，2026-09-27）：
 *   - PoW：GET /api/pow/challenge?difficulty=4 → 200 {"id","challenge","difficulty"}
 *     （不传 difficulty 参数时平台默认 4，显式传参以锁定配置）；
 *     求 nonce 使 SHA-256(challenge+nonce) 十六进制前 4 位为 0
 *     （前端 solvePow 逐 nonce 自 0 递增，前缀 "0".repeat(difficulty)）。
 *   - 建箱：POST /api/inbox/create body {"address":<随机名>,"domain":<域名>,
 *     "pow_id","pow_nonce"} → 200 {"token":"NUKE-xxxxxxxx","email":"名@域名"},
 *     并 Set-Cookie: nukemail_token=<token>（Secure; HttpOnly; SameSite=lax, 72h）。
 *   - 读信：GET /api/inbox（Cookie: nukemail_token=<token>）→ 200
 *     {"token","state","addresses":[{"address","domain",...}],"messages":[...],
 *      "is_premium",...}；messages 元素字段为 sender、sender_name、subject、
 *     body_html、body_text、received_at、read（前端渲染实证，sendBeacon 已读回执）。
 *   - 会话恢复：POST /api/inbox/resume {"accessCode":<token>} 重设 Cookie，仅作兜底。
 *   - 域名：GET /api/domains 返回活跃域名池（is_premium=false 的多于 1 个）。
 *
 * 会话隔离：与 noxen 一致使用 HTTPClientNoCookieJar，nukemail_token 由生成
 *   结果持久化为 token，读信时以显式 Cookie 头逐请求携带。
 */

const nukemailBase = "https://nukemail.app"

/* nukemailPoWChallenge PoW 挑战响应 */
type nukemailPoWChallenge struct {
	ID         string `json:"id"`
	Challenge  string `json:"challenge"`
	Difficulty int    `json:"difficulty"`
}

/* nukemailCreateResponse 建箱响应 */
type nukemailCreateResponse struct {
	Token   string   `json:"token"`
	Email   string   `json:"email"`
	Error   string   `json:"error"`
	Suggest []string `json:"suggestions"`
}

/* nukemailInboxResponse 读信响应 */
type nukemailInboxResponse struct {
	Token     string                   `json:"token"`
	State     string                   `json:"state"`
	Addresses []map[string]interface{} `json:"addresses"`
	Messages  []map[string]interface{} `json:"messages"`
	IsPremium bool                     `json:"is_premium"`
}

/*
 * nukemailSolvePow 求解 PoW：返回使 SHA-256(challenge+nonce) 十六进制
 * 前 difficulty 位为 0 的最小 nonce。算法与前端的 solvePow 逐字对齐
 * （TextEncoder + crypto.subtle.digest，nonce 自 0 递增，无随机起点）。
 * @param challenge 挑战串
 * @param difficulty 难度（十六进制前缀 0 位数）
 * @return nonce、尝试次数、耗时
 */
func nukemailSolvePow(challenge string, difficulty int) (int64, int64, time.Duration) {
	prefix := strings.Repeat("0", difficulty)
	start := time.Now()
	var nonce int64
	hash := sha256.New()
	for nonce = 0; ; nonce++ {
		hash.Reset()
		fmt.Fprintf(hash, "%s%d", challenge, nonce)
		if strings.HasPrefix(hex.EncodeToString(hash.Sum(nil)), prefix) {
			break
		}
	}
	return nonce, nonce + 1, time.Since(start)
}

/* nukemailRandomAddress 生成本地随机名（与前端 generateRandomName 等价形态，词库不完整亦可） */
func nukemailRandomAddress() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 10)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return "nuke" + string(b)
}

/* nukemailDomain 取第一个非 premium 的活跃域名 */
func nukemailDomain() (string, error) {
	req, err := fhttp.NewRequest("GET", nukemailBase+"/api/domains", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	var data struct {
		Domains []struct {
			Domain        string `json:"domain"`
			IsPremiumOnly bool   `json:"is_premium_only"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", err
	}
	for _, d := range data.Domains {
		if !d.IsPremiumOnly && d.Domain != "" {
			return d.Domain, nil
		}
	}
	return "", fmt.Errorf("nukemail generate: 无可用非 premium 域名")
}

/*
 * NukemailGenerate 创建临时邮箱（PoW 建箱）
 * token 为平台返回的 NUKE-<随机> 访问码，读信时转成 nukemail_token Cookie 携带。
 */
func NukemailGenerate() (*CreatedMailbox, error) {
	client := HTTPClientNoCookieJar()

	// 1) 取 PoW 挑战
	req, err := fhttp.NewRequest("GET", nukemailBase+"/api/pow/challenge?difficulty=4", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
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
		return nil, fmt.Errorf("nukemail generate: challenge http %d", resp.StatusCode)
	}
	var ch nukemailPoWChallenge
	if err := json.Unmarshal(raw, &ch); err != nil {
		return nil, err
	}
	if ch.ID == "" || ch.Challenge == "" {
		return nil, fmt.Errorf("nukemail generate: challenge 响应缺少 id/challenge")
	}
	if ch.Difficulty <= 0 {
		ch.Difficulty = 4
	}

	// 2) 本地求 PoW 解（Go 原生 crypto/sha256，实测约 1.4M hash/s，难度 4 通常 < 1s）
	nonce, _, _ := nukemailSolvePow(ch.Challenge, ch.Difficulty)

	// 3) 取域名并建箱
	dom, err := nukemailDomain()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]interface{}{
		"address":   nukemailRandomAddress(),
		"domain":    dom,
		"pow_id":    ch.ID,
		"pow_nonce": fmt.Sprintf("%d", nonce),
	})
	req2, err := fhttp.NewRequest("POST", nukemailBase+"/api/inbox/create", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json")
	req2.Header.Set("User-Agent", GetCurrentUA())
	resp2, err := client.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	raw2, err := io.ReadAll(resp2.Body)
	if err != nil {
		return nil, err
	}
	if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
		return nil, fmt.Errorf("nukemail generate: create http %d: %s", resp2.StatusCode, strings.TrimSpace(string(raw2)))
	}
	var data nukemailCreateResponse
	if err := json.Unmarshal(raw2, &data); err != nil {
		return nil, err
	}
	if data.Token == "" || data.Email == "" {
		return nil, fmt.Errorf("nukemail generate: create 响应缺少 token/email")
	}

	return &CreatedMailbox{
		Channel: "nukemail",
		Email:   data.Email,
		Token:   data.Token,
	}, nil
}

/*
 * NukemailGetEmails 读取收件箱
 * @param token 建箱返回的 NUKE-<随机> 访问码
 * 主通道 GET /api/inbox 带 Cookie: nukemail_token=<token>；
 * 若 token 会话已失效则经 POST /api/inbox/resume 显式重设会话后重试一次。
 */
func NukemailGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("nukemail: token 为空")
	}
	client := HTTPClientNoCookieJar()
	cookie := "nukemail_token=" + token

	do := func() (*nukemailInboxResponse, error) {
		req, err := fhttp.NewRequest("GET", nukemailBase+"/api/inbox", nil)
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
			return nil, fmt.Errorf("nukemail 读信: http %d", resp.StatusCode)
		}
		var data nukemailInboxResponse
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}
		return &data, nil
	}

	data, err := do()
	if err != nil || data.State == "expired" || data.State == "" {
		// 会话可能已过期：经 resume 重设会话后重试
		resumeBody, _ := json.Marshal(map[string]string{"accessCode": token})
		req, err := fhttp.NewRequest("POST", nukemailBase+"/api/inbox/resume", bytes.NewReader(resumeBody))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", GetCurrentUA())
			if resp2, err2 := client.Do(req); err2 == nil {
				io.Copy(io.Discard, resp2.Body)
				resp2.Body.Close()
			}
		}
		data, err = do()
	}
	if err != nil {
		return nil, err
	}

	out := make([]NormEmail, 0, len(data.Messages))
	for _, m := range data.Messages {
		flat := map[string]interface{}{}
		for k, v := range m {
			flat[k] = v
		}
		flat["to"] = email
		// 平台消息字段为 body_html/body_text，与 NormalizeMap 候选对齐：
		// 将 body_text 补到 text 候选、body_html 补到 html 候选（若原字段缺失）
		if flat["text"] == nil && flat["body_text"] != nil {
			flat["text"] = flat["body_text"]
		}
		if flat["html"] == nil && flat["body_html"] != nil {
			flat["html"] = flat["body_html"]
		}
		flat["date"] = m["received_at"]
		flat["read"] = m["read"]
		// sender 是小写发件人地址，sender_name 是展示名（NormalizeMap 用 sender_email/sender 提取）
		flat["sender_email"] = m["sender"]
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
