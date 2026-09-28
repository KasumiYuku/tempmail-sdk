/**
 * firetempmail 渠道 — https://firetempmail.com
 *
 * 无认证 REST：建箱无需请求，本地生成 随机词+0-999@<域>（域池与官网一致：
 * offrework.click / service-today.click / jobsdeforyou.sa.com）；
 * 读信 GET https://mail.firetempmail.com/mail/get?address=<邮箱 URL 编码>，
 * 必须携带 Header Origin: https://firetempmail.com（否则 403）。
 * 响应形如 {"status":"ok","code":200,"msg":"...","stats":{},"mails":[...]}，
 * 邮件字段以 sender/subject/date + content-html/content-text/content-plain
 * 多候选归一化。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define FIRETEMPMAIL_API_BASE "https://mail.firetempmail.com"
#define FIRETEMPMAIL_ORIGIN "https://firetempmail.com"

static const char *firetempmail_domains[] = {
    "offrework.click", "service-today.click", "jobsdeforyou.sa.com"};

static const char *firetempmail_headers[] = {
    "Accept: application/json",
    "Origin: " FIRETEMPMAIL_ORIGIN,
    "Referer: " FIRETEMPMAIL_ORIGIN "/",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* 多候选字段取字符串（非空即返回） */
static const char *firetempmail_pick(const cJSON *m, const char **keys,
                                     int n) {
  for (int i = 0; i < n; i++) {
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(m, keys[i]);
    if (cJSON_IsString(v) && v->valuestring && v->valuestring[0])
      return v->valuestring;
  }
  return NULL;
}

/* URL 编码（RFC3986 未保留字符原样保留） */
static int firetempmail_encode_char(char c, char *out) {
  static const char hex[] = "0123456789ABCDEF";
  unsigned char uc = (unsigned char)c;
  if ((uc >= 'A' && uc <= 'Z') || (uc >= 'a' && uc <= 'z') ||
      (uc >= '0' && uc <= '9') || uc == '-' || uc == '_' || uc == '.' ||
      uc == '~') {
    out[0] = c;
    return 1;
  }
  out[0] = '%';
  out[1] = hex[(uc >> 4) & 0x0F];
  out[2] = hex[uc & 0x0F];
  return 3;
}

static char *firetempmail_encode(const char *s) {
  if (!s)
    return NULL;
  size_t len = strlen(s);
  char *out = (char *)malloc(len * 3 + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < len; i++) {
    o += (size_t)firetempmail_encode_char(s[i], out + o);
  }
  out[o] = '\0';
  return out;
}

/* 随机小写单词（3-6 位）+ 0-999（与官网 faker 观感一致） */
static void firetempmail_local(char *out, size_t cap) {
  static const char chars[] = "abcdefghijklmnopqrstuvwxyz";
  int n = 3 + rand() % 4;
  if ((size_t)n + 1 >= cap)
    n = (int)cap - 2;
  for (int i = 0; i < n; i++)
    out[i] = chars[rand() % 26];
  out[n] = '\0';
}

/**
 * 创建临时邮箱
 * 建箱无需请求，本地生成 随机词+0-999@域名 形式，token 复用完整地址
 */
tm_email_info_t *tm_provider_firetempmail_generate(void) {
  const char *dom =
      firetempmail_domains[rand() % (sizeof(firetempmail_domains) /
                                      sizeof(firetempmail_domains[0]))];

  char local[16];
  firetempmail_local(local, sizeof(local));

  char email[128];
  snprintf(email, sizeof(email), "%s%d@%s", local, rand() % 1000, dom);

  tm_email_info_t *info = tm_email_info_new();
  if (!info)
    return NULL;
  info->channel = CHANNEL_FIRETEMPMAIL;
  info->email = tm_strdup(email);
  info->token = tm_strdup(email);
  return info;
}

/**
 * 读取收件箱
 * GET https://mail.firetempmail.com/mail/get?address=<URL 编码完整邮箱>
 * 必带 Origin 头，邮件字段按多候选归一化
 */
tm_email_t *tm_provider_firetempmail_get_emails(const char *email,
                                                const char *token,
                                                int *count) {
  *count = 0;
  (void)token;
  if (!email || !email[0])
    return NULL;

  char *enc = firetempmail_encode(email);
  if (!enc)
    return NULL;

  char url[512];
  snprintf(url, sizeof(url), "%s/mail/get?address=%s", FIRETEMPMAIL_API_BASE,
           enc);
  free(enc);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, firetempmail_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return NULL;

  /* 错误与成功共用同一信封：status 非空且非 ok 即失败 */
  const char *status =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "status"), "");
  if (status[0] && strcmp(status, "ok") != 0) {
    TM_LOG_ERR("firetempmail: %s",
               TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "msg"), ""));
    cJSON_Delete(root);
    return NULL;
  }

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

    /* 收件人固定为当前邮箱（官网 JSON 无统一 to 字段） */
    cJSON_AddStringToObject(raw, "to", email);

    const char *html_keys[] = {"content-html", "html"};
    const char *htmlv = firetempmail_pick(msg, html_keys, 2);
    if (htmlv)
      cJSON_AddStringToObject(raw, "html", htmlv);

    const char *text_keys[] = {"content-text", "content-plain", "text"};
    const char *textv = firetempmail_pick(msg, text_keys, 3);
    if (textv)
      cJSON_AddStringToObject(raw, "text", textv);

    const char *from_keys[] = {"sender", "from", "from_address"};
    const char *fromv = firetempmail_pick(msg, from_keys, 3);
    if (fromv)
      cJSON_AddStringToObject(raw, "from", fromv);

    const char *subj_keys[] = {"subject", "title"};
    const char *subjv = firetempmail_pick(msg, subj_keys, 2);
    if (subjv)
      cJSON_AddStringToObject(raw, "subject", subjv);

    const char *date_keys[] = {"date", "received_at", "created_at"};
    const char *datev = firetempmail_pick(msg, date_keys, 3);
    if (datev)
      cJSON_AddStringToObject(raw, "date", datev);

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}