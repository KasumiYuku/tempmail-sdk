package provider

import (
	"io"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
)

/* 模拟平台 raw-html 响应：真实样本的字节级副本（/tmp/h1b.out，标签顺序已被实测确认） */
const nimailSampleHTML = "<html><body><h1>HTML-TITLE-htest</h1><div id='m'>HTMLBODY-MARKER-htest-9f3b</div></body></html>" +
	"<script>\nvar _hmt = _hmt || [];\n(function() {\n  var hm = document.createElement(\"script\");\n  hm.src = \"https://hm.baidu.com/hm.js?474bbfaaa6cec5667832d09cbd9cb961\";\n  var s = document.getElementsByTagName(\"script\")[0];\n  s.parentNode.insertBefore(hm, s);\n})();\n</script>"

/* 平台对纯文本邮件的响应：只有注入脚本 */
const nimailSampleEmpty = "<script>\nvar _hmt = _hmt || [];\n(function() {\n  var hm = document.createElement(\"script\");\n  hm.src = \"https://hm.baidu.com/hm.js?474bbfaaa6cec5667832d09cbd9cb961\";\n  var s = document.getElementsByTagName(\"script\")[0];\n  s.parentNode.insertBefore(hm, s);\n})();\n</script>"

type nimailMockRT struct {
	body    string
	status  int
	gotURL  string // 记录实际请求 URL
	reqPath string
}

func (m *nimailMockRT) Do(r *http.Request) (*http.Response, error) {
	m.gotURL = r.URL.String()
	m.reqPath = r.URL.Path
	resp := &http.Response{
		StatusCode:    m.status,
		Header:        http.Header{},
		Body:          io.NopCloser(strings.NewReader(m.body)),
		ContentLength: int64(len(m.body)),
	}
	if m.reqPath == "/api/raw-html/probe465328@nimail.cn/1790466434964" {
		resp.Body = io.NopCloser(strings.NewReader(m.body))
	}
	return resp, nil
}

func TestNimailSanitizeRawHTML(t *testing.T) {
	/* HTML 邮件正文完整保留，注入脚本被剥离 */
	got := nimailSanitizeRawHTML(nimailSampleHTML)
	want := "<html><body><h1>HTML-TITLE-htest</h1><div id='m'>HTMLBODY-MARKER-htest-9f3b</div></body></html>"
	if got != want {
		t.Fatalf("sanitize body = %q, want %q", got, want)
	}

	/* 纯文本邮件：只有注入脚本，视为无正文 */
	if got := nimailSanitizeRawHTML(nimailSampleEmpty); got != "" {
		t.Fatalf("sanitize empty = %q, want empty", got)
	}

	/* 平台错误页 */
	if got := nimailSanitizeRawHTML("<h1>Invalid id</h1>"); got != "" {
		t.Fatalf("sanitize invalid id = %q, want empty", got)
	}
	if got := nimailSanitizeRawHTML("<h1>Invalid email address</h1>"); got != "" {
		t.Fatalf("sanitize invalid email = %q, want empty", got)
	}
	if got := nimailSanitizeRawHTML("<h1>Email not found</h1>"); got != "" {
		t.Fatalf("sanitize not found = %q, want empty", got)
	}
}

func TestNimailFetchBody(t *testing.T) {
	mock := &nimailMockRT{body: nimailSampleHTML, status: 200}
	got := nimailFetchBody(mock, "probe465328@nimail.cn", "1790466434964")
	want := "<html><body><h1>HTML-TITLE-htest</h1><div id='m'>HTMLBODY-MARKER-htest-9f3b</div></body></html>"
	if got != want {
		t.Fatalf("fetch body = %q, want %q", got, want)
	}
	if mock.reqPath != "/api/raw-html/probe465328@nimail.cn/1790466434964" {
		t.Fatalf("path = %q", mock.reqPath)
	}
	if strings.Contains(mock.gotURL, "%40") {
		t.Fatalf("邮箱在路径中不得编码 @，平台不解码会报 Invalid email address: %s", mock.gotURL)
	}
}

func TestNimailAttachmentName(t *testing.T) {
	got := nimailAttachmentName("https://www.nimail.cn/api/attachment/probeattach6221@nimail.cn/6fd7cdcdf58a140cdf6ae0a054e2448d_probe_attach_9f3btxt")
	if got != "probe_attach_9f3btxt" {
		t.Fatalf("attachment name = %q", got)
	}
	if got := nimailAttachmentName(""); got != "" {
		t.Fatalf("empty attachment = %q", got)
	}
}

func TestNimailAttachmentNameUnescape(t *testing.T) {
	/* 文件名带 URL 编码时需解码 */
	got := nimailAttachmentName("https://www.nimail.cn/api/attachment/m@nimail.cn/abc_%E4%B8%AD%E6%96%87.txt")
	if got != "中文.txt" {
		t.Fatalf("unescaped name = %q", got)
	}
}
