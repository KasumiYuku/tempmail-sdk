package provider

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math/rand"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

// nimail.cn：简单 POST API 临时邮箱，无需认证
// 收信管道基于 OpenTrashMail 后端：列表接口 /api/getmails 只返回元数据（from/subject/
// 附件 URL 等），正文需对每封邮件再 GET /api/raw-html/<邮箱>/<id> 二拉。

const nimailBaseURL = "https://www.nimail.cn"

// nimailRawHTMLInjection 是平台对纯文本（无 HTML 附件部分）邮件的 raw-html 响应：
// 返回页仅包含百度统计注入脚本、不含任何正文。若二拉响应是「正文 + 该脚本」的形态，
// 脚本位于正文末尾，正则 \s*$ 仅匹配尾部空白，不会误删正文本身。
var nimailRawHTMLInjection = strings.TrimSpace(`
<script>
var _hmt = _hmt || [];
(function() {
  var hm = document.createElement("script");
  hm.src = "https://hm.baidu.com/hm.js?474bbfaaa6cec5667832d09cbd9cb961";
  var s = document.getElementsByTagName("script")[0];
  s.parentNode.insertBefore(hm, s);
})();
</script>`)

func nimailRandomLocal(length int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, length)
	for i := range buf {
		buf[i] = chars[rand.Intn(len(chars))]
	}
	return string(buf)
}

// NimailGenerate 创建 nimail.cn 临时邮箱
func NimailGenerate() (*CreatedMailbox, error) {
	client := HTTPClient()
	name := nimailRandomLocal(10)
	email := fmt.Sprintf("%s@nimail.cn", name)

	body := fmt.Sprintf("mail=%s", url.QueryEscape(email))
	req, err := http.NewRequest("POST", nimailBaseURL+"/api/applymail", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", getCurrentUA())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", nimailBaseURL)
	req.Header.Set("Referer", nimailBaseURL+"/")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "nimail generate"); err != nil {
		return nil, err
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data struct {
		Success string `json:"success"`
		User    string `json:"user"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("nimail: 返回非 JSON: %s", string(raw[:min(len(raw), 120)]))
	}
	if data.Success != "true" || data.User == "" {
		return nil, fmt.Errorf("nimail: 创建邮箱失败 %s", string(raw))
	}

	return &CreatedMailbox{
		Channel: "nimail",
		Email:   data.User,
		Token:   data.User,
	}, nil
}

// NimailGetEmails 获取 nimail.cn 邮件列表并逐封二拉正文
// 流程：POST /api/getmails 取列表 → 对每封 GET /api/raw-html/<邮箱>/<id> 拉 HTML 正文。
// from 字段为 HTML 转义（如 &lt;supper@openel.top&gt;），需反转义；
// attachments 为字符串数组（附件下载 URL），需转成标准附件结构；平台附件下载端点
// 因服务端 PHP 缺少 mime_content_type 函数而报错，附件 URL 照实透传并保留 raw 字段，
// 便于未来平台修复后直接可用。
func NimailGetEmails(email string) ([]NormEmail, error) {
	client := HTTPClient()

	body := fmt.Sprintf("mail=%s&time=0", url.QueryEscape(email))
	req, err := http.NewRequest("POST", nimailBaseURL+"/api/getmails", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", getCurrentUA())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", nimailBaseURL)
	req.Header.Set("Referer", nimailBaseURL+"/")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := CheckHTTPStatus(resp, "nimail getEmails"); err != nil {
		return nil, err
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data struct {
		Success string            `json:"success"`
		Mail    []json.RawMessage `json:"mail"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	if data.Success != "true" || len(data.Mail) == 0 {
		return []NormEmail{}, nil
	}

	emails := make([]NormEmail, 0, len(data.Mail))
	for _, rawMsg := range data.Mail {
		var item struct {
			ID          string   `json:"id"`
			From        string   `json:"from"`
			Subject     string   `json:"subject"`
			Time        string   `json:"time"`
			Attachments []string `json:"attachments"`
		}
		if err := json.Unmarshal(rawMsg, &item); err != nil {
			/* 单条解析失败不影响其余邮件 */
			continue
		}

		/* HTML 正文二拉：失败时如实留空，不编造正文 */
		htmlBody := nimailFetchBody(client, email, item.ID)

		/* 归一化器的附件字段要求 []interface{}（元素为 map），按 filename/url 形态传入 */
		attIfaces := make([]interface{}, 0, len(item.Attachments))
		for _, u := range item.Attachments {
			if u == "" {
				continue
			}
			attIfaces = append(attIfaces, map[string]interface{}{
				"filename": nimailAttachmentName(u),
				"url":      u,
			})
		}

		flat := map[string]interface{}{
			"id":          item.ID,
			"from":        html.UnescapeString(strings.TrimSpace(item.From)),
			"to":          email,
			"subject":     html.UnescapeString(strings.TrimSpace(item.Subject)),
			"date":        item.Time,
			"html":        htmlBody,
			"attachments": attIfaces,
		}
		emails = append(emails, NormalizeMap(flat, email))
	}

	return emails, nil
}

// nimailAttachmentName 从平台附件 URL 末段提取文件名。
// 平台格式：.../<mailbox>/<md5>_<urlencoded 文件名>（含@的邮箱在路径中不编码）。
// 取最后一个 / 之后的部分，去掉 "<md5>_" 前缀与查询串，再进行 URL 解码。
func nimailAttachmentName(u string) string {
	if u == "" {
		return ""
	}
	path := u
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		path = path[idx+1:]
	}
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}
	if idx := strings.Index(path, "_"); idx >= 0 && idx < len(path)-1 {
		path = path[idx+1:]
	}
	decoded, err := url.QueryUnescape(path)
	if err != nil {
		return path
	}
	return decoded
}

// nimailFetchBody 对单封邮件 GET /api/raw-html/<邮箱>/<id> 获取 HTML 正文。
// 平台 WAF 检查 Referer；库内共享客户端自动维护 Cookie 罐（foreach 独立会话不依赖它）。
// 路径中的邮箱必须保持裸 @（与官网前端 window.open 完全一致）：平台对 %40 编码的
// 地址不会做路径解码，直接返回 Invalid email address。
// 平台对纯文本邮件返回的正文为空（页面只有统计注入脚本），直接归一化后 HTML 为空，
// 无法凭空还原纯文本正文，如实留空；HTML 邮件正文可完整获取。
// 响应分正文和注入脚本两部分：主体可能整段被剔除（只剩注入脚本，长度约 270B），
// 脚本尾部则不加区分地秒删以保证签名恒定且不影响正文。
func nimailFetchBody(client interface {
	Do(*http.Request) (*http.Response, error)
}, email, mailID string) string {
	rawURL := nimailBaseURL + "/api/raw-html/" + email + "/" + mailID

	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", getCurrentUA())
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", nimailBaseURL+"/")

	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	return nimailSanitizeRawHTML(string(rawBody))
}

// nimailSanitizeRawHTML 清理 raw-html 响应：
// 剥离结尾的百度统计注入脚本/HTML 包装；以 <h1> 开头的错误页（Invalid id/
// Invalid email address/Email not found）同样视为失败。脚本与正文可能被平台包裹于
// <html><body> 中，与纯 HTML 正文无可靠区分依据，且 browsers 常用 HTML
// 同样可能以这两标签开头，故不剥离它们。
func nimailSanitizeRawHTML(raw string) string {
	body := strings.TrimSpace(raw)
	if body == "" {
		return ""
	}

	/* 剔除位于末尾的百度统计注入脚本（无论前面是否有空白） */
	body = strings.TrimSuffix(body, nimailRawHTMLInjection)
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}

	/* 平台错误页 */
	lower := strings.ToLower(body)
	if strings.Contains(lower, "invalid email address") ||
		strings.Contains(lower, "invalid id") ||
		strings.Contains(lower, "email not found") ||
		strings.Contains(lower, "fatal error") {
		return ""
	}

	return body
}
