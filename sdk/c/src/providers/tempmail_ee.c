/**
 * tempmail-ee 渠道 — https://tempmail.ee
 *
 * 会话 Cookie 绑定校验模式（无 Cookie 罐）：
 *   - change（换箱）返回的 Set-Cookie 中 temp_mail_session 与 temp_email
 *     共同构成读信凭据；带齐即 200，缺一 403。
 *   - POST /api/mailbox/change 必须带 sec-ch-ua 浏览器特征头，否则 403。
 *   - /api/mails 列表仅元数据，正文须逐个 GET /api/mails/{id}，
 *     content 为 MIME multipart 原文（HTML 实体已反转义），
 *     需解码拆出 text / html。
 *
 * Token 格式："tempmail-ee|temp_email=<邮箱>; temp_mail_session=<会话>"，
 * 读信前按请求邮箱重拼 Cookie 防止凭据错配。
 */

#include "tempmail_internal.h"
#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#define strncasecmp _strnicmp
#define strcasecmp _stricmp
#else
#include <strings.h>
#endif

#define TEMPMAIL_EE_BASE "https://tempmail.ee"
#define TEMPMAIL_EE_UA                                                            \
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) "      \
  "Chrome/154.0.0.0 Safari/537.36"
#define TEMPMAIL_EE_TOKEN_PREFIX "tempmail-ee|"

static const char *tempmail_ee_html_headers[] = {
    "User-Agent: " TEMPMAIL_EE_UA,
    "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    NULL};

/* 浏览器特征安全头（change 必须带 sec-ch-ua；读信带齐更贴近真实浏览器） */
static const char *tempmail_ee_secch_headers[] = {
    "Accept: application/json",
    "Content-Type: application/json",
    "X-Requested-With: XMLHttpRequest",
    "Origin: " TEMPMAIL_EE_BASE,
    "Referer: " TEMPMAIL_EE_BASE "/",
    "Sec-Fetch-Site: same-origin",
    "Sec-Fetch-Mode: cors",
    "Sec-Fetch-Dest: empty",
    "Accept-Language: en-US,en;q=0.9",
    "User-Agent: " TEMPMAIL_EE_UA,
    "Sec-Ch-Ua: \"Chromium\";v=\"154\", \"Google Chrome\";v=\"154\", "
    "\"Not.A/Brand\";v=\"99\"",
    "Sec-Ch-Ua-Mobile: ?0",
    "Sec-Ch-Ua-Platform: \"Linux\"",
    NULL};

/* 无 sec-ch-ua 三件套（单个详情 GET 复用） */
static const char *tempmail_ee_plain_headers[] = {
    "Accept: application/json",
    "Content-Type: application/json",
    "X-Requested-With: XMLHttpRequest",
    "Origin: " TEMPMAIL_EE_BASE,
    "Referer: " TEMPMAIL_EE_BASE "/",
    "Sec-Fetch-Site: same-origin",
    "Sec-Fetch-Mode: cors",
    "Sec-Fetch-Dest: empty",
    "Accept-Language: en-US,en;q=0.9",
    "User-Agent: " TEMPMAIL_EE_UA,
    NULL};

/* ========== 字符串工具 ========== */

/* 去首尾空白（原串原地修改，返回首地址） */
static char *tee_trim(char *s) {
  char *p = s;
  while (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')
    p++;
  size_t n = strlen(p);
  while (n > 0 && (p[n - 1] == ' ' || p[n - 1] == '\t' || p[n - 1] == '\n' ||
                   p[n - 1] == '\r'))
    p[--n] = '\0';
  return p;
}

/* HTML 实体反转义（= &#61;、+ &#43;、/ &#47; 由平台转义，需反转） */
static char *tee_html_unescape(const char *src) {
  if (!src)
    return NULL;
  size_t n = strlen(src);
  char *out = (char *)malloc(n + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    if (src[i] == '&') {
      if (strncmp(src + i, "&amp;", 5) == 0) {
        out[o++] = '&';
        i += 4;
        continue;
      }
      if (strncmp(src + i, "&lt;", 4) == 0) {
        out[o++] = '<';
        i += 3;
        continue;
      }
      if (strncmp(src + i, "&gt;", 4) == 0) {
        out[o++] = '>';
        i += 3;
        continue;
      }
      if (strncmp(src + i, "&quot;", 6) == 0) {
        out[o++] = '"';
        i += 5;
        continue;
      }
      if (strncmp(src + i, "&#61;", 5) == 0) {
        out[o++] = '=';
        i += 4;
        continue;
      }
      if (strncmp(src + i, "&#43;", 5) == 0) {
        out[o++] = '+';
        i += 4;
        continue;
      }
      if (strncmp(src + i, "&#47;", 5) == 0) {
        out[o++] = '/';
        i += 4;
        continue;
      }
      if (strncmp(src + i, "&#39;", 5) == 0) {
        out[o++] = '\'';
        i += 4;
        continue;
      }
      if (strncmp(src + i, "&nbsp;", 6) == 0) {
        out[o++] = ' ';
        i += 5;
        continue;
      }
    }
    out[o++] = src[i];
  }
  out[o] = '\0';
  return out;
}

/* base64 解码（自动跳过空白字符；带 padding 解码） */
static char *tee_b64_decode(const char *src) {
  if (!src)
    return NULL;
  size_t total = 0;
  for (size_t i = 0; src[i]; i++) {
    unsigned char c = (unsigned char)src[i];
    if (c != ' ' && c != '\n' && c != '\r' && c != '\t')
      total++;
  }
  size_t cap = total / 4 * 3 + 4;
  char *out = (char *)malloc(cap);
  if (!out)
    return NULL;
  size_t o = 0;
  unsigned int acc = 0;
  int bits = 0;
  int pad = 0;
  for (size_t i = 0; src[i] && !pad; i++) {
    char c = src[i];
    int v;
    if (c >= 'A' && c <= 'Z')
      v = c - 'A';
    else if (c >= 'a' && c <= 'z')
      v = c - 'a' + 26;
    else if (c >= '0' && c <= '9')
      v = c - '0' + 52;
    else if (c == '+')
      v = 62;
    else if (c == '/')
      v = 63;
    else if (c == '=') {
      pad = 1;
      do {
        i++;
      } while (src[i] == '=' && src[i]);
      if (bits >= 8) {
        bits -= 8;
        out[o++] = (char)((acc >> bits) & 0xFF);
        acc &= (1u << bits) - 1;
      }
      break;
    } else
      continue;
    acc = (acc << 6) | (unsigned int)v;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[o++] = (char)((acc >> bits) & 0xFF);
      acc &= (1u << bits) - 1;
    }
  }
  out[o] = '\0';
  return out;
}

/* quoted-printable 解码（=XX 十六进制 + 软换行 =\\n 折叠） */
static char *tee_qp_decode(const char *src) {
  if (!src)
    return NULL;
  size_t n = strlen(src);
  char *out = (char *)malloc(n + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    if (src[i] == '=' && i + 1 < n && src[i + 1] == '\n') {
      i++;
      continue;
    }
    if (src[i] == '=' && i + 1 < n && src[i + 1] == '\r' && i + 2 < n &&
        src[i + 2] == '\n') {
      i += 2;
      continue;
    }
    if (src[i] == '=' && i + 2 < n && isxdigit((unsigned char)src[i + 1]) &&
        isxdigit((unsigned char)src[i + 2])) {
      unsigned int hi =
          isdigit((unsigned char)src[i + 1])
              ? (unsigned int)(src[i + 1] - '0')
              : (unsigned int)(tolower(src[i + 1]) - 'a' + 10);
      unsigned int lo =
          isdigit((unsigned char)src[i + 2])
              ? (unsigned int)(src[i + 2] - '0')
              : (unsigned int)(tolower(src[i + 2]) - 'a' + 10);
      out[o++] = (char)((hi << 4) | lo);
      i += 2;
      continue;
    }
    out[o++] = src[i];
  }
  out[o] = '\0';
  return out;
}

/* 按 Content-Transfer-Encoding 解码 part 内容 */
static char *tee_decode_part(char *data, const char *cte) {
  data = tee_trim(data);
  if (!data[0])
    return data;
  if (cte && strcasecmp(cte, "base64") == 0) {
    char *decoded = tee_b64_decode(data);
    if (decoded) {
      free(data);
      return tee_trim(decoded);
    }
  }
  if (cte && strcasecmp(cte, "quoted-printable") == 0) {
    char *decoded = tee_qp_decode(data);
    if (decoded) {
      free(data);
      return tee_trim(decoded);
    }
  }
  return data;
}

/* HTML 转纯文本（去 script / 去标签 / 反转义 / 合并空白，简化自 Go 端） */
static char *tee_html_to_text(const char *src) {
  if (!src)
    return NULL;
  size_t n = strlen(src);
  char *buf = (char *)malloc(n * 2 + 1);
  char *out = (char *)malloc(n + 1);
  if (!buf || !out) {
    free(buf);
    free(out);
    return NULL;
  }
  /* 多次扫描：利用 buf 作中间串（先剥 script 再剥标签） */
  size_t o = 0;
  int in_tag = 0;
  for (size_t i = 0; i < n; i++) {
    if (!in_tag && src[i] == '<') {
      /* 尝试跳过 <script ...> ... </script> 与 </script> */
      if (strncasecmp(src + i, "<script", 7) == 0 ||
          strncasecmp(src + i, "</script", 8) == 0 ||
          strncasecmp(src + i, "<style", 6) == 0 ||
          strncasecmp(src + i, "</style", 7) == 0) {
        in_tag = 2;
        continue;
      }
      in_tag = 1;
      continue;
    }
    if (in_tag == 1 && src[i] == '>') {
      in_tag = 0;
      buf[o++] = ' ';
      continue;
    }
    if (in_tag == 2 && src[i] == '>') {
      /* 特殊块结束判定：查找对应闭合标签 */
      const char *end1 = strstr(src + i + 1, "</script>");
      const char *end2 = strstr(src + i + 1, "</style>");
      const char *end = NULL;
      if (end1 && end2) {
        end = (end1 < end2) ? end1 : end2;
      } else {
        end = end1 ? end1 : end2;
      }
      if (end) {
        i = (size_t)(end - src) + 7; /* 跳到闭合标签后（">" 会被循环自增吸收） */
      } else {
        in_tag = 0;
      }
      buf[o++] = ' ';
      continue;
    }
    if (in_tag == 0)
      buf[o++] = src[i];
  }
  buf[o] = '\0';

  char *unesc = tee_html_unescape(buf);
  free(buf);
  if (!unesc)
    return NULL;

  /* 合并空白 */
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

/* 提取单行字符串（原地） */
static char *tee_get_line(char **cursor) {
  char *start = *cursor;
  char *nl = strchr(start, '\n');
  if (nl) {
    *nl = '\0';
    *cursor = nl + 1;
  } else if (*start) {
    *cursor = start + strlen(start);
  } else {
    return NULL;
  }
  return start;
}

/* 小写化字符串（原地） */
static void tee_lower(char *s) {
  for (; *s; s++)
    *s = (char)tolower((unsigned char)*s);
}

/*
 * 解析详情 content 为 text/html：
 * 1) 先反转义（平台已做 HTML 实体转义：= 写成 &#61; 等）。
 * 2) content 是 MIME multipart 原文（含顶部边界装饰头），按首个边界行
 *    切块，各 part 依据 Content-Transfer-Encoding 做 base64 /
 *    quoted-printable 解码，text part 入文本、html part 入 HTML。
 * 3) 无有效 multipart 结构时整体去壳作为正文，避免丢信。
 * 输出 text_out / html_out 由调用方分配（本函数负责填充与互兜底）。
 */
static void tee_parse_content(const char *raw, char *text_out, size_t text_cap,
                              char *html_out, size_t html_cap) {
  text_out[0] = '\0';
  html_out[0] = '\0';
  if (!raw || !raw[0])
    return;

  char *payload = tee_html_unescape(raw);
  if (!payload)
    return;
  /* 规整 \r\n -> \n */
  for (char *p = payload; *p; p++) {
    if (*p == '\r')
      *p = '\n';
  }

  size_t nlines = 1;
  for (char *p = payload; *p; p++) {
    if (*p == '\n')
      nlines++;
  }
  char **lines = (char **)calloc(nlines, sizeof(char *));
  if (!lines) {
    free(payload);
    return;
  }
  {
    char *cursor = payload;
    for (size_t i = 0; i < nlines; i++)
      lines[i] = tee_get_line(&cursor);
  }

  /* 扫描首块找边界行（默认 multipart 边界行处于块首，去 "--" 前缀） */
  char *boundary = NULL;
  for (size_t i = 0; i < nlines && i < 120; i++) {
    char *ln = lines[i];
    if (ln[0] == '-' && ln[1] == '-' && ln[2]) {
      boundary = tee_trim(ln + 2);
      break;
    }
  }

  char *text_cur = text_out;
  char *html_cur = html_out;

  if (!boundary) {
    /* 单 part 降级：依次按「头部区隔（首个空行之后）→ 整体」取内容 */
    char *body = tee_trim(payload);
    if (strchr(body, '\n')) {
      size_t i = 0;
      int found = 0;
      for (; i < nlines; i++) {
        if (lines[i][0] == '\0') {
          found = 1;
          break;
        }
        if (i > 40 || (i >= 3 && !strchr(lines[i], ':')))
          break;
      }
      if (found) {
        /* 拼接空行之后的剩余行 */
        size_t total = 0;
        for (size_t k = i + 1; k < nlines; k++)
          total += strlen(lines[k]) + 1;
        char *joined = (char *)malloc(total + 1);
        if (joined) {
          joined[0] = '\0';
          for (size_t k = i + 1; k < nlines; k++) {
            strcat(joined, lines[k]);
            if (k + 1 < nlines)
              strcat(joined, "\n");
          }
          body = joined;
        }
      }
    }
    char *lower = tm_strdup(payload);
    if (lower) {
      tee_lower(lower);
      char *cte = strstr(lower, "base64") ? "base64"
                                          : (strstr(lower, "quoted-printable")
                                                 ? "quoted-printable"
                                                 : "");
      free(lower);
      char *decoded = tee_decode_part(body, cte);
      snprintf(text_cur, text_cap, "%s", decoded);
      if (decoded != body)
        free(decoded);
    }
  } else {
    /* multipart：按边界切块 */
    size_t blen = strlen(boundary);
    size_t line_i = 0;
    while (line_i < nlines) {
      char *ln = lines[line_i];
      /* 边界行判定：--boundary 或 --boundary-- 开头（跳过 close-delimiter 内
       * 前缀误报） */
      if (ln[0] != '-' || ln[1] != '-' || strncmp(ln + 2, boundary, blen) != 0) {
        line_i++;
        continue;
      }
      if (ln[2 + blen] == '-' && ln[3 + blen] == '-')
        break;

      /* part 头：直到首个空行 */
      char cte[64] = {0};
      char ctype[64] = {0};
      size_t j = line_i + 1;
      for (; j < nlines && lines[j][0] != '\0'; j++) {
        char *colon = strchr(lines[j], ':');
        if (!colon)
          break;
        *colon = '\0';
        char *key = tee_trim(lines[j]);
        char *val = tee_trim(colon + 1);
        tee_lower(key);
        if (cte[0] == '\0' && strcmp(key, "content-transfer-encoding") == 0) {
          snprintf(cte, sizeof(cte), "%s", val);
        } else if (ctype[0] == '\0' && strcmp(key, "content-type") == 0) {
          char *semi = strchr(val, ';');
          if (semi)
            *semi = '\0';
          snprintf(ctype, sizeof(ctype), "%s", tee_trim(val));
        }
      }
      if (j < nlines && lines[j][0] == '\0')
        j++;

      /* part 正文：直到下一个边界行 */
      size_t body_len = 0;
      size_t body_cap = 256;
      char *body = (char *)malloc(body_cap);
      if (!body) {
        line_i++;
        continue;
      }
      body[0] = '\0';
      for (; j < nlines; j++) {
        char *bl = lines[j];
        if (bl[0] == '-' && bl[1] == '-' && strncmp(bl + 2, boundary, blen) == 0)
          break;
        size_t ll = strlen(bl);
        if (body_len + ll + 2 >= body_cap) {
          body_cap = body_len + ll + 256;
          char *nb = (char *)realloc(body, body_cap);
          if (!nb)
            break;
          body = nb;
        }
        memcpy(body + body_len, bl, ll);
        body_len += ll;
        body[body_len++] = '\n';
        body[body_len] = '\0';
      }

      char *decoded = tee_decode_part(body, cte);
      if (strstr(ctype, "text/plain")) {
        if (text_cur[0] == '\0')
          snprintf(text_cur, text_cap, "%s", decoded);
      } else if (strstr(ctype, "text/html")) {
        if (html_cur[0] == '\0')
          snprintf(html_cur, html_cap, "%s", decoded);
      } else {
        if (text_cur[0] == '\0')
          snprintf(text_cur, text_cap, "%s", decoded);
      }
      free(decoded);
      line_i = j;
    }
  }

  /* 互为兜底合成 */
  if (text_out[0] == '\0' && html_out[0] != '\0') {
    char *pt = tee_html_to_text(html_out);
    if (pt) {
      snprintf(text_out, text_cap, "%s", pt);
      free(pt);
    }
  }
  if (html_out[0] == '\0' && text_out[0] != '\0') {
    size_t tl = strlen(text_out);
    size_t cap = tl * 6 + 64;
    if (cap <= html_cap) {
      char *w = html_out;
      w += snprintf(w, html_cap, "<html><body><pre>");
      for (size_t i = 0; i < tl; i++) {
        char c = text_out[i];
        if (c == '&') {
          memcpy(w, "&amp;", 5);
          w += 5;
        } else if (c == '<') {
          memcpy(w, "&lt;", 4);
          w += 4;
        } else if (c == '>') {
          memcpy(w, "&gt;", 4);
          w += 4;
        } else if (c == '"') {
          memcpy(w, "&quot;", 6);
          w += 6;
        } else {
          *w++ = c;
        }
      }
      snprintf(w, html_cap - (size_t)(w - html_out), "</pre></body></html>");
    }
  }

  for (size_t i = 0; i < nlines; i++)
    free(lines[i]);
  free(lines);
  free(payload);
}

/* ========== 会话 Cookie ========== */

/*
 * 从 Set-Cookie 拼接串中提取指定 cookie 值。
 * input 形如 "name=value" 或 "name1=v1; name2=v2; name3=v3"。
 */
static const char *tee_cookie_value(const char *cookies, const char *key) {
  if (!cookies || !key)
    return NULL;
  size_t klen = strlen(key);
  const char *p = cookies;
  const char *seg = p;
  for (;;) {
    /* 迭代 "; " 分段 */
    const char *semi = strstr(seg, "; ");
    size_t seglen = semi ? (size_t)(semi - seg) : strlen(seg);
    if ((size_t)seglen > klen && strncmp(seg, key, klen) == 0) {
      static char buf[512];
      size_t vlen = seglen - klen;
      if (vlen >= sizeof(buf))
        vlen = sizeof(buf) - 1;
      memcpy(buf, seg + klen, vlen);
      buf[vlen] = '\0';
      return buf;
    }
    if (!semi)
      return NULL;
    seg = semi + 2;
  }
}

/* 从 token 提取 temp_mail_session 值（token 必须带渠道前缀） */
static const char *tee_session_from_token(const char *token) {
  size_t plen = strlen(TEMPMAIL_EE_TOKEN_PREFIX);
  if (!token || strncmp(token, TEMPMAIL_EE_TOKEN_PREFIX, plen) != 0)
    return NULL;
  return tee_cookie_value(token + plen, "temp_mail_session=");
}

/* ========== 请求封装 ========== */

/* POST 请求（带浏览器特征头与可选 Cookie） */
static tm_http_response_t *tee_post(const char *path, const char *body,
                                    const char *cookie) {
  char url[256];
  snprintf(url, sizeof(url), "%s%s", TEMPMAIL_EE_BASE, path);

  const char **base = tempmail_ee_secch_headers;
  if (!cookie || !cookie[0])
    return tm_http_request(TM_HTTP_POST, url, base, body, 15);

  char cookie_hdr[1024];
  snprintf(cookie_hdr, sizeof(cookie_hdr), "Cookie: %s", cookie);

  size_t n = 0;
  while (base[n])
    n++;
  const char **headers = (const char **)malloc(sizeof(char *) * (n + 2));
  if (!headers)
    return tm_http_request(TM_HTTP_POST, url, base, body, 15);
  for (size_t k = 0; k < n; k++)
    headers[k] = base[k];
  headers[n] = cookie_hdr;
  headers[n + 1] = NULL;

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, headers, body, 15);
  free(headers);
  return resp;
}

/* GET 请求（浏览器特征头 + 可选 Cookie） */
static tm_http_response_t *tee_get(const char *path, const char *cookie) {
  char url[256];
  snprintf(url, sizeof(url), "%s%s", TEMPMAIL_EE_BASE, path);

  const char **base = tempmail_ee_plain_headers;
  if (!cookie || !cookie[0])
    return tm_http_request(TM_HTTP_GET, url, base, NULL, 15);

  char cookie_hdr[1024];
  snprintf(cookie_hdr, sizeof(cookie_hdr), "Cookie: %s", cookie);

  size_t n = 0;
  while (base[n])
    n++;
  const char **headers = (const char **)malloc(sizeof(char *) * (n + 2));
  if (!headers)
    return tm_http_request(TM_HTTP_GET, url, base, NULL, 15);
  for (size_t k = 0; k < n; k++)
    headers[k] = base[k];
  headers[n] = cookie_hdr;
  headers[n + 1] = NULL;

  tm_http_response_t *resp = tm_http_request(TM_HTTP_GET, url, headers, NULL, 15);
  free(headers);
  return resp;
}

/* 拉取单封详情并解析出 text/html（失败返回 -1） */
static int tee_fetch_detail(const char *msg_id, const char *cookie,
                            char *text_buf, size_t text_cap, char *html_buf,
                            size_t html_cap) {
  size_t need = strlen(msg_id) + 32;
  char *path = (char *)malloc(need);
  if (!path)
    return -1;
  snprintf(path, need, "/api/mails/%s", msg_id);

  tm_http_response_t *resp = tee_get(path, cookie);
  free(path);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return -1;
  }

  cJSON *detail = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!detail)
    return -1;

  const char *content =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(detail, "content"), "");
  if (!content[0]) {
    /* content 缺失时回退兜底候选键 */
    content = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(detail, "text"), "");
    if (!content[0])
      content = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(detail, "body"), "");
    if (!content[0])
      content = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(detail, "html"), "");
  }
  if (content[0]) {
    tee_parse_content(content, text_buf, text_cap, html_buf, html_cap);
  }
  cJSON_Delete(detail);
  return 0;
}

/* ========== 对外接口 ========== */

/**
 * 创建 tempmail.ee 临时邮箱
 * 事务会话：GET / 面熟 → POST /api/mailbox/change（sec-ch-ua 全套头）换新
 * 邮箱 → 提取 Set-Cookie 中的 temp_mail_session，随 token 透传给读信。
 */
tm_email_info_t *tm_provider_tempmail_ee_generate(void) {
  /* 步骤 1：GET / 建立 Cookie 会话（面熟首访，失败不致命） */
  {
    char url[64];
    snprintf(url, sizeof(url), "%s", TEMPMAIL_EE_BASE);
    tm_http_response_t *boot =
        tm_http_request(TM_HTTP_GET, url, tempmail_ee_html_headers, NULL, 15);
    tm_http_response_free(boot);
  }

  /* 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua） */
  const char *body =
      "{\"turnstileToken\":null,\"browserIntegrity\":{"
      "\"webdriver\":false,\"languagesMissing\":false,\"languageMissing\":false,"
      "\"pluginsMissing\":false,\"pluginsUndefined\":false,"
      "\"outerSizeMissing\":false,\"innerSizeMissing\":false,"
      "\"screenMissing\":false,\"screenDepthMissing\":false,"
      "\"timezoneMissing\":false,\"timezoneOffsetMissing\":false,"
      "\"userAgentDataPresent\":true,\"userAgentMissing\":false,"
      "\"platformClass\":\"Linux\",\"mobile\":false,"
      "\"collectionFailed\":false}}";

  tm_http_response_t *chresp =
      tee_post("/api/mailbox/change", body, NULL);
  if (!chresp || chresp->status < 200 || chresp->status >= 300) {
    TM_LOG_ERR("tempmail-ee: 建箱失败 http %ld",
               chresp ? chresp->status : -1);
    tm_http_response_free(chresp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(chresp->body);
  if (!root) {
    TM_LOG_ERR("tempmail-ee: 解析建箱响应失败");
    tm_http_response_free(chresp);
    return NULL;
  }

  cJSON *success = cJSON_GetObjectItemCaseSensitive(root, "success");
  const char *new_email =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "newEmail"), "");
  if (!cJSON_IsTrue(success) || !new_email[0]) {
    TM_LOG_ERR("tempmail-ee: 建箱失败: %s",
               TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "reason"),
                           ""));
    cJSON_Delete(root);
    tm_http_response_free(chresp);
    return NULL;
  }

  /* 步骤 3：从 Set-Cookie 接管会话凭据（仅取 session） */
  const char *cookie_email = tee_cookie_value(chresp->cookies, "temp_email=");
  const char *session = tee_cookie_value(chresp->cookies, "temp_mail_session=");

  const char *final_email = (cookie_email && cookie_email[0]) ? cookie_email
                                                              : new_email;
  if (!session || !session[0]) {
    TM_LOG_ERR("tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信");
    cJSON_Delete(root);
    tm_http_response_free(chresp);
    return NULL;
  }

  /* token = 前缀 + "temp_email=<邮箱>; temp_mail_session=<会话>" */
  size_t tok_need = strlen(TEMPMAIL_EE_TOKEN_PREFIX) + strlen(final_email) +
                    strlen(session) + 64;
  char *token = (char *)malloc(tok_need);
  if (!token) {
    cJSON_Delete(root);
    tm_http_response_free(chresp);
    return NULL;
  }
  snprintf(token, tok_need, "%stemp_email=%s; temp_mail_session=%s",
           TEMPMAIL_EE_TOKEN_PREFIX, final_email, session);

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    free(token);
    cJSON_Delete(root);
    tm_http_response_free(chresp);
    return NULL;
  }
  info->channel = CHANNEL_TEMPMAIL_EE;
  info->email = tm_strdup(final_email);
  info->token = token;
  cJSON_Delete(root);
  tm_http_response_free(chresp);
  return info;
}

/**
 * 读取 tempmail.ee 收件箱
 * 凭据来自 Generate 时从 change 响应提取的 temp_mail_session，
 * 逐请求以显式 Cookie 头携带（temp_email + temp_mail_session 缺一不可）。
 */
tm_email_t *tm_provider_tempmail_ee_get_emails(const char *email,
                                               const char *token, int *count) {
  *count = 0;
  if (!email || !email[0])
    return NULL;
  if (!token || !token[0])
    return NULL;

  /* 解析会话凭据；无有效 temp_mail_session 无法通过平台会话校验 */
  const char *session = tee_session_from_token(token);
  if (!session || !session[0]) {
    TM_LOG_ERR("tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱");
    return NULL;
  }

  /* 会话绑定邮箱：以请求邮箱为准重拼 cookie */
  char cookie[1024];
  snprintf(cookie, sizeof(cookie), "temp_email=%s; temp_mail_session=%s", email,
           session);

  char body[512];
  snprintf(body, sizeof(body), "{\"email\":\"%s\"}", email);

  tm_http_response_t *resp = tee_post("/api/mails", body, cookie);
  if (!resp || resp->status == 403) {
    TM_LOG_ERR("tempmail-ee: inbox http 403（会话 Cookie 校验失败，邮箱可能已"
               "过期，请重新 Generate）");
    tm_http_response_free(resp);
    return NULL;
  }
  if (resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return NULL;

  cJSON *mails = cJSON_GetObjectItemCaseSensitive(root, "mails");
  if (!cJSON_IsArray(mails) || cJSON_GetArraySize(mails) == 0) {
    cJSON_Delete(root);
    return NULL;
  }

  int n = cJSON_GetArraySize(mails);
  *count = n;
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(root);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *msg = cJSON_GetArrayItem(mails, i);
    cJSON *raw = cJSON_CreateObject();

    const char *idv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "id"), "");
    if (!idv[0]) {
      cJSON_Delete(raw);
      continue;
    }
    cJSON_AddStringToObject(raw, "id", idv);

    const char *fromv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "fromAddress"), "");
    if (!fromv[0])
      fromv = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "from"), "");
    if (fromv[0])
      cJSON_AddStringToObject(raw, "from", fromv);

    const char *tov =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "toAddress"), "");
    cJSON_AddStringToObject(raw, "to", tov[0] ? tov : email);

    const char *subjv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "subject"), "");
    if (subjv[0])
      cJSON_AddStringToObject(raw, "subject", subjv);

    const char *datev =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "createdAt"), "");
    if (!datev[0])
      datev = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "date"), "");
    if (!datev[0])
      datev =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "receivedAt"), "");
    if (datev[0])
      cJSON_AddStringToObject(raw, "date", datev);

    cJSON *is_read = cJSON_GetObjectItemCaseSensitive(msg, "isRead");
    if (is_read)
      cJSON_AddItemReferenceToObject(raw, "isRead", is_read);

    emails[i] = tm_normalize_email(raw, email);

    /* 逐封拉详情补正文，失败不阻塞列表其余邮件 */
    {
      char text_buf[64 * 1024];
      char html_buf[64 * 1024];
      text_buf[0] = '\0';
      html_buf[0] = '\0';
      if (tee_fetch_detail(idv, cookie, text_buf, sizeof(text_buf), html_buf,
                           sizeof(html_buf)) == 0) {
        if (text_buf[0]) {
          free(emails[i].text);
          emails[i].text = tm_strdup(text_buf);
        }
        if (html_buf[0]) {
          free(emails[i].html);
          emails[i].html = tm_strdup(html_buf);
        }
      }
    }
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}