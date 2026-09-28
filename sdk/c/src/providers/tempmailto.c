/**
 * Tempmailto 渠道 — https://tempmailto.com
 *
 * Laravel Cookie 会话（共享 http.c 无 Cookie 罐，本文件内维护静态 Cookie
 * 桶，逐请求显式回填 Cookie 头并以响应 Set-Cookie 同名覆写）：
 *   - GET / 首页建立会话（权威邮箱由服务端渲染于 #mainEmail，同会话内
 *     恒定，故建箱只需 GET + 提取），CSRF 取
 *     <meta name="csrf-token" content="...">。
 *   - POST /get_messages（表单 _token=<CSRF>&captcha=）返回
 *     {status, mailbox, email_token, messages, histories}，200 即成功。
 *   - 会话粘性：mailbox 与请求邮箱不一致时 POST /change
 *     （_token + name + domain）拉回目标邮箱，仍不一致报错。
 *   - 详情：GET /view/{id} 按候选 class 提取正文块，HTML 转纯文本。
 *
 * Token 约定：邮箱本身（注册表仅要求非空）。邮箱约 10 分钟无活动过期。
 */

#include "tempmail_internal.h"
#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#ifdef _WIN32
#define strcasecmp _stricmp
#define strncasecmp _strnicmp
#else
#include <strings.h>
#endif

#define TMTO_BASE "https://tempmailto.com"
#define TMTO_UA                                                               \
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) " \
  "Chrome/154.0.0.0 Safari/537.36"

/* Cookie 桶容量（"k1=v1; k2=v2" 形式） */
#define TMTO_COOKIE_CAP 8192

/* 首页 GET 浏览器头 */
static const char *tmto_html_headers[] = {
    "User-Agent: " TMTO_UA,
    "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,"
    "image/avif,image/webp,*/*;q=0.8",
    "Accept-Language: en-US,en;q=0.9",
    "Origin: " TMTO_BASE,
    "Referer: " TMTO_BASE "/",
    NULL};

/* 表单 POST 浏览器头（前端 AXIOS 同源调用轮廓） */
static const char *tmto_xhr_headers[] = {
    "User-Agent: " TMTO_UA,
    "Accept: application/json, text/plain, */*",
    "Accept-Language: en-US,en;q=0.9",
    "Origin: " TMTO_BASE,
    "Referer: " TMTO_BASE "/",
    "Content-Type: application/x-www-form-urlencoded; charset=UTF-8",
    "X-Requested-With: XMLHttpRequest",
    NULL};

/* ========== 字符串工具 ========== */

/* 去首尾空白（原串原地修改，返回首地址） */
static char *tmto_trim(char *s) {
  char *p = s;
  while (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')
    p++;
  size_t n = strlen(p);
  while (n > 0 && (p[n - 1] == ' ' || p[n - 1] == '\t' || p[n - 1] == '\n' ||
                   p[n - 1] == '\r'))
    p[--n] = '\0';
  return p;
}

/* 大小写不敏感子串查找（一次命中，用于标签结构定位） */
static const char *tmto_ci_find(const char *hay, const char *needle) {
  size_t nlen = strlen(needle);
  if (nlen == 0)
    return hay;
  for (const char *p = hay; *p; p++) {
    if (strncasecmp(p, needle, nlen) == 0)
      return p;
  }
  return NULL;
}

/* HTML 实体反转义（命名实体 + &#NNN; / &#xHH; 数字实体） */
static char *tmto_html_unescape(const char *src) {
  if (!src)
    return NULL;
  size_t n = strlen(src);
  char *out = (char *)malloc(n + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    if (src[i] != '&') {
      out[o++] = src[i];
      continue;
    }
    const char *ent = NULL;
    char rep = 0;
    if (strncmp(src + i, "&amp;", 5) == 0) {
      ent = "&amp;", rep = '&';
    } else if (strncmp(src + i, "&lt;", 4) == 0) {
      ent = "&lt;", rep = '<';
    } else if (strncmp(src + i, "&gt;", 4) == 0) {
      ent = "&gt;", rep = '>';
    } else if (strncmp(src + i, "&quot;", 6) == 0) {
      ent = "&quot;", rep = '"';
    } else if (strncmp(src + i, "&#39;", 5) == 0) {
      ent = "&#39;", rep = '\'';
    } else if (strncmp(src + i, "&apos;", 6) == 0) {
      ent = "&apos;", rep = '\'';
    } else if (strncmp(src + i, "&nbsp;", 6) == 0) {
      ent = "&nbsp;", rep = ' ';
    } else if (strncmp(src + i, "&#", 2) == 0) {
      long code = 0;
      const char *q = src + i + 2;
      int ok = 0;
      if (*q == 'x' || *q == 'X') {
        q++;
        while (isxdigit((unsigned char)*q)) {
          int d = isdigit((unsigned char)*q)
                      ? *q - '0'
                      : (tolower(*q) - 'a' + 10);
          if (code < 0x110000)
            code = code * 16 + d;
          q++;
          ok = 1;
        }
      } else {
        while (isdigit((unsigned char)*q)) {
          if (code < 0x110000)
            code = code * 10 + (*q - '0');
          q++;
          ok = 1;
        }
      }
      if (ok && *q == ';') {
        if (code > 0 && code < 0x110000 && code <= 255)
          out[o++] = (char)code;
        else if (code >= 0x20 && code <= 255)
          out[o++] = (char)code;
        i = (size_t)(q - src);
        continue;
      }
    }
    if (ent) {
      out[o++] = rep;
      i += strlen(ent) - 1;
      continue;
    }
    out[o++] = src[i];
  }
  out[o] = '\0';
  return out;
}

/* HTML 特殊字符转义（pre 包装正文用） */
static char *tmto_html_escape(const char *src) {
  if (!src)
    return NULL;
  size_t n = strlen(src);
  char *out = (char *)malloc(n * 6 + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    switch (src[i]) {
    case '&':
      memcpy(out + o, "&amp;", 5);
      o += 5;
      break;
    case '<':
      memcpy(out + o, "&lt;", 4);
      o += 4;
      break;
    case '>':
      memcpy(out + o, "&gt;", 4);
      o += 4;
      break;
    case '"':
      memcpy(out + o, "&quot;", 6);
      o += 6;
      break;
    case '\'':
      memcpy(out + o, "&#39;", 5);
      o += 5;
      break;
    default:
      out[o++] = src[i];
    }
  }
  out[o] = '\0';
  return out;
}

/* HTML 转纯文本（去 script/style/标签 + 反转义 + 合并空白） */
static char *tmto_html_to_text(const char *src) {
  if (!src)
    return NULL;
  size_t n = strlen(src);
  char *buf = (char *)malloc(n + 1);
  if (!buf)
    return NULL;

  /* 第一遍：剔除 <script>...</script> 与 <style>...</style> 内容 */
  size_t o = 0;
  for (size_t i = 0; i < n;) {
    if (strncasecmp(src + i, "<script", 7) == 0 ||
        strncasecmp(src + i, "<style", 6) == 0) {
      const char *close = NULL;
      const char *e1 = tmto_ci_find(src + i + 1, "</script>");
      const char *e2 = tmto_ci_find(src + i + 1, "</style>");
      if (e1 && e2)
        close = (e1 < e2) ? e1 : e2;
      else
        close = e1 ? e1 : e2;
      if (close) {
        buf[o++] = ' ';
        i = (size_t)(close - src) + strlen(close);
        continue;
      }
    }
    buf[o++] = src[i++];
  }
  buf[o] = '\0';

  /* 第二遍：剥标签（'<' 至 '>' 置换为空白） */
  size_t o2 = 0;
  int in_tag = 0;
  for (size_t i = 0; buf[i]; i++) {
    if (buf[i] == '<') {
      in_tag = 1;
      continue;
    }
    if (in_tag && buf[i] == '>') {
      in_tag = 0;
      buf[o2++] = ' ';
      continue;
    }
    if (!in_tag)
      buf[o2++] = buf[i];
  }
  buf[o2] = '\0';

  char *unesc = tmto_html_unescape(buf);
  free(buf);
  if (!unesc)
    return NULL;

  /* 第三遍：合并空白并收尾 trim */
  char *out = (char *)malloc(strlen(unesc) + 1);
  if (!out) {
    free(unesc);
    return NULL;
  }
  size_t uo = 0;
  int ws = 0;
  for (size_t i = 0; unesc[i]; i++) {
    char c = unesc[i];
    if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
      ws = 1;
      continue;
    }
    if (ws && uo > 0)
      out[uo++] = ' ';
    ws = 0;
    out[uo++] = c;
  }
  out[uo] = '\0';
  free(unesc);
  return out;
}

/* 表单字段 URL 编码（未保留字符直通，其余 %XX） */
static char *tmto_urlencode(const char *src) {
  size_t n = strlen(src);
  char *out = (char *)malloc(n * 3 + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  static const char hex[] = "0123456789ABCDEF";
  for (size_t i = 0; i < n; i++) {
    unsigned char c = (unsigned char)src[i];
    if (isalnum(c) || c == '-' || c == '_' || c == '.' || c == '~') {
      out[o++] = (char)c;
    } else {
      out[o++] = '%';
      out[o++] = hex[c >> 4];
      out[o++] = hex[c & 0xF];
    }
  }
  out[o] = '\0';
  return out;
}

/* 提取单 tag（lt..gt 范围）内指定属性的引号值；未找到返回 -1 */
static int tmto_tag_attr(const char *lt, const char *gt, const char *attr,
                         char *out, size_t cap) {
  out[0] = '\0';
  size_t alen = strlen(attr);
  const char *p = lt;
  while (p < gt) {
    p = tmto_ci_find(p, attr);
    if (!p || p >= gt)
      break;
    /* 属性名边界确认：前为空白或被 attr 覆盖起点，后邻空白或 '=' */
    const char *q = p + alen;
    while (q < gt && (*q == ' ' || *q == '\t' || *q == '\n' || *q == '\r'))
      q++;
    if (q >= gt || *q != '=') {
      p += alen;
      continue;
    }
    q++;
    while (q < gt && (*q == ' ' || *q == '\t' || *q == '\n' || *q == '\r'))
      q++;
    if (q >= gt || (*q != '"' && *q != '\'')) {
      p += alen;
      continue;
    }
    char quote = *q++;
    const char *end = q;
    while (end < gt && *end != quote)
      end++;
    size_t vlen = (size_t)(end - q);
    if (vlen >= cap)
      vlen = cap - 1;
    memcpy(out, q, vlen);
    out[vlen] = '\0';
    return 0;
  }
  return -1;
}

/* class 属性值是否包含指定词（按空白分词的精确 token 匹配） */
static int tmto_class_has(const char *clsval, const char *word) {
  size_t wlen = strlen(word);
  const char *p = clsval;
  while (p && *p) {
    while (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')
      p++;
    const char *end = p;
    while (*end && *end != ' ' && *end != '\t' && *end != '\n' && *end != '\r')
      end++;
    if ((size_t)(end - p) == wlen && strncmp(p, word, wlen) == 0)
      return 1;
    p = end;
  }
  return 0;
}

/* ========== 会话 Cookie 桶 ========== */

static char tmto_cookie_buf[TMTO_COOKIE_CAP];

/* 清空 Cookie 桶（Generate 建立全新会话前调用） */
static void tmto_cookie_clear(void) { tmto_cookie_buf[0] = '\0'; }

/* 覆写/追加一个 "name=value" 对（同名覆写并保持既有位置，异名追加尾部） */
static void tmto_cookie_set(const char *name, const char *value) {
  if (!name || !name[0] || !value)
    return;
  size_t nlen = strlen(name);
  /* 先扫描既有同名段 */
  const char *match = NULL;
  const char *match_end = NULL;
  const char *p = tmto_cookie_buf;
  while (p && *p) {
    const char *pend = strstr(p, "; ");
    size_t slen = pend ? (size_t)(pend - p) : strlen(p);
    if (slen > nlen && strncmp(p, name, nlen) == 0 && p[nlen] == '=') {
      match = p;
      match_end = pend ? pend : p + slen;
      break;
    }
    if (!pend)
      break;
    p = pend + 2;
  }

  /* 桶剩余空间不足容下结果：放弃本次更新（宁缺勿溢出） */
  size_t need =
      (match ? (size_t)(match - tmto_cookie_buf) : strlen(tmto_cookie_buf)) +
      strlen(tmto_cookie_buf) + nlen + strlen(value) + 8;
  if (need >= sizeof(tmto_cookie_buf))
    return;

  {
    char *nb = (char *)malloc(need);
    if (!nb)
      return;
    if (!match) {
      snprintf(nb, need, "%s%s%s=%s", tmto_cookie_buf,
               tmto_cookie_buf[0] ? "; " : "", name, value);
    } else {
      /* 同名覆写：前段 + 新值 + 后段 */
      size_t pre_len = (size_t)(match - tmto_cookie_buf);
      const char *suf = match_end;
      if (suf[0] == ';')
        suf += 2; /* 越过 "; " 分隔 */
      snprintf(nb, need, "%.*s%s=%s%s%s", (int)pre_len, tmto_cookie_buf, name,
               value, suf[0] ? "; " : "", suf);
    }
    snprintf(tmto_cookie_buf, sizeof(tmto_cookie_buf), "%s", nb);
    free(nb);
  }
}

/* 吸收响应 Set-Cookie 合并串（多组 "name=value" 以 "; " 分隔，逐组覆写） */
static void tmto_cookie_absorb(const char *cookies) {
  if (!cookies || !cookies[0])
    return;
  const char *p = cookies;
  while (p && *p) {
    const char *pend = strstr(p, "; ");
    size_t slen = pend ? (size_t)(pend - p) : strlen(p);
    const char *eq = memchr(p, '=', slen);
    if (eq) {
      char name[256];
      size_t nlen = (size_t)(eq - p);
      if (nlen >= sizeof(name))
        nlen = sizeof(name) - 1;
      memcpy(name, p, nlen);
      name[nlen] = '\0';
      /* 去掉属性残留（值原样保留到段尾；组内不应再含分号） */
      char value[TMTO_COOKIE_CAP];
      size_t vlen = slen - nlen - 1;
      if (vlen >= sizeof(value))
        vlen = sizeof(value) - 1;
      memcpy(value, eq + 1, vlen);
      value[vlen] = '\0';
      tmto_cookie_set(name, value);
    }
    if (!pend)
      break;
    p = pend + 2;
  }
}

/* ========== 请求封装 ========== */

/* 带会话 Cookie 头的请求（桶空时不附加 Cookie 头） */
static tm_http_response_t *tmto_do(tm_http_method_t method, const char *url,
                                   const char **base, const char *body) {
  if (tmto_cookie_buf[0] == '\0')
    return tm_http_request(method, url, base, body, 15);

  char cookie_hdr[TMTO_COOKIE_CAP + 8];
  snprintf(cookie_hdr, sizeof(cookie_hdr), "Cookie: %s", tmto_cookie_buf);

  size_t n = 0;
  while (base[n])
    n++;
  const char **headers = (const char **)malloc(sizeof(char *) * (n + 2));
  if (!headers)
    return tm_http_request(method, url, base, body, 15);
  for (size_t k = 0; k < n; k++)
    headers[k] = base[k];
  headers[n] = cookie_hdr;
  headers[n + 1] = NULL;

  tm_http_response_t *resp = tm_http_request(method, url, headers, body, 15);
  free(headers);
  return resp;
}

/* GET 首页并吸收会话 Cookie；非 2xx 记日志并返回 NULL */
static tm_http_response_t *tmto_fetch_home(void) {
  char url[64];
  snprintf(url, sizeof(url), "%s", TMTO_BASE);
  tm_http_response_t *resp = tmto_do(TM_HTTP_GET, url, tmto_html_headers, NULL);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("tempmailto: 首页 http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  tmto_cookie_absorb(resp->cookies);
  return resp;
}

/* 从首页提取 #mainEmail 的 value（服务端渲染的当前邮箱） */
static int tmto_extract_main_email(const char *page, char *out, size_t cap) {
  out[0] = '\0';
  const char *pos = strstr(page, "id=\"mainEmail\"");
  if (!pos)
    pos = strstr(page, "id='mainEmail'");
  if (!pos)
    return -1;
  /* 回退至所在 tag 起点（pos 前必有一个 '<'） */
  const char *lt = pos;
  while (lt > page && lt[-1] != '<')
    lt--;
  const char *gt = strchr(lt, '>');
  if (!gt)
    return -1;
  if (tmto_tag_attr(lt, gt, "value", out, cap) == 0 && out[0])
    return 0;
  return -1;
}

/* 从首页提取 meta csrf-token content */
static int tmto_extract_csrf(const char *page, char *out, size_t cap) {
  out[0] = '\0';
  const char *cur = page;
  while ((cur = strstr(cur, "<meta")) != NULL) {
    const char *gt = strchr(cur, '>');
    if (gt) {
      char name[64];
      /* 该 meta 须带 name="csrf-token" */
      if (tmto_tag_attr(cur, gt, "name", name, sizeof(name)) == 0 &&
          strcmp(name, "csrf-token") == 0) {
        if (tmto_tag_attr(cur, gt, "content", out, cap) == 0 && out[0])
          return 0;
      }
    }
    cur++;
  }
  return -1;
}

/* GET /view/{id} 提取正文 HTML（失败返回 NULL） */
static char *tmto_view_detail(const char *id) {
  size_t need = strlen(id) + 40;
  char *url = (char *)malloc(need);
  if (!url)
    return NULL;
  snprintf(url, need, "%s/view/%s", TMTO_BASE, id);
  tm_http_response_t *resp = tmto_do(TM_HTTP_GET, url, tmto_html_headers, NULL);
  free(url);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }
  tmto_cookie_absorb(resp->cookies);

  static const char *classes[] = {"mail-body",      "mail_content",
                                  "email-body",     "content-body",
                                  "message-content", "mail-content"};
  char *picked = NULL;
  const char *page = resp->body ? resp->body : "";
  for (size_t k = 0; k < sizeof(classes) / sizeof(classes[0]) && !picked; k++) {
    const char *cur = page;
    const char *pos;
    while ((pos = strstr(cur, classes[k])) != NULL) {
      /* pos 须落在某 tag 内且 class 属性精确含该词 */
      const char *lt = pos;
      while (lt > page && lt[-1] != '<')
        lt--;
      const char *gt = strchr(lt, '>');
      if (!gt)
        break;
      const char *tagname = lt + 1;
      size_t tlen = 0;
      while (tagname + tlen < gt && !isspace((unsigned char)tagname[tlen]) &&
             tagname[tlen] != '>')
        tlen++;
      int ok_tag = (tlen == 3 && strncasecmp(tagname, "div", 3) == 0) ||
                   (tlen == 7 && strncasecmp(tagname, "section", 7) == 0) ||
                   (tlen == 7 && strncasecmp(tagname, "article", 7) == 0);
      char cls[512];
      if (ok_tag && tmto_tag_attr(lt, gt, "class", cls, sizeof(cls)) == 0 &&
          tmto_class_has(cls, classes[k])) {
        /* 内容至首个闭合（div/section/article，与 Go 非贪婪语义一致） */
        const char *body = gt + 1;
        const char *end = NULL;
        static const char *closes[] = {"</div>", "</section>", "</article>"};
        for (size_t c = 0; c < 3; c++) {
          const char *e = tmto_ci_find(body, closes[c]);
          if (e && (!end || e < end))
            end = e;
        }
        size_t block_len = end ? (size_t)(end - body) : strlen(body);
        char *frag = (char *)malloc(block_len + 1);
        if (frag) {
          memcpy(frag, body, block_len);
          frag[block_len] = '\0';
          char *t = tmto_trim(frag);
          if (t[0]) {
            picked = tm_strdup(t);
          }
          free(frag);
        }
        break;
      }
      cur = pos + 1;
    }
  }

  /* 回退：<main> / <article> 整体 */
  if (!picked) {
    static const char *fallbacks[] = {"main", "article"};
    for (size_t k = 0; k < 2 && !picked; k++) {
      const char *cur = page;
      const char *pos;
      size_t open_len = 1 + strlen(fallbacks[k]);
      while ((pos = tmto_ci_find(cur, fallbacks[k])) != NULL) {
        /* 校验为开标签起点 */
        const char *lt = pos - 1;
        if (lt < page || lt[0] != '<') {
          cur = pos + 1;
          continue;
        }
        const char *gt = strchr(lt, '>');
        if (!gt)
          break;
        const char *tagname = lt + 1;
        size_t tlen = 0;
        while (tagname + tlen < gt && !isspace((unsigned char)tagname[tlen]) &&
               tagname[tlen] != '>')
          tlen++;
        if (tlen != open_len - 1 ||
            strncasecmp(tagname, fallbacks[k], tlen) != 0) {
          cur = pos + 1;
          continue;
        }
        char close_tag[24];
        snprintf(close_tag, sizeof(close_tag), "</%s>", fallbacks[k]);
        const char *end = tmto_ci_find(gt + 1, close_tag);
        size_t block_len = end ? (size_t)(end - (gt + 1)) : strlen(gt + 1);
        char *frag = (char *)malloc(block_len + 1);
        if (frag) {
          memcpy(frag, gt + 1, block_len);
          frag[block_len] = '\0';
          char *t = tmto_trim(frag);
          if (t[0]) {
            picked = tm_strdup(t);
          }
          free(frag);
        }
        break;
      }
    }
  }

  tm_http_response_free(resp);
  return picked;
}

/* ========== 事务封装 ========== */

/* GET 首页取 CSRF（同时吸收 Cookie 保持会话鲜活性） */
static char *tmto_get_csrf(void) {
  tm_http_response_t *resp = tmto_fetch_home();
  if (!resp)
    return NULL;
  char csrf[512];
  if (tmto_extract_csrf(resp->body ? resp->body : "", csrf, sizeof(csrf)) !=
          0 ||
      !csrf[0]) {
    TM_LOG_ERR("tempmailto: 首页未找到 csrf-token");
    tm_http_response_free(resp);
    return NULL;
  }
  tm_http_response_free(resp);
  return tm_strdup(csrf);
}

/* POST /get_messages（_token + captcha 留空）；成功返回 JSON root */
static cJSON *tmto_get_messages(void) {
  char *csrf = tmto_get_csrf();
  if (!csrf)
    return NULL;

  size_t need = strlen(csrf) * 3 + 64;
  char *body = (char *)malloc(need);
  char url[64];
  snprintf(url, sizeof(url), "%s/get_messages", TMTO_BASE);
  if (!body) {
    free(csrf);
    return NULL;
  }
  snprintf(body, need, "_token=%s&captcha=", csrf);
  free(csrf);

  tm_http_response_t *resp = tmto_do(TM_HTTP_POST, url, tmto_xhr_headers, body);
  free(body);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("tempmailto 读信: http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  tmto_cookie_absorb(resp->cookies);
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("tempmailto: 解析读信响应失败");
    return NULL;
  }
  return root;
}

/* POST /change 换箱（name/domain 取自目标邮箱）；成功返回变更后的 mailbox */
static char *tmto_change(const char *email) {
  char *csrf = tmto_get_csrf();
  if (!csrf)
    return NULL;

  /* 拆 local part 与域名（缺省 TmSdk / tempmailto.com） */
  char name[256];
  char domain[256];
  const char *at = strchr(email, '@');
  size_t nlen = at ? (size_t)(at - email) : strlen(email);
  if (nlen >= sizeof(name))
    nlen = sizeof(name) - 1;
  memcpy(name, email, nlen);
  name[nlen] = '\0';
  if (!name[0])
    snprintf(name, sizeof(name), "TmSdk");
  if (at && at[1])
    snprintf(domain, sizeof(domain), "%s", at + 1);
  else
    snprintf(domain, sizeof(domain), "tempmailto.com");

  char *ename = tmto_urlencode(name);
  char *edomain = tmto_urlencode(domain);
  if (!ename || !edomain) {
    free(ename);
    free(edomain);
    free(csrf);
    return NULL;
  }
  size_t need = strlen(csrf) + strlen(ename) + strlen(edomain) + 64;
  char *body = (char *)malloc(need);
  char url[64];
  snprintf(url, sizeof(url), "%s/change", TMTO_BASE);
  if (!body) {
    free(ename);
    free(edomain);
    free(csrf);
    return NULL;
  }
  snprintf(body, need, "_token=%s&name=%s&domain=%s", csrf, ename, edomain);
  free(ename);
  free(edomain);
  free(csrf);

  tm_http_response_t *resp = tmto_do(TM_HTTP_POST, url, tmto_xhr_headers, body);
  free(body);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("tempmailto: change 失败 http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  tmto_cookie_absorb(resp->cookies);
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("tempmailto: 解析 change 响应失败");
    return NULL;
  }
  const char *mailbox =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "mailbox"), "");
  char *out = NULL;
  if (mailbox[0])
    out = tm_strdup(mailbox);
  cJSON_Delete(root);
  if (!out)
    TM_LOG_ERR("tempmailto: change 响应异常");
  return out;
}

/* 行字段安全转字符串：字符串 trimmed / 数字转十进制 / 其余空串 */
static void tmto_row_str(const cJSON *row, const char *key, char *out,
                         size_t cap) {
  out[0] = '\0';
  const cJSON *v = cJSON_GetObjectItemCaseSensitive(row, key);
  if (cJSON_IsString(v) && v->valuestring) {
    snprintf(out, cap, "%s", v->valuestring);
    char *t = tmto_trim(out);
    if (t != out)
      memmove(out, t, strlen(t) + 1);
  } else if (cJSON_IsNumber(v)) {
    if (v->valuedouble == (long long)v->valuedouble)
      snprintf(out, cap, "%lld", (long long)v->valuedouble);
    else
      snprintf(out, cap, "%g", v->valuedouble);
  }
}

/* is_seen 三态转布尔：bool / 数字 / 字符串（"1"|"true"） */
static int tmto_is_seen(const cJSON *row) {
  const cJSON *v = cJSON_GetObjectItemCaseSensitive(row, "is_seen");
  if (cJSON_IsBool(v))
    return cJSON_IsTrue(v) ? 1 : 0;
  if (cJSON_IsNumber(v))
    return v->valuedouble != 0 ? 1 : 0;
  if (cJSON_IsString(v) && v->valuestring) {
    if (strcmp(v->valuestring, "1") == 0 ||
        strcasecmp(v->valuestring, "true") == 0)
      return 1;
  }
  return 0;
}

/* 当前 UTC RFC3339（date 兜底） */
static void tmto_utc_now(char *out, size_t cap) {
  time_t t = time(NULL);
  struct tm tmu;
#ifdef _WIN32
  if (gmtime_s(&tmu, &t) != 0)
    memset(&tmu, 0, sizeof(tmu));
#else
  gmtime_r(&t, &tmu);
#endif
  strftime(out, cap, "%Y-%m-%dT%H:%M:%SZ", &tmu);
}

/* ========== 对外接口 ========== */

/**
 * 创建 tempmailto.com 临时邮箱
 * GET 首页建立 Cookie 会话并提取服务端渲染的当前邮箱；
 * Token 约定为邮箱本身。邮箱约 10 分钟无活动过期。
 */
tm_email_info_t *tm_provider_tempmailto_generate(void) {
  tmto_cookie_clear();
  tm_http_response_t *resp = tmto_fetch_home();
  if (!resp)
    return NULL;

  char email[512];
  if (tmto_extract_main_email(resp->body ? resp->body : "", email,
                              sizeof(email)) != 0 ||
      !email[0]) {
    TM_LOG_ERR("tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱");
    tm_http_response_free(resp);
    return NULL;
  }
  tm_http_response_free(resp);

  tm_email_info_t *info = tm_email_info_new();
  if (!info)
    return NULL;
  info->channel = CHANNEL_TEMPMAILTO;
  info->email = tm_strdup(email);
  info->token = tm_strdup(email);
  /* 10 分钟无活动过期的时效语义（毫秒时间戳） */
  info->expires_at = ((long long)time(NULL) + 600) * 1000;
  return info;
}

/**
 * 读取 tempmailto.com 当前邮箱的收件箱
 * 会话粘性：mailbox 与请求邮箱不一致时 change 拉回；仍不一致报错。
 */
tm_email_t *tm_provider_tempmailto_get_emails(const char *email,
                                              const char *token, int *count) {
  *count = 0;
  if (!email || !email[0]) {
    TM_LOG_ERR("tempmailto: 邮箱为空，请重新 Generate");
    *count = -1;
    return NULL;
  }
  /* token 防御性非空（thunk 已校验，此处兜底） */
  if (!token || !token[0]) {
    *count = -1;
    return NULL;
  }

  cJSON *root = tmto_get_messages();
  if (!root) {
    *count = -1;
    return NULL;
  }

  /* 会话粘性：当前 mailbox 与请求邮箱不符 -> change 拉回后重取列表 */
  const char *mailbox =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "mailbox"), "");
  char mb[512];
  snprintf(mb, sizeof(mb), "%s", mailbox);
  char *tmb = tmto_trim(mb);
  if (tmb[0] && strcasecmp(tmb, email) != 0) {
    char *changed = tmto_change(email);
    if (!changed) {
      cJSON_Delete(root);
      *count = -1;
      return NULL;
    }
    if (strcasecmp(changed, email) != 0) {
      TM_LOG_ERR("tempmailto: 会话邮箱无法拉回请求邮箱");
      free(changed);
      cJSON_Delete(root);
      *count = -1;
      return NULL;
    }
    free(changed);
    cJSON_Delete(root);
    root = tmto_get_messages();
    if (!root) {
      *count = -1;
      return NULL;
    }
  }

  cJSON *messages = cJSON_GetObjectItemCaseSensitive(root, "messages");
  if (!cJSON_IsArray(messages) || cJSON_GetArraySize(messages) == 0) {
    cJSON_Delete(root);
    return NULL; /* 空箱，count 保持 0 */
  }

  int n = cJSON_GetArraySize(messages);
  *count = n;
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(root);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *row = cJSON_GetArrayItem(messages, i);
    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;

    /* id（空跳过该行） */
    char id[256];
    tmto_row_str(row, "id", id, sizeof(id));
    if (!id[0]) {
      cJSON_Delete(raw);
      continue;
    }

    /* from：from_name 优先，其次 from_email / from */
    char from[1024];
    tmto_row_str(row, "from_name", from, sizeof(from));
    if (!from[0])
      tmto_row_str(row, "from_email", from, sizeof(from));
    if (!from[0])
      tmto_row_str(row, "from", from, sizeof(from));

    /* subject */
    char subject[2048];
    tmto_row_str(row, "subject", subject, sizeof(subject));

    /* date：receivedAt / received_at / createdAt，空则以当前 UTC 兜底 */
    char date[512];
    tmto_row_str(row, "receivedAt", date, sizeof(date));
    if (!date[0])
      tmto_row_str(row, "received_at", date, sizeof(date));
    if (!date[0])
      tmto_row_str(row, "createdAt", date, sizeof(date));
    if (!date[0])
      tmto_utc_now(date, sizeof(date));

    /* 正文：详情页二拉，失败回退列表候选字段 */
    char *detail_html = tmto_view_detail(id);
    char *text = detail_html ? tmto_html_to_text(detail_html) : NULL;
    if (!text || !text[0]) {
      free(text);
      text = NULL;
      char fallback[16384];
      tmto_row_str(row, "body", fallback, sizeof(fallback));
      if (!fallback[0])
        tmto_row_str(row, "text", fallback, sizeof(fallback));
      if (!fallback[0])
        tmto_row_str(row, "snippet", fallback, sizeof(fallback));
      if (!fallback[0])
        tmto_row_str(row, "preview", fallback, sizeof(fallback));
      if (fallback[0])
        text = tm_strdup(fallback);
    }
    if (!text || !text[0]) {
      free(text);
      text = tm_strdup(subject); /* 最终兜底主题 */
    }
    char *html = detail_html;
    if (!html || !html[0]) {
      /* pre 包装空格随 normalize 兜底，此处显式组装以免 text 空 */
      html = (char *)malloc(strlen(text) * 6 + 64);
      if (html) {
        char *esc = tmto_html_escape(text);
        if (esc) {
          snprintf(html, strlen(text) * 6 + 64,
                   "<html><body><pre>%s</pre></body></html>", esc);
          free(esc);
        } else {
          free(html);
          html = NULL;
        }
      }
    }

    cJSON_AddStringToObject(raw, "id", id);
    if (from[0])
      cJSON_AddStringToObject(raw, "from", from);
    if (subject[0])
      cJSON_AddStringToObject(raw, "subject", subject);
    cJSON_AddStringToObject(raw, "date", date);
    cJSON_AddBoolToObject(raw, "isRead", tmto_is_seen(row) ? 1 : 0);
    if (text && text[0])
      cJSON_AddStringToObject(raw, "text", text);
    if (html && html[0])
      cJSON_AddStringToObject(raw, "html", html);

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
    free(text);
    free(html);
  }

  cJSON_Delete(root);
  return emails;
}