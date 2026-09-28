package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

/**
 * DisposableMail — https://disposablemail.app
 * 纯 REST JSON API，无需认证
 * 创建收件箱: POST https://disposablemail.app/api/inbox
 * 获取邮件: GET https://disposablemail.app/api/inbox/emails?token={token}
 * 域名: @disposablemail.dev, @mailmehere.cc
 */

const disposablemailAppAPIBase = "https://disposablemail.app/api"

/**
 * 归因注记（2026-09-28 平台级归因）：
 * 探针实测（MX 为 mail.disposablemail.app）：
 * - disposablemail.dev 域可正常收信，Sentinel 邮件经 SMTP 发送后平台 API 30 秒内可见。
 * - mailmehere.cc 域存在收信丢失（连续 2 封信均未落件；随后补发才被平台记录）。
 * - 建箱支持 {"domain": "..."} 显式指定域；服务器会随机分配域并偶发限流（HTTP 429）。
 * 根因：平台随机域名策略 + mailmehere.cc 域收信不可靠，非 SDK 客户端缺陷。
 * SDK 已修正为按邮件真实字段（fromAddress/bodyText 等）显式映射。
 */

/* disposablemailAppEmailMsg disposablemail.app 邮件对象的真实响应结构
 * 平台下发的字段与 SDK 设计文档中的映射表不同（如 from_address 实为 fromAddress），
 * 直接按真实字段解析避免字段候选策略错配 */
type disposablemailAppEmailMsg struct {
	ID          string      `json:"id"`
	FromAddress string      `json:"fromAddress"`
	FromName    string      `json:"fromName"`
	Subject     string      `json:"subject"`
	BodyText    string      `json:"bodyText"`
	BodyHTML    string      `json:"bodyHtml"`
	ReceivedAt  string      `json:"receivedAt"`
	IsRead      bool        `json:"isRead"`
	Forwarded   bool        `json:"forwarded"`
	Size        int64       `json:"size"`
	Attachments interface{} `json:"attachments"`
}

/* disposablemailAppHeaders 设置请求头 */
func disposablemailAppHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://disposablemail.app/")
	req.Header.Set("Origin", "https://disposablemail.app")
	req.Header.Set("User-Agent", getCurrentUA())
}

/*
 * DisposablemailAppGenerate 创建 disposablemail.app 临时邮箱
 * 流程:
 *   1. POST /api/inbox （body 为空 JSON {}）
 *   2. 解析响应中的 address、token
 *   3. token 直接存储 API 返回的 token 字符串
 */
func DisposablemailAppGenerate(channel ...string) (*CreatedMailbox, error) {
	req, err := http.NewRequest("POST", disposablemailAppAPIBase+"/inbox", strings.NewReader(`{"domain":"disposablemail.dev"}`))
	if err != nil {
		return nil, fmt.Errorf("disposablemail-app: 创建请求失败: %w", err)
	}
	disposablemailAppHeaders(req)

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("disposablemail-app: 请求创建收件箱失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("disposablemail-app: 读取响应失败: %w", err)
	}

	if err := CheckHTTPStatus(resp, "disposablemail-app inbox"); err != nil {
		return nil, err
	}

	/*
	 * 解析响应:
	 * {"id":"cmqzoipqq...","address":"8c802d7d6a@disposablemail.dev",
	 *  "token":"t6rGc1kHFrTCwheYDdIZnBvhdmaI3rQc_SaSJsnAMD4",
	 *  "domain":"disposablemail.dev","expiresAt":"...","createdAt":"..."}
	 */
	var result struct {
		Address   string `json:"address"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
		CreatedAt string `json:"createdAt"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("disposablemail-app: 解析响应失败: %w", err)
	}

	if result.Address == "" || result.Token == "" {
		return nil, fmt.Errorf("disposablemail-app: 创建收件箱失败, address 或 token 为空, body=%s", string(body))
	}

	ch := "disposablemail-app"
	if len(channel) > 0 && channel[0] != "" {
		ch = channel[0]
	}

	return &CreatedMailbox{
		Channel:   ch,
		Email:     result.Address,
		Token:     result.Token,
		ExpiresAt: result.ExpiresAt,
		CreatedAt: result.CreatedAt,
	}, nil
}

/*
 * DisposablemailAppGetEmails 获取 disposablemail.app 邮件列表
 * 流程:
 *   1. GET /api/inbox/emails?token={token}
 *   2. 解析响应中的 emails 数组并归一化
 */
func DisposablemailAppGetEmails(email, token string) ([]NormEmail, error) {
	email = strings.TrimSpace(email)
	if email == "" {
		return nil, fmt.Errorf("disposablemail-app: 邮箱地址为空")
	}
	if token == "" {
		return nil, fmt.Errorf("disposablemail-app: token 为空")
	}

	/*
	 * 平台对同一 URL 偶发返回陈旧空列表（SDK 压测观察：手工带 Origin 可读、
	 * SDK 同 URL n=0），附加 cachebust 强制回源；同时显式禁缓存。
	 */
	u := fmt.Sprintf("%s/inbox/emails?token=%s&cachebust=%d", disposablemailAppAPIBase, token, time.Now().UnixNano())
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, fmt.Errorf("disposablemail-app: 创建请求失败: %w", err)
	}
	disposablemailAppHeaders(req)
	req.Header.Set("Cache-Control", "no-cache")
	/* GET 请求不需要 Content-Type */
	req.Header.Del("Content-Type")

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("disposablemail-app: 请求获取邮件失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("disposablemail-app: 读取响应失败: %w", err)
	}

	if err := CheckHTTPStatus(resp, "disposablemail-app emails"); err != nil {
		return nil, err
	}

	/*
	 * 解析响应:
	 * {"emails":[],"total":0,"inbox":{"address":"...","expiresAt":"..."}}
	 * 有邮件时 emails 数组包含邮件对象
	 */
	var result struct {
		Emails []disposablemailAppEmailMsg `json:"emails"`
		Total  int                         `json:"total"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("disposablemail-app: 解析邮件列表失败: %w", err)
	}

	if len(result.Emails) == 0 {
		return []NormEmail{}, nil
	}

	/* 以邮件对象原始结构显式映射后归一化，避免依赖空类型的隐性解析 */
	emails := make([]NormEmail, 0, len(result.Emails))
	for _, msg := range result.Emails {
		flat := map[string]interface{}{
			"id":          msg.ID,
			"from":        msg.FromAddress,
			"to":          email,
			"subject":     msg.Subject,
			"text":        msg.BodyText,
			"html":        msg.BodyHTML,
			"date":        msg.ReceivedAt,
			"isRead":      msg.IsRead,
			"name":        msg.FromName,
			"attachments": msg.Attachments,
		}
		emails = append(emails, NormalizeMap(flat, email))
	}

	return emails, nil
}
