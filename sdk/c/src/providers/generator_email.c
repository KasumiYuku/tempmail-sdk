/**
 * GeneratorEmail 渠道 — https://generator.email（PHP SSR 网页型）
 *
 * 调研实证结论（与 Go 端 generator_email.go 一致）：
 *   - 无独立建箱 API：SSR 直接生成随机邮箱并写进页面内联
 *     window.SITE_DATA：cur_user:"hripynok"、cur_domain:"redproxies.com"
 *     （邮箱=user@domain），同页 Set-Cookie:
 *     inbox_ctx=redproxies.com%2Fhripynok（URL 编码）选中该邮箱会话。
 *   - 读信同为 SSR：GET /inbox4/ 带 inbox_ctx Cookie 返回该邮箱渲染页。
 *     信件列表渲染在 #email-table，每条为 class 含 list-group-item2 的
 *     行（平台实测真实类为 list-group-item2，非 list-group-item）内
 *     三个子 div：from_div_45g45gg（From）、subj_div_45g45gg（Subject）、
 *     time_div_45g45gg（Time (UTC)）。空箱时容器留空、num_mess=0。
 *   - 限制：本站邮件正文不提供纯文本/HTML 原文（默认渲染摘要），
 *     SDK 按列表三要素归一。
 *
 * Cookie 策略：模块内静态维护 inbox_ctx（Generate 首页夺取，读信回带）。
 * token 语义：{"email","domain","user"} JSON 快照。
 */

#include "tempmail_internal.h"
#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#define strncasecmp _strnicmp
#else
#include <strings.h>
#endif

#define GE_BASE "https://generator.email"
#define GE_INBOX "https://generator.email/inbox4/"
#define GE_UA                                                                 \
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) " \
  "Chrome/154.0.0.0 Safari/537.36"
#define GE_MAX_MAILS 64
#define GE_CELL_CAP 512

/* 模块级 inbox_ctx Cookie 状态（本渠道独占） */
static char g_inbox_ctx[512];

/* 收下响应 Set-Cookie 中 inbox_ctx 值（页面 Set-Cookie: inbox_ctx=域%2F用户） */
static void ge_merge_cookies(const char *cookies) {
  if (!cookies)
    return;
  const char *p = cookies;
  while (p && *p) {
    const char *eq = strstr(p, "=");
    if (!eq)
      break;
    size_t nlen = (size_t)(eq - p);
    const char *val = eq + 1;
    size_t vlen = strcspn(val, ";");
    if (nlen == 9 && strncmp(p, "inbox_ctx", 9) == 0 && vlen > 0) {
      size_t cap = sizeof(g_inbox_ctx);
      size_t copy = vlen < cap - 1 ? vlen : cap - 1;
      memcpy(g_inbox_ctx, val, copy);
      g_inbox_ctx[copy] = '\0';
    }
    const char *semi = strchr(val, ';');
    if (!semi)
      break;
    p = semi + 1;
    while (*p == ' ')
      p++;
  }
}

/* 去标签与脚本/样式块，压缩空白（定长区间版本） */
static void ge_strip_tags_n(const char *src, size_t srclen, char *out,
                            size_t cap) {
  size_t w = 0;
  size_t i = 0;
  int need_space = 0;
  while (i < srclen) {
    /* 跳过 script / style 完整块 */
    if (i + 6 < srclen &&
        (strncmp(src + i, "<script", 7) == 0 ||
         strncmp(src + i, "<style", 6) == 0)) {
      const char *close_tag = strncmp(src + i, "<script", 7) == 0
                                  ? "</script>"
                                  : "</style>";
      /* 区间内查找闭合标签 */
      int found_close = 0;
      for (size_t j = i; j + strlen(close_tag) <= srclen; j++) {
        if (strncmp(src + j, close_tag, strlen(close_tag)) == 0) {
          i = j + strlen(close_tag);
          found_close = 1;
          break;
        }
      }
      if (found_close) {
        need_space = 1;
        continue;
      }
    }
    if (src[i] == '<') {
      while (i < srclen && src[i] != '>')
        i++;
      if (i < srclen)
        i++;
      need_space = 1;
      continue;
    }
    if (isspace((unsigned char)src[i])) {
      need_space = 1;
      i++;
      continue;
    }
    if (need_space && w > 0 && w + 1 < cap) {
      out[w++] = ' ';
      need_space = 0;
    }
    if (w + 1 < cap)
      out[w++] = src[i];
    i++;
  }
  out[w] = '\0';
}

/* 去标签与脚本/样式块（\0 结尾版本，供页面区块使用） */
static void ge_strip_tags(const char *src, char *out, size_t cap) {
  ge_strip_tags_n(src, strlen(src), out, cap);
}

/* 提取条目内 class 含 <cls> 的 div 内文本；成功返回 1 */
static int ge_extract_cell(const char *row, size_t rowlen, const char *cls,
                           char *out, size_t cap) {
  size_t clen = strlen(cls);
  out[0] = '\0';
  /* 扫描整个行区间提取 class 匹配块内容 */
  for (size_t i = 0; i + clen <= rowlen; i++) {
    if (strncmp(row + i, cls, clen) != 0)
      continue;
    /* 回溯块起点（最近的 '<'） */
    size_t pos = i;
    while (pos > 0 && row[pos - 1] != '<')
      pos--;
    /* 跳过标签，取内容起止 */
    size_t k = i;
    while (k < rowlen && row[k] != '>')
      k++;
    if (k >= rowlen)
      return 0;
    size_t content_start = k + 1;
    /* 内容终点：最近的 </div> 或行尾 */
    size_t content_end = rowlen;
    for (size_t j = content_start; j + 6 <= rowlen; j++) {
      if (strncmp(row + j, "</div>", 6) == 0) {
        content_end = j;
        break;
      }
    }
    if (content_end <= content_start)
      continue;
    ge_strip_tags_n(row + content_start, content_end - content_start, out, cap);
    if (out[0])
      return 1;
  }
  return 0;
}

/* 请求收件箱渲染页并返回 HTML 拷贝（需 free）；失败返回 NULL */
static char *ge_fetch_page(void) {
  const char *headers[8];
  int n = 0;
  headers[n++] = "User-Agent: " GE_UA;
  headers[n++] = "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,"
                 "image/avif,image/webp,*/*;q=0.8";
  headers[n++] = "Accept-Language: en-US,en;q=0.9";
  headers[n++] = "Referer: " GE_BASE "/";
  char cookie_hdr[600];
  if (g_inbox_ctx[0]) {
    snprintf(cookie_hdr, sizeof(cookie_hdr), "Cookie: inbox_ctx=%s",
             g_inbox_ctx);
    headers[n++] = cookie_hdr;
  }
  headers[n] = NULL;

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, GE_INBOX, headers, NULL, 20);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("generator-email: 请求失败 http %ld", resp ? resp->status : -1L);
    tm_http_response_free(resp);
    return NULL;
  }
  ge_merge_cookies(resp->cookies);
  char *body = tm_strdup(resp->body ? resp->body : "");
  tm_http_response_free(resp);
  return body;
}

/* 从页面提取 SITE_DATA 快照字段（如 cur_user:"hripynok"）；返回 malloc 拷贝
 * （需 free），无匹配返回 NULL */
static char *ge_field(const char *src, const char *field) {
  size_t fldlen = strlen(field);
  char *needle = (char *)malloc(fldlen + 3);
  if (!needle)
    return NULL;
  snprintf(needle, fldlen + 3, "%s:\"", field);
  const char *hit = strstr(src, needle);
  free(needle);
  if (!hit)
    return NULL;
  const char *val = hit + fldlen + 2;
  size_t vlen = strcspn(val, "\"");
  if (vlen == 0)
    return NULL;
  char *out = (char *)malloc(vlen + 1);
  if (!out)
    return NULL;
  memcpy(out, val, vlen);
  out[vlen] = '\0';
  return out;
}

/* 解析列表条目块：提取 from/subject/time 三要素（任一非空即成功） */
static int ge_parse_row(const char *row, size_t rowlen, char *from,
                        size_t fcap, char *subj, size_t scap, char *when,
                        size_t wcap) {
  ge_extract_cell(row, rowlen, "from_div_45g45gg", from, fcap);
  ge_extract_cell(row, rowlen, "subj_div_45g45gg", subj, scap);
  ge_extract_cell(row, rowlen, "time_div_45g45gg", when, wcap);
  return from[0] || subj[0] || when[0];
}

/**
 * 创建 generator.email 临时邮箱
 * 解析首页 SITE_DATA 快照（cur_user/cur_domain）得到邮箱地址。
 */
tm_email_info_t *tm_provider_generator_email_generate(void) {
  char *page = ge_fetch_page();
  if (!page)
    return NULL;
  char *user = ge_field(page, "cur_user");
  char *domain = ge_field(page, "cur_domain");
  if (!user || !user[0] || !domain || !domain[0]) {
    TM_LOG_ERR("generator-email: 首页未携带邮箱快照（cur_user/cur_domain）");
    free(page);
    free(user);
    free(domain);
    return NULL;
  }

  size_t elen = strlen(user) + strlen(domain) + 2;
  char *email = (char *)malloc(elen);
  if (!email) {
    free(page);
    free(user);
    free(domain);
    return NULL;
  }
  snprintf(email, elen, "%s@%s", user, domain);

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    free(page);
    free(user);
    free(domain);
    free(email);
    return NULL;
  }
  info->channel = CHANNEL_GENERATOR_EMAIL;
  info->email = email;

  /* 会话凭据 JSON：{email, domain, user} */
  size_t tlen = strlen(email) + strlen(domain) + strlen(user) + 64;
  info->token = (char *)malloc(tlen);
  if (info->token) {
    snprintf(info->token, tlen,
             "{\"email\":\"%s\",\"domain\":\"%s\",\"user\":\"%s\"}", email,
             domain, user);
  }
  free(page);
  free(user);
  free(domain);
  return info;
}

/**
 * 读取 generator.email 收件箱
 * 解析收件箱渲染页的列表条目（from/subj/time 三要素）；本站不提供
 * 原文正文，SDK 按摘要归一。
 */
tm_email_t *tm_provider_generator_email_get_emails(const char *email,
                                                   const char *token,
                                                   int *count) {
  *count = 0;
  if (!token || !token[0])
    return NULL;
  if (!email || !email[0])
    return NULL;

  cJSON *sess = cJSON_Parse(token);
  if (!sess) {
    TM_LOG_ERR("generator-email: 会话凭据解析失败");
    return NULL;
  }
  const char *tok_email =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "email"), "");
  const char *tok_domain =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "domain"), "");
  if (!tok_email[0] || strcmp(tok_email, email) != 0) {
    TM_LOG_ERR("generator-email: 会话邮箱与查询邮箱不匹配");
    cJSON_Delete(sess);
    return NULL;
  }

  char *page = ge_fetch_page();
  if (!page) {
    cJSON_Delete(sess);
    return NULL;
  }

  /* 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换 */
  char *serv_domain = ge_field(page, "cur_domain");
  if (!serv_domain || strcmp(serv_domain, tok_domain) != 0) {
    TM_LOG_ERR("generator-email: 会话域名已切换（token %s，服务端 %s）",
               tok_domain, serv_domain ? serv_domain : "");
    free(page);
    free(serv_domain);
    cJSON_Delete(sess);
    return NULL;
  }
  free(serv_domain);

  /* 列表区域锚定（#email-table ... #markodile 之间） */
  const char *table = strstr(page, "id=\"email-table\"");
  const char *markodile = strstr(page, "id=\"markodile\"");
  size_t region_start = table ? (size_t)(table - page) : 0;
  size_t region_end = markodile ? (size_t)(markodile - page) : strlen(page);

  struct ge_row {
    char from[GE_CELL_CAP];
    char subj[GE_CELL_CAP];
    char when[256];
  };
  struct ge_row rows[GE_MAX_MAILS];
  int n_rows = 0;

  /* 逐行匹配 class 含 list-group-item2 的条目（平台实测真实类名） */
  const char *p = page;
  size_t cls_len = strlen("list-group-item2");
  while (p && (p = strstr(p, "list-group-item2")) != NULL) {
    size_t pos = (size_t)(p - page);
    /* 条目块起点：回溯到最近的 '<' */
    const char *row_start = p;
    while (row_start > page && row_start[-1] != '<')
      row_start--;
    /* 块内容：跳过起始标签后取整个条目 div 组；保守取从行首起 4096 字节 */
    const char *content_start = row_start;
    size_t row_cap = strlen(page) - (size_t)(content_start - page);
    if (row_cap > 4096)
      row_cap = 4096;

    if (n_rows < GE_MAX_MAILS) {
      if (ge_parse_row(content_start, row_cap, rows[n_rows].from,
                       sizeof(rows[n_rows].from), rows[n_rows].subj,
                       sizeof(rows[n_rows].subj), rows[n_rows].when,
                       sizeof(rows[n_rows].when))) {
        /* 在列表区域内的候选才采纳 */
        if (pos >= region_start && pos <= region_end) {
          n_rows++;
        }
      }
    }
    p += cls_len;
  }

  tm_email_t *emails = tm_emails_new(n_rows > 0 ? n_rows : 1);
  if (!emails) {
    *count = -1;
    free(page);
    cJSON_Delete(sess);
    return NULL;
  }

  *count = n_rows;
  for (int i = 0; i < n_rows; i++) {
    cJSON *raw = cJSON_CreateObject();
    if (raw) {
      cJSON_AddStringToObject(raw, "from", rows[i].from);
      cJSON_AddStringToObject(raw, "to", email);
      cJSON_AddStringToObject(raw, "subject", rows[i].subj);
      cJSON_AddStringToObject(raw, "date", rows[i].when);
      emails[i] = tm_normalize_email(raw, email);
      cJSON_Delete(raw);
    } else {
      memset(&emails[i], 0, sizeof(emails[i]));
    }
  }

  free(page);
  cJSON_Delete(sess);
  return emails;
}