package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * Tmpkit 渠道实现（tmpkit.com，Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）
 *
 * 调研实证结论（2026-09-28 curl/tls-client/浏览器抓包实测回验）：
 *   - POST /api/rpc/tempmail/initSession，body 为 tRPC 包裹 {"json":{}}，
 *     免凭据。响应 {"json":{"session":{"sessionId","email","createdAt",
 *     "expiresAt","extendedCount"},"availableDomains":["tmpkit.com"]}}，
 *     同时 Set-Cookie: tempmail_session=<sessionId>; Max-Age=3600（与
 *     sessionId 同值）。
 *   - POST /api/rpc/tempmail/getEmails，body {"json":{"offset":0,
 *     "limit":20}}，需带 tempmail_session Cookie。响应
 *     {"json":{"emails":[...],"total":N,"session":{"email","expiresIn"}}}；
 *     不带 Cookie 时同样 200 但 emails 为空、session 为 null。列表元素
 *     键为 mailId/from/subject/excerpt/date/timestamp/hasAttach/isRead
 *     （无 id 键）。
 *   - 关键：getEmails 必须与 SDK 同款浏览器 TLS 指纹客户端
 *     （tls-client，见 config.go buildTLSClient）发起才能 200；裸 curl
 *     （Go http2 / OpenSSL）在全部 body 与头组合下均被 Cloudflare
 *     边缘 400：{"json":{"defined":false,"code":"BAD_REQUEST",...,
 *     "issues":[{"code":"invalid_type","path":[],"message":"Invalid input:
 *     expected object, received undefined"}]}}，此 400 是网关层过滤而非
 *     tRPC 语义。tls-client 下 {"json":{"offset":0,"limit":20}} 与
 *     {"json":{}} 均 200。
 *   - POST /api/rpc/tempmail/getEmailDetail，body
 *     {"json":{"mailId":<数字>}}（mailId 为数字，字符串会 zod 400）。
 *     响应为单封详情对象：mailId/from/to/subject/body/date/timestamp/
 *     contentType/sourceEmail；body 为含 <br> 的纯文本正文（哨兵
 *     MKTH-MKTM-MKTT 读回即靠此键，normalizeText 已含 body 候选）。
 *     getEmailDetail 不带会话 Cookie 时 500（浏览器带 Cookie 全程 200）。
 *
 * 会话粘性：initSession 的 Set-Cookie 先落全局 Cookie 罐；token 同时
 * 保存 sessionId（tempMailSession=<sid>），每次读信先覆写对域 Cookie，
 * 防全局罐被并行会话覆盖后串箱。
 */

/* tmpkitOrigin / tmpkitRPCPrefix 站点与 rpc 前缀 */
const (
	tmpkitOrigin    = "https://tmpkit.com"
	tmpkitRPCPrefix = tmpkitOrigin + "/api/rpc/tempmail"
)

/*
 * tmpkitRPC 对 tmpkit 发起 rpc 调用
 * body 为 tRPC 包裹 {"json":<reqBody>}，响应外层 {"json":{...}} 解析为
 * 通用 map 返回（JSON 数字自动转 float64），并按需覆写对域会话 Cookie。
 * 与前端 tRPC 客户端逐项对齐：Content-Type application/json、
 * Accept 为通配值、Referer https://tmpkit.com/en、Origin https://tmpkit.com
 * （均实测不缺亦可 200），Cookie 与 UA 由共享 HTTPClient 以构造时选取
 * 的浏览器 profile 共同呈现。
 * @param procedure - rpc 过程名（initSession / getEmails ...）
 * @param reqBody   - 过程参数（可为 nil）
 * @param cookie    - sessionId；非空时先覆写 tempmail_session Cookie
 */
func tmpkitRPC(procedure string, reqBody map[string]interface{}, cookie string) (map[string]interface{}, error) {
	payload, err := json.Marshal(map[string]interface{}{"json": reqBody})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, tmpkitRPCPrefix+"/"+procedure, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", tmpkitOrigin)
	req.Header.Set("Referer", tmpkitOrigin+"/en")

	client := HTTPClient()
	if cookie != "" {
		u, _ := url.Parse(tmpkitOrigin)
		client.SetCookies(u, []*http.Cookie{
			{Name: "tempmail_session", Value: cookie, Path: "/", HttpOnly: true, Secure: true},
		})
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tmpkit: %s 失败 http %d: %s", procedure, resp.StatusCode, string(body))
	}

	var outer struct {
		JSON map[string]interface{} `json:"json"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		return nil, fmt.Errorf("tmpkit: 解析 %s 响应失败: %w", procedure, err)
	}
	if outer.JSON == nil {
		return nil, fmt.Errorf("tmpkit: %s 响应缺 json 载荷: %s", procedure, string(body))
	}
	return outer.JSON, nil
}

/* tmpkitMapGet 从 map 容错取字符串 */
func tmpkitMapGet(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key]; ok && v != nil {
		return fmt.Sprintf("%v", v)
	}
	return ""
}

/*
 * TmpkitGenerate 创建 tmpkit.com 临时邮箱
 * 调 initSession（tRPC 包裹 {"json":{}}），sessionId 与邮箱地址同返。
 */
func TmpkitGenerate() (*CreatedMailbox, error) {
	data, err := tmpkitRPC("initSession", map[string]interface{}{}, "")
	if err != nil {
		return nil, err
	}
	sess, ok := data["session"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("tmpkit: 创建会话响应缺 session 字段")
	}
	email := tmpkitMapGet(sess, "email")
	sessionID := tmpkitMapGet(sess, "sessionId")
	if email == "" || sessionID == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("tmpkit: 创建会话响应缺少必要字段（email/sessionId）")
	}
	return &CreatedMailbox{
		Channel: "tmpkit",
		Email:   email,
		Token:   "tempMailSession=" + sessionID,
	}, nil
}

/*
 * TmpkitGetEmails 获取 tmpkit.com 邮件列表
 * getEmails（offset 0 / limit 20）取摘要，逐封 getEmailDetail 拉详情并
 * 将详情键并入摘要（键名不猜测，归一化交给 normalize 候选字段）；
 * 详情失败时回退列表摘要。getEmails 返回的 session.email 与收信邮箱
 * 不符时报错，防止会话被覆盖。
 * @param email - 邮箱地址
 * @param token - 会话凭据串（tempMailSession=<sessionId>）
 */
func TmpkitGetEmails(email, token string) ([]NormEmail, error) {
	mailbox := strings.TrimSpace(strings.TrimPrefix(token, "tempMailSession="))
	if mailbox == "" {
		return nil, fmt.Errorf("tmpkit: 会话 token 为空")
	}

	data, err := tmpkitRPC("getEmails", map[string]interface{}{"offset": 0, "limit": 20}, mailbox)
	if err != nil {
		return nil, err
	}
	/* 会话指向校验：getEmails 响应自带 session.email（无 Cookie 时
	   session 为 null，同样视为会话失效） */
	if s, ok := data["session"].(map[string]interface{}); ok {
		if got := tmpkitMapGet(s, "email"); got != "" && got != email {
			return nil, fmt.Errorf("tmpkit: 会话邮箱不匹配（响应 %s，请求 %s）", got, email)
		}
	} else {
		return nil, fmt.Errorf("tmpkit: 会话已失效（getEmails 返回空会话）")
	}

	list, ok := data["emails"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("tmpkit: 邮件列表响应缺 emails 字段")
	}

	out := make([]NormEmail, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		/* mailId 为数字：合法时逐封拉详情 */
		mailID := float64(0)
		if v, exists := m["mailId"]; exists && v != nil {
			switch val := v.(type) {
			case float64:
				mailID = val
			}
		}
		if mailID > 0 {
			if detail, derr := tmpkitRPC("getEmailDetail", map[string]interface{}{"mailId": int64(mailID)}, mailbox); derr == nil {
				for k, v := range detail {
					if k == "session" || k == "emails" || k == "total" || k == "error" {
						continue
					}
					m[k] = v
				}
			}
		}
		out = append(out, NormalizeMap(m, email))
	}
	return out, nil
}
