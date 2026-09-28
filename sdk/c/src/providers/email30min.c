/**
 * email30min 渠道 — https://30minemail.com
 *
 * 无认证 GET 轮询（全局 srand 由 client.c 负责）：
 *   - 建箱 GET /?generate 返回完整 HTML 页面，从 "@30minemail.com" 回溯
 *     本地名起点（空白/>/引号前），本地名 < 8 字符视为异常；
 *     token 复用完整地址（服务端以地址定位收件箱）。
 *   - 读信 GET /messages.php?email=<urlenc 完整地址>&_=<unix毫秒>，
 *     响应 {"ok":true,"expired":false,"count","emails":[{id,from,to,
 *     subject,date,html}]}；!ok 或 expired 整体报错；元素无 to 注入地址。
 */
#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define T30_BASE "https://30minemail.com"
#define T30_DOMAIN "30minemail.com"

static const char *t30_get_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* URL 编码（RFC3986，未保留字符原样） */
static char *t30_url_encode(const char *s) {
  static const char hex[] = "0123456789ABCDEF";
  size_t n = strlen(s);
  char *out = (char *)malloc(n * 3 + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    unsigned char c = (unsigned char)s[i];
    if ((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
        (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' ||
        c == '~') {
      out[o++] = (char)c;
    } else {
      out[o++] = '%';
      out[o++] = hex[(c >> 4) & 0x0F];
      out[o++] = hex[c & 0x0F];
    }
  }
  out[o] = '\0';
  return out;
}

/* 去首尾空白（原地修改，返回首地址） */
static char *t30_trim(char *s) {
  char *p = s;
  while (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')
    p++;
  size_t n = strlen(p);
  while (n > 0 && (p[n - 1] == ' ' || p[n - 1] == '\t' || p[n - 1] == '\n' ||
                   p[n - 1] == '\r'))
    p[--n] = '\0';
  return p;
}

/**
 * 创建 30minemail.com 临时邮箱
 * GET /?generate 返回 HTML，从页面解析本地名地址；token 复用完整地址。
 */
tm_email_info_t *tm_provider_email30min_generate(void) {
  char url[128];
  snprintf(url, sizeof(url), "%s/?generate", T30_BASE);
  const char *html_headers[] = {
      "Accept: text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
      "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
      "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 "
      "Safari/537.36",
      NULL};

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, html_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300 || !resp->body) {
    TM_LOG_ERR("30minemail: 创建邮箱失败 http %ld",
               resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }

  char needle[64];
  snprintf(needle, sizeof(needle), "@%s", T30_DOMAIN);
  char *hit = strstr(resp->body, needle);
  if (!hit) {
    TM_LOG_ERR("30minemail: 创建页面未找到邮箱地址");
    tm_http_response_free(resp);
    return NULL;
  }

  /* 向前回溯本地名起点：空白或 > 或引号之后 */
  char *start = hit;
  while (start > resp->body) {
    char c = start[-1];
    if (c == ' ' || c == '\n' || c == '\t' || c == '>' || c == '"')
      break;
    start--;
  }
  size_t local_len = (size_t)(hit - start);
  if (local_len < 8 || local_len > 64) {
    TM_LOG_ERR("30minemail: 创建页面解析地址异常");
    tm_http_response_free(resp);
    return NULL;
  }
  char local[80];
  memcpy(local, start, local_len);
  local[local_len] = '\0';
  char *lt = t30_trim(local);
  if (strlen(lt) < 8) {
    TM_LOG_ERR("30minemail: 创建页面解析地址异常");
    tm_http_response_free(resp);
    return NULL;
  }

  char email[128];
  snprintf(email, sizeof(email), "%s@%s", lt, T30_DOMAIN);
  tm_http_response_free(resp);

  tm_email_info_t *info = tm_email_info_new();
  if (!info)
    return NULL;
  info->channel = CHANNEL_EMAIL30MIN;
  info->email = tm_strdup(email);
  info->token = tm_strdup(email);
  return info;
}

/**
 * 读取 30minemail.com 收件箱
 * GET /messages.php?email=<urlenc>&_=<unix毫秒>；!ok 或 expired 整体报错，
 * 邮件元素无 to 字段时注入当前地址。
 */
tm_email_t *tm_provider_email30min_get_emails(const char *email,
                                              const char *token, int *count) {
  *count = 0;
  (void)token;
  if (!email || !email[0])
    return NULL;

  char *enc = t30_url_encode(email);
  if (!enc)
    return NULL;
  char url[640];
  snprintf(url, sizeof(url), "%s/messages.php?email=%s&_=%lld", T30_BASE, enc,
           (long long)(time(NULL) * 1000LL));
  free(enc);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, t30_get_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("30minemail: 读取收件箱失败 http %ld",
               resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("30minemail: 解析收件箱响应失败");
    return NULL;
  }

  int ok = 0;
  {
    const cJSON *ov = cJSON_GetObjectItemCaseSensitive(root, "ok");
    if (cJSON_IsTrue(ov) || (cJSON_IsNumber(ov) && ov->valueint != 0))
      ok = 1;
  }
  int expired = 0;
  {
    const cJSON *ev = cJSON_GetObjectItemCaseSensitive(root, "expired");
    if (cJSON_IsTrue(ev) || (cJSON_IsNumber(ev) && ev->valueint != 0))
      expired = 1;
  }
  if (!ok || expired) {
    TM_LOG_ERR("30minemail: 收件箱不可用或已过期");
    cJSON_Delete(root);
    return NULL;
  }

  cJSON *emails_arr = cJSON_GetObjectItemCaseSensitive(root, "emails");
  if (!cJSON_IsArray(emails_arr) || cJSON_GetArraySize(emails_arr) == 0) {
    cJSON_Delete(root);
    return NULL;
  }

  int n = cJSON_GetArraySize(emails_arr);
  *count = n;
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(root);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *msg = cJSON_GetArrayItem(emails_arr, i);
    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;

    /* 无 to 字段时注入收件人地址（normalize 候选已含 html 等） */
    if (cJSON_GetObjectItemCaseSensitive(msg, "to") == NULL)
      cJSON_AddStringToObject(raw, "to", email);

    /* 逐字段透传（id/from/to/subject/date/html） */
    const char *keys[] = {"id",   "from",    "to",      "subject",
                          "date", "html",    "text",    "body"};
    for (int k = 0; k < 8; k++) {
      const cJSON *v = cJSON_GetObjectItemCaseSensitive(msg, keys[k]);
      if (cJSON_IsString(v) && v->valuestring && v->valuestring[0])
        cJSON_AddStringToObject(raw, keys[k], v->valuestring);
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}