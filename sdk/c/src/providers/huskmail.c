/**
 * huskmail 渠道 — https://huskmail.xyz
 *
 * POST /v1/accounts 建箱（body {}，响应 id/address/password/token/expiresAt/
 * tier，token 为 JWT），GET /v1/messages 读信（Header Authorization:
 * Bearer <token>，响应 {"messages":[...]}），GET /v1/messages/{id} 取
 * 单封详情（Bearer）。收信域固定为 @huskmail.xyz。
 * 注意：API 域为 api.huskmail.space。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define HUSKMAIL_BASE "https://api.huskmail.space"

static const char *huskmail_json_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *huskmail_get_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/**
 * 创建 huskmail（@huskmail.xyz）临时邮箱
 * POST /v1/accounts（空 JSON body）返回 address 与 JWT token
 */
tm_email_info_t *tm_provider_huskmail_generate(void) {
  char url[128];
  snprintf(url, sizeof(url), "%s/v1/accounts", HUSKMAIL_BASE);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, huskmail_json_headers, "{}", 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("huskmail: 建箱失败");
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("huskmail: 解析建箱响应失败");
    return NULL;
  }

  const char *address =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "address"), "");
  const char *token =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "token"), "");
  if (!address[0] || !token[0]) {
    TM_LOG_ERR("huskmail: 响应缺少 address 或 token");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_HUSKMAIL;
  info->email = tm_strdup(address);
  info->token = tm_strdup(token);
  cJSON_Delete(root);
  return info;
}

/* 从列表/详情元素提取邮件 ID，候选字段 id/Id/slug/messageId/message_id */
static const char *huskmail_message_id(cJSON *msg) {
  const char *keys[] = {"id", "Id", "slug", "messageId", "message_id"};
  for (int i = 0; i < 5; i++) {
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(msg, keys[i]);
    if (cJSON_IsString(v) && v->valuestring && v->valuestring[0]) {
      return v->valuestring;
    }
  }
  return NULL;
}

/* 获取单封详情：详情对象缺失键时合并进列表元素，失败返回 -1 */
static int huskmail_get_detail(const char *token, const char *msg_id,
                               cJSON *list_item) {
  char url[512];
  snprintf(url, sizeof(url), "%s/v1/messages/%s", HUSKMAIL_BASE, msg_id);

  char auth_hdr[512];
  snprintf(auth_hdr, sizeof(auth_hdr), "Authorization: Bearer %s", token);
  const char *headers[] = {auth_hdr, huskmail_get_headers[0],
                           huskmail_get_headers[1], NULL};

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return -1;
  }

  cJSON *detail = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!detail)
    return -1;

  cJSON *child = NULL;
  cJSON_ArrayForEach(child, detail) {
    const char *key = child->string;
    if (!key)
      continue;
    if (cJSON_GetObjectItemCaseSensitive(list_item, key) == NULL) {
      cJSON_AddItemReferenceToObject(list_item, key, child);
    }
  }
  cJSON_Delete(detail);
  return 0;
}

/**
 * 获取邮件列表：{"messages":[...]} 列表 + 逐封详情合并，详情失败回退列表摘要
 */
tm_email_t *tm_provider_huskmail_get_emails(const char *email,
                                            const char *token, int *count) {
  *count = 0;
  if (!token || !token[0])
    return NULL;
  if (!email || !email[0])
    return NULL;

  char url[128];
  snprintf(url, sizeof(url), "%s/v1/messages", HUSKMAIL_BASE);

  char auth_hdr[512];
  snprintf(auth_hdr, sizeof(auth_hdr), "Authorization: Bearer %s", token);
  const char *headers[] = {auth_hdr, huskmail_get_headers[0],
                           huskmail_get_headers[1], NULL};

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, headers, NULL, 15);
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
    const char *msg_id = huskmail_message_id(msg);
    if (msg_id) {
      huskmail_get_detail(token, msg_id, msg);
    }
    emails[i] = tm_normalize_email(msg, email);
  }

  cJSON_Delete(root);
  return emails;
}