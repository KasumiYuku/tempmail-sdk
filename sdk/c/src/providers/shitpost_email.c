/**
 * shitpost-email 渠道 — https://shitpost.email（公共实例）
 *
 * 无认证 REST：POST /api/create 建箱（username/domain/ttl →
 * email/token/type/expires），GET /api/inbox?email=&token= 读信
 * （messages[] 含 from/fromName/subject/text/html/date）。
 * 域名池：shitpost.email / letsfuckingpiss.party（克隆自
 * shamu4life/throwaway-email 公共实例）。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define SHITPOST_EMAIL_BASE "https://shitpost.email"

static const char *shitpost_email_json_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *shitpost_email_get_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *shitpost_email_domains[] = {"shitpost.email",
                                                "letsfuckingpiss.party"};

/* 生成 "sdk"+10 位随机本地名 */
static void shitpost_email_local(char *out, size_t cap) {
  static const char chars[] = "abcdefghijklmnopqrstuvwxyz0123456789";
  size_t o = 0;
  memcpy(out, "sdk", 3);
  o += 3;
  for (int i = 0; i < 10 && o + 1 < cap; i++) {
    out[o++] = chars[rand() % (sizeof(chars) - 1)];
  }
  out[o] = '\0';
}

/* URL 编码（RFC3986 未保留字符原样保留） */
static int shitpost_email_encode_char(char c, char *out) {
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

static char *shitpost_email_encode(const char *s) {
  if (!s)
    return NULL;
  size_t len = strlen(s);
  char *out = (char *)malloc(len * 3 + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < len; i++) {
    o += (size_t)shitpost_email_encode_char(s[i], out + o);
  }
  out[o] = '\0';
  return out;
}

/**
 * 创建临时邮箱
 * POST /api/create body {username,domain,ttl}
 */
tm_email_info_t *tm_provider_shitpost_email_generate(void) {
  char local[32];
  shitpost_email_local(local, sizeof(local));
  const char *dom =
      shitpost_email_domains[rand() % (sizeof(shitpost_email_domains) /
                                        sizeof(shitpost_email_domains[0]))];

  char body[256];
  snprintf(body, sizeof(body),
           "{\"username\":\"%s\",\"domain\":\"%s\",\"ttl\":3600}", local, dom);

  char url[128];
  snprintf(url, sizeof(url), "%s/api/create", SHITPOST_EMAIL_BASE);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, shitpost_email_json_headers, body, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("shitpost-email: 建箱失败");
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("shitpost-email: 解析建箱响应失败");
    return NULL;
  }

  const char *email =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "email"), "");
  const char *token =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "token"), "");
  if (!email[0] || !token[0]) {
    TM_LOG_ERR("shitpost-email: 响应缺少 email 或 token");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_SHITPOST_EMAIL;
  info->email = tm_strdup(email);
  info->token = tm_strdup(token);
  cJSON_Delete(root);
  return info;
}

/**
 * 读取收件箱
 * GET /api/inbox?email=&token=
 */
tm_email_t *tm_provider_shitpost_email_get_emails(const char *email,
                                                  const char *token,
                                                  int *count) {
  *count = 0;
  if (!token || !token[0])
    return NULL;
  if (!email || !email[0])
    return NULL;

  char *enc_email = shitpost_email_encode(email);
  char *enc_token = shitpost_email_encode(token);
  if (!enc_email || !enc_token) {
    free(enc_email);
    free(enc_token);
    return NULL;
  }

  char url[512];
  snprintf(url, sizeof(url), "%s/api/inbox?email=%s&token=%s",
           SHITPOST_EMAIL_BASE, enc_email, enc_token);
  free(enc_email);
  free(enc_token);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, shitpost_email_get_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return NULL;

  cJSON *messages = cJSON_GetObjectItemCaseSensitive(root, "messages");
  if (!cJSON_IsArray(messages) || cJSON_GetArraySize(messages) == 0) {
    cJSON_Delete(root);
    return NULL;
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
    cJSON *msg = cJSON_GetArrayItem(messages, i);
    cJSON *raw = cJSON_CreateObject();
    cJSON_AddStringToObject(
        raw, "from",
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "from"), ""));
    cJSON_AddStringToObject(raw, "to", email);
    cJSON_AddStringToObject(
        raw, "subject",
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "subject"), ""));
    cJSON_AddStringToObject(
        raw, "text",
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "text"), ""));
    cJSON_AddStringToObject(
        raw, "html",
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "html"), ""));
    cJSON_AddStringToObject(
        raw, "date",
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "date"), ""));

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}