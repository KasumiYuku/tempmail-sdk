/*
 * 10minutemail.one：SSR __NUXT_DATA__ 中的 mailServiceToken（JWT）+ SSR 状态 $semailDomains（活跃域）；
 * 本地随机用户名与活跃域组合成地址；收信 GET web API /mailbox/{email}
 */
package provider

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"

	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

const (
	tenminuteSiteOrigin = "https://10minutemail.one"
	tenminuteAPIBase    = "https://web.10minutemail.one/api/v1"
)

var (
	tenminuteNuxtDataRe   = regexp.MustCompile(`(?is)<script[^>]*\bid="__NUXT_DATA__"[^>]*>([\s\S]*?)</script>`)
	tenminuteJWTRe        = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)
	tenminuteDomainRe     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*\.[a-zA-Z]{2,}$`)
	tenminuteKnownDomains = []string{"dbwot.com", "ygwpr.com", "imxwe.com"}
)

func tenminuteRandHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func tenminuteEncMailboxEmail(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return url.QueryEscape(email)
	}
	return url.PathEscape(parts[0]) + "%40" + url.PathEscape(parts[1])
}

func tenminuteParseNuxtDataArray(html string) ([]interface{}, error) {
	m := tenminuteNuxtDataRe.FindStringSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("10minute-one: __NUXT_DATA__ not found")
	}
	var arr []interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func tenminuteResolveRef(arr []interface{}, value interface{}, depth int) interface{} {
	if depth > 64 {
		return nil
	}
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case float64:
		if v == float64(int64(v)) && v >= 0 {
			idx := int(v)
			if idx < len(arr) {
				return tenminuteResolveRef(arr, arr[idx], depth+1)
			}
		}
		return value
	default:
		return value
	}
}

func tenminuteParseMailServiceToken(arr []interface{}) (string, error) {
	n := len(arr)
	if n > 48 {
		n = 48
	}
	for i := 0; i < n; i++ {
		el := arr[i]
		m, ok := el.(map[string]interface{})
		if !ok {
			continue
		}
		if ref, ok := m["mailServiceToken"]; ok {
			t := tenminuteResolveRef(arr, ref, 0)
			if s, ok := t.(string); ok && tenminuteJWTRe.MatchString(s) {
				return s, nil
			}
		}
	}
	for _, el := range arr {
		m, ok := el.(map[string]interface{})
		if !ok {
			continue
		}
		if ref, ok := m["mailServiceToken"]; ok {
			t := tenminuteResolveRef(arr, ref, 0)
			if s, ok := t.(string); ok && tenminuteJWTRe.MatchString(s) {
				return s, nil
			}
		}
	}
	for _, el := range arr {
		if s, ok := el.(string); ok && tenminuteJWTRe.MatchString(s) {
			return s, nil
		}
	}
	return "", fmt.Errorf("10minute-one: mailServiceToken not found")
}

func tenminuteParseQuotedJSONArray(html, field string) []string {
	key := field + `:"`
	start := strings.Index(html, key)
	if start < 0 {
		return nil
	}
	sliceStart := start + len(key)
	if sliceStart >= len(html) || html[sliceStart] != '[' {
		return nil
	}
	depth := 0
	j := sliceStart
	for ; j < len(html); j++ {
		c := html[j]
		if c == '[' {
			depth++
		} else if c == ']' {
			depth--
			if depth == 0 {
				j++
				break
			}
		}
	}
	raw := html[sliceStart:j]
	unesc := strings.ReplaceAll(raw, `\"`, `"`)
	var out []string
	if err := json.Unmarshal([]byte(unesc), &out); err != nil {
		return nil
	}
	return out
}

/*
 * tenminuteParseSSRActiveDomains 从 __NUXT_DATA__ 数组解析 SSR 状态中的 $semailDomains（当前活跃域）。
 * 页面公开配置 emailDomains 已与平台实际收信域脱钩，必须改读 SSR 状态：
 * 状态 map 形如 {"$semailDomains": 11}，值是指向域名引用数组（如 [12,13,14]）的下标，
 * 数组元素再由下标解析出字符串域名（当前为 dbwot.com/ygwpr.com/imxwe.com）。
 * 保留值直接为字符串列表或单域名的兼容分支。
 */
func tenminuteParseSSRActiveDomains(arr []interface{}) []string {
	// 形如 {"$semailDomains": n}：n 指向域名引用数组（逐元素索引）或域名列表
	for _, el := range arr {
		m, ok := el.(map[string]interface{})
		if !ok {
			continue
		}
		ref, ok := m["$semailDomains"]
		if !ok {
			continue
		}
		rv := tenminuteResolveRef(arr, ref, 0)
		switch list := rv.(type) {
		case []interface{}:
			var out []string
			for _, e := range list {
				s, ok := tenminuteResolveRef(arr, e, 0).(string)
				if !ok || !tenminuteDomainRe.MatchString(s) {
					continue
				}
				out = append(out, s)
			}
			if len(out) > 0 {
				return out
			}
		case string:
			if tenminuteDomainRe.MatchString(list) {
				return []string{list}
			}
		}
	}
	// 无状态 map 包裹的扁平形式 ["$semailDomains", n, "域1", "域2", ...]
	for i := 0; i+1 < len(arr); i++ {
		key, ok := arr[i].(string)
		if !ok || key != "$semailDomains" {
			continue
		}
		ref, ok := arr[i+1].(float64)
		if !ok || ref != float64(int64(ref)) || ref < 0 || int(ref) >= len(arr) {
			continue
		}
		start := int(ref)
		var out []string
		for _, el := range arr[start:] {
			s, ok := el.(string)
			if !ok {
				break
			}
			if !tenminuteDomainRe.MatchString(s) {
				break
			}
			out = append(out, s)
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func tenminutePickLocale(domain *string) string {
	if domain == nil {
		return "zh"
	}
	s := strings.TrimSpace(*domain)
	if s == "" {
		return "zh"
	}
	if strings.Contains(s, ".") && !strings.Contains(s, "/") {
		return "zh"
	}
	return s
}

func tenminutePickMailboxDomain(hint *string, available []string) (string, error) {
	if len(available) == 0 {
		return "", fmt.Errorf("10minute-one: no email domains")
	}
	if hint != nil {
		h := strings.ToLower(strings.TrimSpace(*hint))
		if strings.Contains(h, ".") {
			for _, d := range available {
				if strings.EqualFold(d, h) {
					return d, nil
				}
			}
		}
	}
	return available[randIntn(len(available))], nil
}

func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func tenminuteRandomLocal(n int) (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		x, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			return "", err
		}
		b[i] = chars[x.Int64()]
	}
	return string(b), nil
}

func tenminuteJWTExpUnix(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		pad := parts[1]
		if m := len(pad) % 4; m != 0 {
			pad += strings.Repeat("=", 4-m)
		}
		pad = strings.ReplaceAll(strings.ReplaceAll(pad, "-", "+"), "_", "/")
		raw, err = base64.StdEncoding.DecodeString(pad)
		if err != nil {
			return 0
		}
	}
	var pl struct {
		Exp float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &pl) != nil || pl.Exp <= 0 {
		return 0
	}
	return int64(pl.Exp)
}

func tenminuteAPIHeaders(token string) http.Header {
	rid, _ := tenminuteRandHex(16)
	h := make(http.Header)
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Authorization", "Bearer "+token)
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Type", "application/json")
	h.Set("DNT", "1")
	h.Set("Origin", tenminuteSiteOrigin)
	h.Set("Pragma", "no-cache")
	h.Set("Referer", tenminuteSiteOrigin+"/")
	h.Set("User-Agent", GetCurrentUA())
	h.Set("X-Request-ID", rid)
	h.Set("X-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	return h
}

/*
 * tenminuteItemNeedsDetail 判定列表项是否必须再拉单封详情：
 * 列表接口只返回元数据（id/from/to/subject/date/seen/size），不含任何正文字段，
 * 因此有 id 的邮件一律需要详情二拉。不再基于 text/body/html 等正交键判空，
 * 避免因「列表没有正文键」而误判为无需详情、导致归一化后正文恒为空。
 */
func tenminuteItemNeedsDetail(m map[string]interface{}) bool {
	return m["id"] != nil
}

// TenminuteOneGenerate 拉取 SSR 页面，解析 JWT 与域名列表，生成随机邮箱地址
func TenminuteOneGenerate(domain *string) (*CreatedMailbox, error) {
	loc := tenminutePickLocale(domain)
	pageURL := fmt.Sprintf("%s/%s", tenminuteSiteOrigin, url.PathEscape(loc))
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", GetCurrentUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("DNT", "1")
	req.Header.Set("Referer", pageURL)

	client := HTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "10minute-one page"); err != nil {
		return nil, err
	}
	htmlBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	html := string(htmlBytes)

	arr, err := tenminuteParseNuxtDataArray(html)
	if err != nil {
		return nil, err
	}
	token, err := tenminuteParseMailServiceToken(arr)
	if err != nil {
		return nil, err
	}

	// 优先使用 SSR 状态里的活跃域（$semailDomains），页面公开配置 emailDomains 已弃收
	var domains []string
	if ssr := tenminuteParseSSRActiveDomains(arr); len(ssr) > 0 {
		domains = ssr
	}
	if len(domains) == 0 {
		domains = tenminuteKnownDomains
	}

	blocked := make(map[string]struct{})
	for _, u := range tenminuteParseQuotedJSONArray(html, "blockedUsernames") {
		blocked[strings.ToLower(strings.TrimSpace(u))] = struct{}{}
	}

	var domHint *string
	if domain != nil {
		ds := strings.TrimSpace(*domain)
		if strings.Contains(ds, ".") {
			domHint = &ds
		}
	}
	mailDomain, err := tenminutePickMailboxDomain(domHint, domains)
	if err != nil {
		return nil, err
	}

	var local string
	for attempt := 0; attempt < 32; attempt++ {
		cand, err := tenminuteRandomLocal(10)
		if err != nil {
			return nil, err
		}
		if _, bad := blocked[strings.ToLower(cand)]; !bad {
			local = cand
			break
		}
	}
	if local == "" {
		return nil, fmt.Errorf("10minute-one: could not pick username")
	}

	address := fmt.Sprintf("%s@%s", local, mailDomain)
	mb := &CreatedMailbox{Channel: "10minute-one", Email: address, Token: token}
	if exp := tenminuteJWTExpUnix(token); exp > 0 {
		mb.ExpiresAt = exp
	}
	return mb, nil
}

// TenminuteOneGetEmails GET /mailbox/{email}，必要时再拉取单封详情
func TenminuteOneGetEmails(email, token string) ([]NormEmail, error) {
	if email == "" || token == "" {
		return nil, fmt.Errorf("10minute-one: email and token required")
	}
	u := fmt.Sprintf("%s/mailbox/%s", tenminuteAPIBase, tenminuteEncMailboxEmail(email))
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header = tenminuteAPIHeaders(token)

	client := HTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "10minute-one inbox"); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var list []map[string]interface{}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("10minute-one: invalid inbox JSON")
	}

	out := make([]NormEmail, 0, len(list))
	for _, row := range list {
		if row == nil {
			continue
		}
		m := row
		if tenminuteItemNeedsDetail(m) {
			mid := url.PathEscape(fmt.Sprint(m["id"]))
			du := fmt.Sprintf("%s/mailbox/%s/%s", tenminuteAPIBase, tenminuteEncMailboxEmail(email), mid)
			req2, err := http.NewRequest("GET", du, nil)
			if err == nil {
				req2.Header = tenminuteAPIHeaders(token)
				if resp2, err := client.Do(req2); err == nil {
					func() {
						defer resp2.Body.Close()
						if resp2.StatusCode >= 200 && resp2.StatusCode < 300 {
							if b, err := io.ReadAll(resp2.Body); err == nil {
								var detail map[string]interface{}
								if json.Unmarshal(b, &detail) == nil {
									for k, v := range detail {
										if _, ok := m[k]; !ok {
											m[k] = v
										}
									}
									// 详情接口正文位于嵌套 body.text/body.html：无条件提升覆盖，
									// 列表仅含元数据（id/from/to/subject/date/seen），不存在正文键冲突。
									if body2, ok := detail["body"].(map[string]interface{}); ok {
										if tv, ok := body2["text"].(string); ok && tv != "" {
											m["text"] = tv
										}
										if hv, ok := body2["html"].(string); ok && hv != "" {
											m["html"] = hv
										}
									} else if tb, ok := detail["textBody"].(string); ok && tb != "" {
										// textBody → text 兜底（防御性，平台漂移时保底）
										m["text"] = tb
									}
								}
							}
						}
					}()
				}
			}
		}
		out = append(out, NormalizeMap(m, email))
	}
	return out, nil
}
