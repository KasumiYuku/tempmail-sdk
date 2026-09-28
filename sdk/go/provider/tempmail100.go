package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * TempMail100 渠道实现（tempmail100.com）
 * POST /init 建箱（空 body，响应 code/data.token（JWT），无 Cookie），
 * POST /web/generate 建随机地址（Header Authorization: <token>，响应 code/data.address，域为 uz8.net 等轮换域），
 * GET /web/emails 读信列表（Header Authorization: <token>，响应 code/data.list[]/data.total）。
 *
 * 平台限制（实测确认，非 SDK 缺陷）：
 * 列表元素字段为 {uuid, subject, fromAddress, toAddress, fromName, content, timestamp, read}，
 * 其中 content 恒为空字符串；详情端点 /detail/{uuid} 对真实 token 返回 HTTP 200 + 空 body，
 * /api/* 无邮件资源（GET /api/emails 返回 code 1002 Authorization Invalid，其余 /api 路径 404）。
 * 因此正文永久不可得，本渠道客观为「列表-only」：subject/fromAddress/fromName/timestamp/read 可
 * 正确输出，verify 的正文哨兵恒无法命中，verdict 稳定停留在 partial/no-body 属平台限制。
 * 前端 default.js 的 appendEmail 仅用 fromName/fromAddress/subject/timestamp，与上述一致。
 */

const tempmail100Base = "https://tempmail100.com"

/* tempmail100InitResponse 初始化响应 */
type tempmail100InitResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Token    string `json:"token"`
		Address  string `json:"address"`
		Redirect bool   `json:"redirect"`
	} `json:"data"`
}

/* tempmail100GenerateResponse 建箱响应 */
type tempmail100GenerateResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Address string `json:"address"`
	} `json:"data"`
}

/* tempmail100EmailsResponse 邮件列表响应 */
type tempmail100EmailsResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		List  []map[string]interface{} `json:"list"`
		Total int                      `json:"total"`
	} `json:"data"`
}

/* tempmail100AuthHeaders 设置 tempmail100 请求的通用请求头（前端使用 Authorization: <token> 不带 Bearer） */
func tempmail100AuthHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", GetCurrentUA())
	if token != "" {
		req.Header.Set("Authorization", token)
	}
}

/*
 * TempMail100Generate 创建 tempmail100.com 临时邮箱
 * 流程：POST /init 取得 JWT token，再 POST /web/generate 创建地址
 */
func TempMail100Generate() (*CreatedMailbox, error) {
	/* 第一步：初始化取得 token */
	req, err := http.NewRequest(http.MethodPost, tempmail100Base+"/init", nil)
	if err != nil {
		return nil, err
	}
	tempmail100AuthHeaders(req, "")

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
		return nil, fmt.Errorf("tempmail100: 初始化失败 http %d: %s", resp.StatusCode, string(body))
	}

	var init tempmail100InitResponse
	if err := json.Unmarshal(body, &init); err != nil {
		return nil, fmt.Errorf("tempmail100: 解析初始化响应失败: %w", err)
	}
	if init.Code != 0 || init.Data.Token == "" {
		return nil, fmt.Errorf("tempmail100: 初始化响应异常: %s", string(body))
	}

	/* 第二步：创建随机地址 */
	req2, err := http.NewRequest(http.MethodPost, tempmail100Base+"/web/generate", nil)
	if err != nil {
		return nil, err
	}
	tempmail100AuthHeaders(req2, init.Data.Token)

	resp2, err := HTTPClient().Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()

	body2, err := io.ReadAll(resp2.Body)
	if err != nil {
		return nil, err
	}
	if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
		return nil, fmt.Errorf("tempmail100: 创建地址失败 http %d: %s", resp2.StatusCode, string(body2))
	}

	var gen tempmail100GenerateResponse
	if err := json.Unmarshal(body2, &gen); err != nil {
		return nil, fmt.Errorf("tempmail100: 解析创建地址响应失败: %w", err)
	}
	if gen.Code != 0 || gen.Data.Address == "" || !strings.Contains(gen.Data.Address, "@") {
		return nil, fmt.Errorf("tempmail100: 创建地址响应异常: %s", string(body2))
	}

	return &CreatedMailbox{
		Channel: "tempmail100",
		Email:   gen.Data.Address,
		Token:   init.Data.Token,
	}, nil
}

/*
 * TempMail100GetEmails 获取 tempmail100.com 邮件列表
 * 流程：GET /web/emails（Authorization 头为裸 token）返回 code/data.list/data.total。
 * 列表元素 content 恒为空（正文端点不存在，为平台限制），如实输出空正文，不制造假内容。
 * @param email - 邮箱地址
 * @param token - 初始化返回的 JWT 认证令牌
 */
func TempMail100GetEmails(email, token string) ([]NormEmail, error) {
	req, err := http.NewRequest(http.MethodGet, tempmail100Base+"/web/emails", nil)
	if err != nil {
		return nil, err
	}
	tempmail100AuthHeaders(req, token)

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
		return nil, fmt.Errorf("tempmail100: 获取邮件列表失败 http %d: %s", resp.StatusCode, string(body))
	}

	var data tempmail100EmailsResponse
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("tempmail100: 解析邮件列表失败: %w", err)
	}
	if data.Code != 0 {
		return nil, fmt.Errorf("tempmail100: 获取邮件列表响应异常: %s", data.Message)
	}
	if data.Data.List == nil {
		return []NormEmail{}, nil
	}

	out := make([]NormEmail, 0, len(data.Data.List))
	for _, m := range data.Data.List {
		out = append(out, tempmail100NormalizeItem(m, email))
	}
	return out, nil
}

/*
 * tempmail100NormalizeItem 将 /web/emails 列表元素归一为统一邮件结构。
 * 列表元素为 {uuid, subject, fromAddress, toAddress, fromName, content, timestamp, read}，
 * content 平台恒为空（正文端点不存在），如实留空；read 为布尔已读标记。
 * fromName+fromAddress 组合为 "Name <address>" 填入 from 供 normalizeFrom 提取；
 * timestamp 为毫秒值（>1e12 时 normalizeDate 按 UnixMilli 解析）。
 */
func tempmail100NormalizeItem(item map[string]interface{}, email string) NormEmail {
	fromName := tempmail100Str(item["fromName"])
	fromAddress := tempmail100Str(item["fromAddress"])
	if fromName != "" && !strings.EqualFold(fromName, fromAddress) && strings.Contains(fromAddress, "@") {
		fromAddress = fmt.Sprintf("%s <%s>", fromName, fromAddress)
	}

	flat := map[string]interface{}{
		"id":        tempmail100Str(item["uuid"]),
		"from":      fromAddress,
		"to":        tempmail100Str(item["toAddress"]),
		"subject":   tempmail100Str(item["subject"]),
		"content":   tempmail100Str(item["content"]),
		"timestamp": item["timestamp"],
		"isRead":    tempmail100Read(item["read"]),
	}
	return NormalizeMap(flat, email)
}

/* tempmail100Str 将接口字段值安全转换为字符串，nil 或非字符串类型返回空串 */
func tempmail100Str(v interface{}) string {
	s, ok := v.(string)
	if !ok {
		if f, isNum := v.(float64); isNum {
			return strconv.FormatInt(int64(f), 10)
		}
		return ""
	}
	return s
}

/* tempmail100Read 将 read 字段归一为布尔已读标记，兼容 bool / float64(0|1) / string("true"|"1") */
func tempmail100Read(v interface{}) bool {
	switch val := v.(type) {
	case bool:
		return val
	case float64:
		return val != 0
	case string:
		return strings.EqualFold(strings.TrimSpace(val), "true") || strings.TrimSpace(val) == "1"
	default:
		return false
	}
}
