/**
 * tempmails-io 渠道 — https://tempmails.io
 *
 * 无认证 REST：POST /api/temp-mail/generate 建箱（响应 email/token/expires_at），
 * GET /api/temp-mail/inbox/{token} 读信（messages[] 含
 * from_email/text_body/html_body/attachments）。
 * 邮箱借用 uberip.com 等公共域（10 分钟自动过期）。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define TEMPMAILS_IO_BASE "https://tempmails.io"

static const char *tempmails_io_json_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *tempmails_io_get_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/**
 * 创建 10 分钟临时邮箱
 * POST /api/temp-mail/generate（空 body）
 */
tm_email_info_t *tm_provider_tempmails_io_generate(void) {
  char url[128];
  snprintf(url, sizeof(url), "%s/api/temp-mail/generate", TEMPMAILS_IO_BASE);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, tempmails_io_json_headers, "{}", 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("tempmails-io: 建箱失败");
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("tempmails-io: 解析建箱响应失败");
    return NULL;
  }

  cJSON *success = cJSON_GetObjectItemCaseSensitive(root, "success");
  cJSON *data = cJSON_GetObjectItemCaseSensitive(root, "data");
  const char *email = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(data, "email"), "");
  const char *token = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(data, "token"), "");

  if (!cJSON_IsTrue(success) || !email[0] || !token[0]) {
    TM_LOG_ERR("tempmails-io: 响应缺少 email 或 token");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_TEMPMAILS_IO;
  info->email = tm_strdup(email);
  info->token = tm_strdup(token);
  cJSON_Delete(root);
  return info;
}

/**
 * 获取收件箱邮件列表
 * 分两步：POST /api/temp-mail/fetch-emails/{token} 触发上游同步（失败不致命），
 * 再 GET /api/temp-mail/inbox/{token} 读取收件箱。
 */
tm_email_t *tm_provider_tempmails_io_get_emails(const char *email,
                                                const char *token, int *count) {
  *count = 0;
  if (!token || !token[0])
    return NULL;
  if (!email || !email[0])
    return NULL;

  /* 1) 触发同步（失败忽略） */
  char fetch_url[256];
  snprintf(fetch_url, sizeof(fetch_url), "%s/api/temp-mail/fetch-emails/%s",
           TEMPMAILS_IO_BASE, token);
  tm_http_response_t *fresp =
      tm_http_request(TM_HTTP_POST, fetch_url, tempmails_io_get_headers, NULL, 15);
  tm_http_response_free(fresp);

  /* 2) 读取收件箱 */
  char inbox_url[256];
  snprintf(inbox_url, sizeof(inbox_url), "%s/api/temp-mail/inbox/%s",
           TEMPMAILS_IO_BASE, token);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, inbox_url, tempmails_io_get_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return NULL;

  cJSON *data = cJSON_GetObjectItemCaseSensitive(root, "data");
  cJSON *messages = cJSON_GetObjectItemCaseSensitive(data, "messages");
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
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "from_email"), ""));
    cJSON_AddStringToObject(raw, "to", email);
    cJSON_AddStringToObject(
        raw, "text",
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "text_body"), ""));
    cJSON_AddStringToObject(
        raw, "html",
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "html_body"), ""));
    cJSON_AddStringToObject(
        raw, "date",
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "received_at"), ""));

    /* 附件透传归一化 */
    cJSON *attachs = cJSON_GetObjectItemCaseSensitive(msg, "attachments");
    if (cJSON_IsArray(attachs)) {
      cJSON_AddItemReferenceToObject(raw, "attachments", attachs);
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}