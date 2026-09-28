package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"

	http "github.com/bogdanfinn/fhttp"
)

/**
 * BestTempMail -- https://best-temp-mail.com
 * 纯 JSON REST API，无需认证，无需 Session
 * 创建邮箱: POST /api/v3/createEmail  body: {"intToken":"<uuid>"}
 * 获取邮件: POST /api/v3/getEmailList  body: {"address":"...","id":"...","intToken":"...","update_tag":"..."}
 *
 * 平台级归因（2026-09-28 探针实测）：
 * - v1 面（POST api.best-tempmail.com/v1/inboxes 建箱、GET /v1/inboxes/{address}/messages 读信）
 *   返回 "Daily free inbox limit reached"（免费额度 3/天，真实限流），不可用于 SDK 无状态建箱；
 *   v3 面（best-temp-mail.com/api/v3）建箱+收发链路全通（MX mail.aabkmail.com 由 Cloudflare
 *   Email Routing 代理，SMTP 直投后平台 API 30 秒内可见），改道 v1 无必要。
 * - v3 响应真实字段为 "emailList"（含 from_address/text/send_time_by_sender），
 *   而非文档所述 "emails" 数组；必须按 emailList 解析并按平台真实字段归一化。
 * - update_tag 是服务端读游标：首读需携带建箱返回的初始值；服务端每次响应下发
 *   新游标。SDK 在 GetEmails 后把新游标并回 token 持久化，避免重复读到旧快照。
 */

const (
	bestTempMailAPIBase = "https://best-temp-mail.com/api/v3"
	/* bestTempMailTokenLen 服务端令牌均为 UUID，180 字符上限足够且防御异常 token */
	bestTempMailTokenLen = 180
)

/* bestTempMailToken 存储在 token 字段中的 JSON 结构
 * UpdateTag 由服务端随每次 getEmailList 响应轮换，读信后由 SDK 并回 token */
type bestTempMailToken struct {
	IntToken  string `json:"intToken"`
	ID        string `json:"id"`
	UpdateTag string `json:"update_tag"`
}

/* bestTempMailAuthMeta 特例鉴权元数据：EmailInfo.token 对 provider 层不可见，
 * 同一进程内以 email 为键在包内存中寄存服务端最新 update_tag，
 * 与 token 字符串互补，避免多进程/多实例场景 token 滞后丢失新游标 */
type bestTempMailAuthMeta struct {
	UpdateTag string
}

var (
	/* bestTempMailAuthMu 保护 bestTempMailAuth 并发读写 */
	bestTempMailAuthMu sync.Mutex
	/* bestTempMailAuth 以邮箱地址为键的进程内鉴权元数据存储 */
	bestTempMailAuth = make(map[string]*bestTempMailAuthMeta)
)

/* bestTempMailGenUUID 生成 UUID v4 格式字符串 */
func bestTempMailGenUUID() string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		rand.Uint32(),
		rand.Uint32()&0xffff,
		(rand.Uint32()&0x0fff)|0x4000,
		(rand.Uint32()&0x3fff)|0x8000,
		rand.Uint64()&0xffffffffffff,
	)
}

/* bestTempMailHeaders 设置请求头 */
func bestTempMailHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://best-temp-mail.com/")
	req.Header.Set("Origin", "https://best-temp-mail.com")
	req.Header.Set("User-Agent", getCurrentUA())
}

/*
 * BestTempMailGenerate 创建 best-temp-mail.com 临时邮箱
 * 流程:
 *   1. 客户端生成 UUID 作为 intToken
 *   2. POST /api/v3/createEmail {"intToken":"<uuid>"}
 *   3. 解析响应中的 address、id、update_tag
 *   4. 将 intToken + id + update_tag 序列化为 JSON 存入 token
 */
func BestTempMailGenerate(channel ...string) (*CreatedMailbox, error) {
	intToken := bestTempMailGenUUID()

	/* 构造请求体 */
	reqBody, err := json.Marshal(map[string]string{"intToken": intToken})
	if err != nil {
		return nil, fmt.Errorf("best-temp-mail: 序列化请求体失败: %w", err)
	}

	req, err := http.NewRequest("POST", bestTempMailAPIBase+"/createEmail", strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, fmt.Errorf("best-temp-mail: 创建请求失败: %w", err)
	}
	bestTempMailHeaders(req)

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("best-temp-mail: 请求创建邮箱失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("best-temp-mail: 读取响应失败: %w", err)
	}

	if err := CheckHTTPStatus(resp, "best-temp-mail createEmail"); err != nil {
		return nil, err
	}

	/* 解析响应: {"data":{"address":"...","id":"...","update_tag":"..."},"status":"success","t":1} */
	var result struct {
		Status string `json:"status"`
		Data   struct {
			Address   string `json:"address"`
			ID        string `json:"id"`
			UpdateTag string `json:"update_tag"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("best-temp-mail: 解析响应失败: %w", err)
	}

	if result.Status != "success" || result.Data.Address == "" {
		return nil, fmt.Errorf("best-temp-mail: 创建邮箱失败, status=%s, body=%s", result.Status, string(body))
	}

	/* 序列化 token */
	tokenData := bestTempMailToken{
		IntToken:  intToken,
		ID:        result.Data.ID,
		UpdateTag: result.Data.UpdateTag,
	}
	tokenJSON, err := json.Marshal(tokenData)
	if err != nil {
		return nil, fmt.Errorf("best-temp-mail: 序列化 token 失败: %w", err)
	}

	ch := "best-temp-mail"
	if len(channel) > 0 && channel[0] != "" {
		ch = channel[0]
	}

	/* 把初始游标寄存在进程内元数据，供 GetEmails 并回最新游标 */
	bestTempMailSaveAuth(result.Data.Address, result.Data.UpdateTag)

	return &CreatedMailbox{
		Channel: ch,
		Email:   result.Data.Address,
		Token:   string(tokenJSON),
	}, nil
}

/*
 * bestTempMailSaveAuth 持久化（进程内）某邮箱的服务端最新 update_tag 与 token JSON
 * 游标由服务端在每次 getEmailList 响应中下发最新值，SDK 读信后调用本函数更新
 */
func bestTempMailSaveAuth(email, updateTag string) {
	bestTempMailAuthMu.Lock()
	defer bestTempMailAuthMu.Unlock()
	bestTempMailAuth[email] = &bestTempMailAuthMeta{UpdateTag: updateTag}
}

/*
 * bestTempMailLatestUpdateTag 取某邮箱当前可知的最新 update_tag
 * 进程内元数据由每次 getEmailList 响应更新，是热游标，优先使用；
 * token 中解析出的游标仅作冷启动回退（进程重启、跨进程场景）
 */
func bestTempMailLatestUpdateTag(email, token string) string {
	bestTempMailAuthMu.Lock()
	meta := bestTempMailAuth[email]
	bestTempMailAuthMu.Unlock()
	if meta != nil && meta.UpdateTag != "" {
		return meta.UpdateTag
	}

	t := bestTempMailToken{}
	if len(token) > bestTempMailTokenLen {
		return ""
	}
	if err := json.Unmarshal([]byte(token), &t); err != nil {
		return ""
	}
	return t.UpdateTag
}

/*
 * bestTempMailParseToken 解析 token JSON；长度异常时返回错误
 */
func bestTempMailParseToken(token string) (bestTempMailToken, error) {
	t := bestTempMailToken{}
	if len(token) == 0 || len(token) > bestTempMailTokenLen {
		return t, fmt.Errorf("best-temp-mail: token 长度异常")
	}
	if err := json.Unmarshal([]byte(token), &t); err != nil {
		return t, fmt.Errorf("best-temp-mail: 解析 token 失败: %w", err)
	}
	return t, nil
}

/*
 * BestTempMailGetEmails 获取 best-temp-mail.com 邮件列表
 * 流程:
 *   1. 从 token JSON 中解析出 intToken、id；按进程内热游标优先、token 冷游标回退取 update_tag
 *   2. POST /api/v3/getEmailList {"address":"...","id":"...","intToken":"...","update_tag":"..."}
 *   3. 按平台真实字段 emailList[]（from_address/subject/text/send_time_by_sender）归一化
 *   4. 把服务端下发的新 update_tag 存回进程内元数据（并回 hot path）防重放旧快照
 */
func BestTempMailGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("best-temp-mail: 邮箱地址为空")
	}

	/* 解析 token（intToken/id 无状态变化；update_tag 走热游标优先策略） */
	tkn, err := bestTempMailParseToken(token)
	if err != nil {
		return nil, err
	}

	cursor := bestTempMailLatestUpdateTag(email, token)

	/* 构造请求体（update_tag 使用当前基线游标） */
	reqBody, err := json.Marshal(map[string]string{
		"address":    email,
		"id":         tkn.ID,
		"intToken":   tkn.IntToken,
		"update_tag": cursor,
	})
	if err != nil {
		return nil, fmt.Errorf("best-temp-mail: 序列化请求体失败: %w", err)
	}

	req, err := http.NewRequest("POST", bestTempMailAPIBase+"/getEmailList", strings.NewReader(string(reqBody)))
	if err != nil {
		return nil, fmt.Errorf("best-temp-mail: 创建请求失败: %w", err)
	}
	bestTempMailHeaders(req)

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("best-temp-mail: 请求获取邮件失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("best-temp-mail: 读取响应失败: %w", err)
	}

	if err := CheckHTTPStatus(resp, "best-temp-mail getEmailList"); err != nil {
		return nil, err
	}

	/* 解析响应（平台真实字段: data.emailList[]，非文档所述 data.emails[]）:
	 * {"data":{"hasNewEmail":false},"status":"success","t":0}
	 * {"data":{"emailList":[{"from_address":"...","subject":"...","text":"...","send_time_by_sender":<毫秒>}],
	 *   "hasNewEmail":true,"update_tag":"<新游标>"},"status":"success","t":N}
	 */
	var result struct {
		Status string `json:"status"`
		Data   struct {
			HasNewEmail bool                     `json:"hasNewEmail"`
			EmailList   []map[string]interface{} `json:"emailList"`
			UpdateTag   string                   `json:"update_tag"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("best-temp-mail: 解析邮件列表失败: %w", err)
	}

	if !result.Data.HasNewEmail || len(result.Data.EmailList) == 0 {
		/* 无新邮件：不改变游标（空结果响应不含 update_tag） */
		return []NormEmail{}, nil
	}

	/* 服务端随响应轮换游标，并回进程内元数据 */
	if result.Data.UpdateTag != "" {
		bestTempMailSaveAuth(email, result.Data.UpdateTag)
	}

	/* 真实字段映射: from_address/text/send_time_by_sender */
	emails := make([]NormEmail, 0, len(result.Data.EmailList))
	for _, m := range result.Data.EmailList {
		flat := map[string]interface{}{
			"from":    m["from_address"],
			"subject": m["subject"],
			"text":    m["text"],
			"html":    m["html"],
			"date":    m["send_time_by_sender"],
		}
		emails = append(emails, NormalizeMap(flat, email))
	}

	return emails, nil
}
