package provider

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

const neighboursBaseURL = "https://neighbours.sh/api/v1"

var neighboursHeaders = map[string]string{
	"Accept":     "application/json",
	"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
}

type neighboursDomainListResponse struct {
	Data struct {
		Domains []string `json:"domains"`
	} `json:"data"`
}

/* neighboursInboxRow 收件箱列表行：uid 为数字，用 json.Number 承接避免 float64 精度丢失（科学计数法） */
type neighboursInboxRow struct {
	UID         json.Number `json:"uid"`
	From        any         `json:"from"`
	To          any         `json:"to"`
	Subject     any         `json:"subject"`
	Date        any         `json:"date"`
	Attachments any         `json:"attachments"`
}

type neighboursInboxResponse struct {
	Data []neighboursInboxRow `json:"data"`
}

/* neighboursDetailData 单邮件详情：html 实测可能为布尔 false，用 any 承接，扁平化时非字符串视为空 */
type neighboursDetailData struct {
	UID         json.Number `json:"uid"`
	From        any         `json:"from"`
	To          any         `json:"to"`
	Subject     any         `json:"subject"`
	Text        any         `json:"text"`
	HTML        any         `json:"html"`
	Date        any         `json:"date"`
	ReceivedAt  any         `json:"received_at"`
	Snippet     any         `json:"snippet"`
	Attachments any         `json:"attachments"`
}

func neighboursRequest(path string, allowNotFound bool) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, neighboursBaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range neighboursHeaders {
		req.Header.Set(k, v)
	}

	resp, err := HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if allowNotFound && resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("neighbours http %d", resp.StatusCode)
	}
	return raw, nil
}

func neighboursJSON(path string, out any, allowNotFound bool) error {
	raw, err := neighboursRequest(path, allowNotFound)
	if err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func neighboursRandomInt(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func neighboursRandomLocal() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	b.WriteString("sdk")
	for i := 0; i < 16; i++ {
		b.WriteByte(chars[neighboursRandomInt(len(chars))])
	}
	return b.String()
}

func neighboursAnyString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case []any:
		for _, item := range v {
			if hit := neighboursAnyString(item); hit != "" {
				return hit
			}
		}
		return ""
	case map[string]any:
		if s, ok := v["address"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		if s, ok := v["text"].(string); ok && strings.Contains(s, "@") {
			return strings.TrimSpace(s)
		}
		if inner, ok := v["value"]; ok {
			return neighboursAnyString(inner)
		}
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func neighboursFirstAddress(value any) string {
	if hit := neighboursAnyString(value); hit != "" {
		return hit
	}
	return ""
}

func neighboursFlattenMessage(raw map[string]any, recipient string) map[string]any {
	text := neighboursAnyString(raw["text"])
	if text == "" {
		text = neighboursAnyString(raw["snippet"])
	}
	date := neighboursAnyString(raw["date"])
	if date == "" {
		date = neighboursAnyString(raw["received_at"])
	}
	to := neighboursFirstAddress(raw["to"])
	if to == "" {
		to = recipient
	}
	return map[string]any{
		"id":          neighboursAnyString(raw["uid"]),
		"from":        neighboursFirstAddress(raw["from"]),
		"to":          to,
		"subject":     neighboursAnyString(raw["subject"]),
		"text":        text,
		"html":        neighboursHTML(raw["html"]),
		"date":        date,
		"attachments": raw["attachments"],
	}
}

/* neighboursHTML 详情响应的 html 有时是文字正文，有时是布尔 false，非字符串视为空 */
func neighboursHTML(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

/* neighboursDetail 拉取单邮件详情，返回结构体指针，失败返回 nil */
func neighboursDetail(address, uid string) *neighboursDetailData {
	raw, err := neighboursRequest("/inbox/"+url.PathEscape(address)+"/"+url.PathEscape(uid), true)
	if err != nil || raw == nil {
		return nil
	}
	var data struct {
		Data *neighboursDetailData `json:"data"`
	}
	if err := json.Unmarshal(raw, &data); err != nil || data.Data == nil {
		return nil
	}
	return data.Data
}

/* neighboursFlattenDetail 将详情结构体扁平化为归一化用的中间 map，字段空缺时兜底 */
func neighboursFlattenDetail(row neighboursInboxRow, detail *neighboursDetailData, recipient string) map[string]any {
	raw := map[string]any{
		"uid":         row.UID,
		"from":        row.From,
		"to":          row.To,
		"subject":     row.Subject,
		"date":        row.Date,
		"attachments": row.Attachments,
	}
	if detail != nil {
		fields := map[string]any{
			"uid":         detail.UID,
			"from":        detail.From,
			"to":          detail.To,
			"subject":     detail.Subject,
			"text":        detail.Text,
			"html":        detail.HTML,
			"date":        detail.Date,
			"received_at": detail.ReceivedAt,
			"snippet":     detail.Snippet,
			"attachments": detail.Attachments,
		}
		for k, v := range fields {
			/* 以列表行已有字段为准，不丢弃 subject/from/date，正文 text/html 字段由详情补充 */
			switch k {
			case "uid", "from", "to", "subject", "date", "attachments":
				if v != nil {
					raw[k] = v
				}
			default:
				raw[k] = v
			}
		}
	}

	out := neighboursFlattenMessage(raw, recipient)
	if out["id"] == "" {
		if uid := row.UID.String(); uid != "" {
			out["id"] = uid
		} else if detail != nil && detail.UID.String() != "" {
			out["id"] = detail.UID.String()
		}
	}
	return out
}

func NeighboursGenerate(domain *string) (*CreatedMailbox, error) {
	var data neighboursDomainListResponse
	if err := neighboursJSON("/config/domains", &data, false); err != nil {
		return nil, err
	}
	domains := make([]string, 0, len(data.Data.Domains))
	for _, item := range data.Data.Domains {
		if hit := strings.TrimSpace(strings.ToLower(item)); hit != "" {
			domains = append(domains, hit)
		}
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("neighbours: domain list is empty")
	}

	selected := domains[neighboursRandomInt(len(domains))]
	if domain != nil && strings.TrimSpace(*domain) != "" {
		wanted := strings.TrimSpace(strings.ToLower(*domain))
		selected = ""
		for _, item := range domains {
			if item == wanted {
				selected = item
				break
			}
		}
		if selected == "" {
			return nil, fmt.Errorf("neighbours: unsupported domain %s", *domain)
		}
	}

	email := neighboursRandomLocal() + "@" + selected
	return &CreatedMailbox{
		Channel: "neighbours",
		Email:   email,
		Token:   email,
	}, nil
}

func NeighboursGetEmails(email string) ([]NormEmail, error) {
	address := strings.TrimSpace(email)
	if address == "" {
		return nil, fmt.Errorf("neighbours: empty email")
	}

	var data neighboursInboxResponse
	if err := neighboursJSON("/inbox/"+url.PathEscape(address), &data, true); err != nil {
		return nil, err
	}
	if len(data.Data) == 0 {
		return []NormEmail{}, nil
	}

	out := make([]NormEmail, 0, len(data.Data))
	for _, row := range data.Data {
		/* uid 为 json.Number，须验证为纯数字后整型格式化，防止 float64 科学计数法破坏详情 URL */
		uidRaw := strings.TrimSpace(row.UID.String())
		uid := ""
		if uidRaw != "" && isDigits(uidRaw) {
			uid = uidRaw
		}
		var detail *neighboursDetailData
		if uid != "" {
			detail = neighboursDetail(address, uid)
		}
		/* 详情获取失败时不丢弃列表行已有的 subject/from/date */
		out = append(out, NormalizeMap(neighboursFlattenDetail(row, detail, address), address))
	}
	return out, nil
}

/* isDigits 校验字符串是否全部由十进制数字组成 */
func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}
