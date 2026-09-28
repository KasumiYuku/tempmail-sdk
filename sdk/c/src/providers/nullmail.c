/**
 * nullmail 渠道 — https://www.nullmail.cc（域 maildock.store）
 *
 * 无认证 REST：POST /api/emails（空 JSON body）建箱，响应
 * {"address":"...@maildock.store","expiry":"..."}；
 * 读信 GET /api/emails/{address}（URL 编码），响应 {"expiry":"...",
 * "emails":[...]}，列表项只有 id/sender/subject/delivered，正文须逐封
 * 二拉 GET /api/emails/{addr}/body/{id}（响应 {"body":...}），
 * 失败降级留空不阻断列表。
 * 请求须携带 Origin/Referer（官网同站 fetch 行为）。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define NULLMAIL_BASE "https://www.nullmail.cc"

static const char *nullmail_json_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "Origin: " NULLMAIL_BASE,
    "Referer: " NULLMAIL_BASE "/",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *nullmail_get_headers[] = {
    "Accept: application/json",
    "Origin: " NULLMAIL_BASE,
    "Referer: " NULLMAIL_BASE "/",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* URL 编码（RFC3986 未保留字符原样保留；@ 编码为 %40） */
static int nullmail_encode_char(char c, char *out) {
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

static char *nullmail_encode(const char *s) {
  if (!s)
    return NULL;
  size_t len = strlen(s);
  char *out = (char *)malloc(len * 3 + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < len; i++) {
    o += (size_t)nullmail_encode_char(s[i], out + o);
  }
  out[o] = '\0';
  return out;
}

/**
 * 创建临时邮箱
 * POST /api/emails（空 JSON body），token 复用完整地址
 */
tm_email_info_t *tm_provider_nullmail_generate(void) {
  char url[128];
  snprintf(url, sizeof(url), "%s/api/emails", NULLMAIL_BASE);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, nullmail_json_headers, "{}", 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("nullmail: 建箱失败");
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("nullmail: 解析建箱响应失败");
    return NULL;
  }

  const char *address =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "address"), "");
  if (!address[0]) {
    TM_LOG_ERR("nullmail: 响应缺少 address 字段");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_NULLMAIL;
  info->email = tm_strdup(address);
  info->token = tm_strdup(address);
  cJSON_Delete(root);
  return info;
}

/* 单封正文二拉：GET /api/emails/{addr}/body/{id}，响应 {"body":...} */
static char *nullmail_get_body(const char *addr, const char *id) {
  char *enc_addr = nullmail_encode(addr);
  if (!enc_addr)
    return NULL;

  size_t need = strlen(enc_addr) + strlen(id) + 128;
  char *url = (char *)malloc(need);
  if (!url) {
    free(enc_addr);
    return NULL;
  }
  snprintf(url, need, "%s/api/emails/%s/body/%s", NULLMAIL_BASE, enc_addr, id);
  free(enc_addr);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, nullmail_get_headers, NULL, 15);
  free(url);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return NULL;

  char *body =
      tm_strdup(TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "body"), ""));
  cJSON_Delete(root);
  return body;
}

/**
 * 读取收件箱
 * GET /api/emails/{address} 列表（无正文），逐封二拉 body 端点取正文
 */
tm_email_t *tm_provider_nullmail_get_emails(const char *email,
                                            const char *token, int *count) {
  *count = 0;
  (void)token;
  if (!email || !email[0])
    return NULL;

  char *enc = nullmail_encode(email);
  if (!enc)
    return NULL;

  size_t need = strlen(enc) + 64;
  char *url = (char *)malloc(need);
  if (!url) {
    free(enc);
    return NULL;
  }
  snprintf(url, need, "%s/api/emails/%s", NULLMAIL_BASE, enc);
  free(enc);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, nullmail_get_headers, NULL, 15);
  free(url);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return NULL;

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

    const char *idv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "id"), "");
    if (idv[0])
      cJSON_AddStringToObject(raw, "id", idv);

    const char *senderv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "sender"), "");
    if (senderv[0])
      cJSON_AddStringToObject(raw, "from", senderv);

    cJSON_AddStringToObject(raw, "to", email);

    const char *subjv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "subject"), "");
    if (subjv[0])
      cJSON_AddStringToObject(raw, "subject", subjv);

    const char *deliv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "delivered"), "");
    if (deliv[0])
      cJSON_AddStringToObject(raw, "date", deliv);

    /* 逐封二拉正文，失败降级留空不阻断列表 */
    if (idv[0]) {
      char *body = nullmail_get_body(email, idv);
      if (body) {
        if (body[0])
          cJSON_AddStringToObject(raw, "text", body);
        free(body);
      }
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}