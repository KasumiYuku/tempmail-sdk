/**
 * crazymailing 渠道 — https://crazymailing.com
 *
 * Next.js 全栈站点（全局 srand 由 client.c 负责）：
 *   - 建箱 POST /api/mailbox（空 JSON body {} + Content-Type application/json）
 *     → {"mailbox":{"id","address","expiresAt"}}，token=id，
 *     ExpiresAt 原样字符串。
 *   - 读信 GET /api/messages?mailbox=<urlenc 完整地址> → {"messages":[...]}；
 *     每封 GET /api/message/{id}/body 拉正文 HTML（失败兜底列表摘要）；
 *     元素无 to 注入当前地址。
 *   - 通用头：Accept + Origin: 基址 + Referer: 基址/ + UA。
 */
#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define CRAZY_BASE "https://crazymailing.com"

static const char *crazy_json_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "Origin: " CRAZY_BASE,
    "Referer: " CRAZY_BASE "/",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *crazy_get_headers[] = {
    "Accept: application/json",
    "Origin: " CRAZY_BASE,
    "Referer: " CRAZY_BASE "/",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* URL 编码（RFC3986，未保留字符原样） */
static char *crazy_url_encode(const char *s) {
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

/**
 * 创建 crazymailing.com 临时邮箱
 * POST /api/mailbox（空 JSON body），token=mailbox.id，
 * expires_at 保留平台原样字符串（RFC3339）。
 */
tm_email_info_t *tm_provider_crazymailing_generate(void) {
  char url[128];
  snprintf(url, sizeof(url), "%s/api/mailbox", CRAZY_BASE);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, crazy_json_headers, "{}", 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("crazymailing: 建箱失败 http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("crazymailing: 解析建箱响应失败");
    return NULL;
  }

  cJSON *mailbox = cJSON_GetObjectItemCaseSensitive(root, "mailbox");
  const char *address =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(mailbox, "address"), "");
  const char *id =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(mailbox, "id"), "");
  const char *expires =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(mailbox, "expiresAt"), "");
  if (!address[0]) {
    TM_LOG_ERR("crazymailing: 建箱响应缺少 mailbox.address");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_CRAZYMAILING;
  info->email = tm_strdup(address);
  info->token = tm_strdup(id);
  info->created_at = tm_strdup(expires); /* ExpiresAt 原样字符串 */
  cJSON_Delete(root);
  return info;
}

/* 拉取单封正文（GET /api/message/{id}/body，完整 HTML 页面）；失败返回 NULL */
static char *crazy_fetch_body(const char *id) {
  size_t need = strlen(id) + 64;
  char *url = (char *)malloc(need);
  if (!url)
    return NULL;
  snprintf(url, need, "%s/api/message/%s/body", CRAZY_BASE, id);
  const char *headers[] = {
      "Accept: text/html,application/xhtml+xml,*/*;q=0.8",
      "Origin: " CRAZY_BASE,
      "Referer: " CRAZY_BASE "/",
      "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
      "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 "
      "Safari/537.36",
      NULL};
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, headers, NULL, 15);
  free(url);
  if (!resp || resp->status < 200 || resp->status >= 300 || !resp->body) {
    tm_http_response_free(resp);
    return NULL;
  }
  char *body = tm_strdup(resp->body);
  tm_http_response_free(resp);
  return body;
}

/**
 * 读取 crazymailing.com 收件箱
 * GET /api/messages?mailbox=<urlenc>；逐封拉正文 HTML（失败兜底），
 * 元素无 to 注入当前地址。
 */
tm_email_t *tm_provider_crazymailing_get_emails(const char *email,
                                                const char *token,
                                                int *count) {
  *count = 0;
  (void)token;
  if (!email || !email[0])
    return NULL;

  char *enc = crazy_url_encode(email);
  if (!enc)
    return NULL;
  char url[640];
  snprintf(url, sizeof(url), "%s/api/messages?mailbox=%s", CRAZY_BASE, enc);
  free(enc);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, crazy_get_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("crazymailing: 读取收件箱失败 http %ld",
               resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("crazymailing: 解析收件箱响应失败");
    return NULL;
  }

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
    if (!raw)
      continue;

    /* 注入收件人地址（to 缺失时保底） */
    cJSON_AddStringToObject(raw, "to", email);

    /* 列表元数据透传（id/from/fromName/subject/preview/receivedAt/seen） */
    const char *keys[] = {"id", "from", "fromName", "subject", "preview",
                          "receivedAt", "seen"};
    for (int k = 0; k < 7; k++) {
      const cJSON *v = cJSON_GetObjectItemCaseSensitive(msg, keys[k]);
      if (cJSON_IsString(v) && v->valuestring && v->valuestring[0])
        cJSON_AddStringToObject(raw, keys[k], v->valuestring);
    }

    /* 逐封二拉正文：成功且非空则覆盖 html（失败不阻断） */
    const char *idv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "id"), "");
    if (idv[0]) {
      char *html = crazy_fetch_body(idv);
      if (html && html[0])
        cJSON_AddStringToObject(raw, "html", html);
      free(html);
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}