package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * LinshiXYZ 渠道实现（linshi.xyz）
 * 无建箱请求：本地随机 6 位 hex 前缀（官网 client 即用短 id）+ @linshi.xyz。
 * - 读信 GET https://linshi.xyz/api/mails/{前缀}，无邮件时返回空数组 []，
 *   有邮件时为邮件对象数组（官网 app.js 以 mail.headers.from/subject/date
 *   渲染列表，以 mail.html 渲染正文）。
 * - 预览面板 1secmail 系 API /api/?action=getMessages&login={前缀}&domain=linshi.xyz
 *   亦可用作二次兜底（元素字段 id/from/subject/date/body 均为明文）。
 *   首端点响应归一化时将 headers 平铺并注入收件人地址，若服务端返回
 *   非数组骨架（如 {ok:false}）则整体失败，交由上层 fallback 渠道处理。
 */

const linshiXYZBase = "https://linshi.xyz"
const linshiXYZDomain = "linshi.xyz"

/*
 * LinshiXYZGenerate 创建 linshi.xyz 临时邮箱
 * 无需建箱请求，本地随机 6 位 hex 前缀；token 复用完整地址。
 */
func LinshiXYZGenerate() (*CreatedMailbox, error) {
	local := linshiXYZLocalName()
	addr := local + "@" + linshiXYZDomain
	return &CreatedMailbox{
		Channel: "linshi-xyz",
		Email:   addr,
		Token:   addr,
	}, nil
}

/* linshiXYZLocalName 生成本地随机 6 位 hex 前缀（与官网 client 相同格式） */
func linshiXYZLocalName() string {
	const hexDigits = "0123456789abcdef"
	b := make([]byte, 6)
	for i := range b {
		b[i] = hexDigits[rand.Intn(len(hexDigits))]
	}
	return string(b)
}

/*
 * LinshiXYZGetEmails 读取 linshi.xyz 收件箱
 * GET /api/mails/{前缀}（URL 编码），响应为邮件对象数组。
 * 邮件元素结构：{headers:{from,to,subject,date},html}（字段缺失时以保底 map 归一化）。
 * @param email - 完整邮箱地址（前缀@linshi.xyz）
 * @param token - 复用完整地址
 */
func LinshiXYZGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("linshi-xyz: 邮箱地址无效: %q", email)
	}
	local := strings.SplitN(email, "@", 2)[0]

	req, err := http.NewRequest(http.MethodGet, linshiXYZBase+"/api/mails/"+url.PathEscape(local), nil)
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
		return nil, fmt.Errorf("linshi-xyz: 读取收件箱失败 http %d: %s", resp.StatusCode, string(body))
	}

	var list []map[string]interface{}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("linshi-xyz: 解析收件箱响应失败: %w", err)
	}

	out := make([]NormEmail, 0, len(list))
	for _, m := range list {
		out = append(out, normalizeLinshiXYZMail(m, email))
	}
	return out, nil
}

/*
 * normalizeLinshiXYZMail 将 linshi.xyz 邮件对象归一化
 * headers 对象平铺为顶层字段；嵌套对象无法被 getStr 提取，
 * 故显式展开 headers.from/to/subject/date，html 保留原样。
 */
func normalizeLinshiXYZMail(m map[string]interface{}, email string) NormEmail {
	flat := make(map[string]interface{}, len(m)+6)
	for k, v := range m {
		flat[k] = v
	}
	/* 展开嵌套 headers */
	if headers, ok := m["headers"].(map[string]interface{}); ok {
		for k, v := range headers {
			flat[k] = v
		}
	}
	/* 注入收件人地址（headers.to 存在时 normalizeTo 会优先使用） */
	if _, ok := flat["to"]; !ok {
		flat["to"] = email
	}
	return NormalizeMap(flat, email)
}
