package provider

import (
	"fmt"
	"html"
	"io"
	"math/rand"
	"net/url"
	"strconv"
	"strings"

	http "github.com/bogdanfinn/fhttp"
)

/*
 * mailnesia 渠道（mailnesia.com）
 * 公开收件箱：任意的本地部分无需注册即存在，访问收件箱页面即"建箱"。
 * 页面协议依据其官方开源仓库（github.com/Gilwyad/mailnesia.com）源码实测核对：
 * 列信页 GET /mailbox/{name}（分页 ?p=N，每页 40 封），行模板为
 * <tr id="{id}" class="emailheader">，内含 5 个 <td>，前 4 个依次为
 * <time datetime="{YYYY-MM-DD HH:mm:ss+00:00}">、发件人 <a class="email">、
 * 收件人 <a class="email">、主题 <a class="email">；id 为数据库自增整数。
 * 详情页 GET /mailbox/{name}/{id}，正文容器结构为
 * <div class="emails"><div class="body"><div class="boxcontent"><div class="pill-content">
 * <div id="{key}_{id}">{正文HTML}</div> ... </div></div></div></div>，
 * 纯文本部分 key 为 text_plain（正文经 <pre> 包裹、转义与 URL 链接化），
 * HTML 部分 key 为 text_html；请求不存在的 id 时站点回退渲染该邮箱列表页。
 */

const mailnesiaBase = "https://mailnesia.com"
const mailnesiaDomain = "mailnesia.com"

/* mailnesiaGetText 使用共享客户端 GET 抓取页面文本，非 2xx 返回错误 */
func mailnesiaGetText(u string) (string, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,*/*")
	resp, err := HTTPClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("mailnesia: http %d", resp.StatusCode)
	}
	return string(body), nil
}

/* mailnesiaRandomLocal 生成随机本地部分（19 位小写字母数字，tn 前缀） */
func mailnesiaRandomLocal() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 19)
	copy(b, "tn")
	for i := 2; i < len(b); i++ {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

/* mailnesiaLocalPart 从邮箱地址提取 @ 前的本地部分 */
func mailnesiaLocalPart(email string) string {
	parts := strings.Split(strings.TrimSpace(email), "@")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

/* mailnesiaMailboxURL 拼接列信页地址 */
func mailnesiaMailboxURL(local string) string {
	return fmt.Sprintf("%s/mailbox/%s", mailnesiaBase, url.PathEscape(local))
}

/* mailnesiaDetailURL 拼接详情页地址 */
func mailnesiaDetailURL(local, id string) string {
	return fmt.Sprintf("%s/%s", mailnesiaMailboxURL(local), url.PathEscape(id))
}

/* mailnesiaCleanText 去除 HTML 标签并解实体后压缩空白 */
func mailnesiaCleanText(raw string) string {
	text := strings.ReplaceAll(raw, "<br>", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\t", " ")
	for {
		start := strings.Index(text, "<")
		if start < 0 {
			break
		}
		end := strings.Index(text[start+1:], ">")
		if end < 0 {
			break
		}
		text = text[:start] + text[start+2+end:]
	}
	return strings.Join(strings.Fields(html.UnescapeString(text)), " ")
}

/*
 * mailnesiaParsedCell 列表页单行解析结果。
 * id 为数字自增主键，from/to/subject 为行内前三个文本锚点，
 * date 为 <time datetime> 属性值（UTC 格式 YYYY-MM-DD HH:mm:ss+00:00）。
 */
type mailnesiaParsedCell struct {
	id      int64
	date    string
	from    string
	to      string
	subject string
}

/* mailnesiaParseCell 解析一个 <tr class="emailheader"> 单元并返回解析结果 */
func mailnesiaParseCell(raw string) (mailnesiaParsedCell, bool) {
	var cell mailnesiaParsedCell
	trim := strings.TrimSpace(raw)
	if trim == "" || !strings.Contains(trim, "emailheader") {
		return cell, false
	}
	/* 行首模板为 <tr id="{数字}" class="emailheader"：取 id="..." 引号内值 */
	if ki := strings.Index(trim, `id="`); ki >= 0 {
		val := trim[ki+len(`id="`):]
		if end := strings.Index(val, `"`); end >= 0 {
			if idNum, err := strconv.ParseInt(strings.TrimSpace(val[:end]), 10, 64); err == nil && idNum > 0 {
				cell.id = idNum
			}
		}
	}
	if cell.id == 0 {
		return cell, false
	}

	/* 提取 <time datetime="..."> 属性值作为接收时间 */
	if ti := strings.Index(raw, `<time`); ti >= 0 {
		seg := raw[ti:]
		if gt := strings.Index(seg, ">"); gt >= 0 {
			seg = seg[:gt]
		}
		key := `datetime="`
		if ki := strings.Index(seg, key); ki >= 0 {
			val := seg[ki+len(key):]
			if end := strings.Index(val, `"`); end >= 0 {
				cell.date = html.UnescapeString(strings.TrimSpace(val[:end]))
			}
		}
	}

	/* 行内前三个 <a class="email"> 锚点依次为发件人、收件人、主题 */
	parts := strings.Split(raw, `<a class="email"`)
	if len(parts) < 4 {
		return cell, false
	}
	anchors := parts[1:4]
	getText := func(i int) string {
		seg := anchors[i]
		if gt := strings.Index(seg, ">"); gt >= 0 {
			seg = seg[gt+1:]
			if end := strings.Index(seg, "</a>"); end >= 0 {
				seg = seg[:end]
			}
		}
		return seg
	}
	cell.from = mailnesiaCleanText(getText(0))
	cell.to = mailnesiaCleanText(getText(1))
	cell.subject = mailnesiaCleanText(getText(2))
	return cell, true
}

/* mailnesiaParseListPage 从列信页 HTML 解析全部邮件行 */
func mailnesiaParseListPage(page string) []mailnesiaParsedCell {
	cells := make([]mailnesiaParsedCell, 0, 16)
	rest := page
	for {
		at := strings.Index(rest, `class="emailheader"`)
		if at < 0 {
			break
		}
		/* 回退到行首 <tr */
		tr := strings.LastIndex(rest[:at], "<tr")
		if tr < 0 {
			rest = rest[at+1:]
			continue
		}
		end := strings.Index(rest[at:], "</tr>")
		if end < 0 {
			break
		}
		end += at + len("</tr>")
		if cell, ok := mailnesiaParseCell(rest[tr:end]); ok {
			cells = append(cells, cell)
		}
		rest = rest[end:]
	}
	return cells
}

/*
 * mailnesiaExtractDetail 提取详情页正文。
 * 依据官方模板，正文包裹在 <div id="{key}_{id}"> 内（key 明细按
 * text_plain / text_html 优先级取），其上层有唯一的 pill-content 骨架。
 * 正文为完整 HTML，须按起始标签后的同名 </div> 配对截断，
 * 不能用第一个 </div> 截断，否则嵌套 div 的 HTML 正文会被截断。
 */
func mailnesiaExtractDetail(page string, id int64) (text, htmlPart string) {
	anchor := `class="pill-content"`
	at := strings.LastIndex(page, anchor)
	if at < 0 {
		return "", ""
	}
	search := page[at:]

	/* 精确匹配目标 id 的正文容器，防止不同 id 的邮件互相截串 */
	for _, key := range []string{"text_plain", "text_html"} {
		needle := `id="` + key + `_` + strconv.FormatInt(id, 10) + `"`
		ki := strings.Index(search, needle)
		if ki < 0 {
			continue
		}
		startEnd := strings.Index(search[ki:], ">")
		if startEnd < 0 {
			continue
		}
		start := ki + startEnd + 1

		/* 从起始标签后的第一个 "<" 开始，按嵌套深度配对到对应的 </div>。
		 * 容器自身已占一层（depth 初始 1），内层 <div> 加一、</div> 减一，
		 * 减到 0 处即本容器闭合。 */
		end := -1
		depth := 1
		pos := start
		for pos < len(search) {
			lt := strings.IndexByte(search[pos:], '<')
			if lt < 0 {
				break
			}
			lt += pos
			gt := strings.IndexByte(search[lt:], '>')
			if gt < 0 {
				break
			}
			gt += lt
			tok := strings.TrimSpace(search[lt+1 : gt])
			if tok == "" {
				pos = gt + 1
				continue
			}
			/* 跳过注释与 CDATA 等非标签记号 */
			first := tok[0]
			if first == '!' || first == '?' {
				pos = gt + 1
				continue
			}
			/* 取标签名：闭合标签以 / 开头；其余取第一个空白前单词并去自闭合斜杠 */
			nameTok := tok
			if strings.HasPrefix(nameTok, "/") {
				nameTok = nameTok[1:]
			}
			if n := strings.IndexAny(nameTok, " /"); n >= 0 {
				nameTok = nameTok[:n]
			}
			tagName := strings.ToLower(nameTok)
			if tagName == "div" {
				if strings.HasPrefix(tok, "/") {
					depth--
					if depth <= 0 {
						end = lt
						break
					}
				} else {
					depth++
				}
			}
			pos = gt + 1
		}
		if end < 0 {
			if nk := strings.Index(search[start:], "</div>"); nk >= 0 {
				end = start + nk
			}
		}
		content := strings.TrimSpace(search[start:end])
		content = strings.TrimSpace(strings.TrimSuffix(content, "</div>"))
		if content == "" {
			continue
		}
		if key == "text_plain" {
			text = mailnesiaCleanText(content)
		} else if key == "text_html" {
			htmlPart = content
		}
	}
	return text, htmlPart
}

/*
 * MailnesiaGenerate 创建 mailnesia 地址。
 * mailnesia 无需注册，此处仅以 GET 收件箱页验证网络可达并拿到空箱 200；
 * token 语义为"公开邮箱地址"，与邮箱一致（任何 GET 都能读取该收件箱）。
 */
func MailnesiaGenerate() (*CreatedMailbox, error) {
	/*
	 * 平台级风控实锤（2026-09-28）：mailnesia.com/mailbox/{name} 对
	 * SDK tls-client 浏览器指纹与 curl/urllib 多种 UA 全部返回 403
	 * （接入代理证得 X-Proxied 命中即 403、24h 冷却；旧后缀 @etgdev.de /
	 * @binkmail.com 已漂移）。实现保持原样，403 如实上抛。
	 */
	local := mailnesiaRandomLocal()
	if _, err := mailnesiaGetText(mailnesiaMailboxURL(local)); err != nil {
		return nil, err
	}
	email := local + "@" + mailnesiaDomain
	return &CreatedMailbox{Channel: "mailnesia", Email: email, Token: email}, nil
}

/*
 * MailnesiaGetEmails 获取 mailnesia 收件箱邮件列表。
 * 先 GET 列信页（仅第 0 页），逐行解析后按需 GET 详情页补全正文。
 */
func MailnesiaGetEmails(email string) ([]NormEmail, error) {
	local := mailnesiaLocalPart(email)
	if local == "" {
		return nil, fmt.Errorf("mailnesia: empty email")
	}
	page, err := mailnesiaGetText(mailnesiaMailboxURL(local))
	if err != nil {
		return nil, err
	}
	cells := mailnesiaParseListPage(page)
	out := make([]NormEmail, 0, len(cells))
	for _, cell := range cells {
		flat := map[string]interface{}{
			"id":      cell.id,
			"from":    cell.from,
			"to":      cell.to,
			"subject": cell.subject,
			"date":    cell.date,
		}
		text, htmlPart := mailnesiaExtractDetail(page, cell.id)
		if text == "" && htmlPart == "" {
			if detail, derr := mailnesiaGetText(mailnesiaDetailURL(local, strconv.FormatInt(cell.id, 10))); derr == nil {
				text, htmlPart = mailnesiaExtractDetail(detail, cell.id)
			}
		}
		if text != "" {
			flat["text"] = text
		}
		if htmlPart != "" {
			flat["html"] = htmlPart
		}
		out = append(out, NormalizeMap(flat, email))
	}
	return out, nil
}
