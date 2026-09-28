/**
 * tempmailportal 渠道 — https://api.tempmailportal.com
 *
 * POST /api/v2/inbox 建箱（body {}，响应 address/token/private/expiresAt/
 * retentionMs，token 为 p2 前缀），GET /api/messages 读信（Header
 * Authorization: Bearer <token>），GET /api/messages/{id} 取单封详情
 * （Bearer）。详情失败时以列表摘要归一。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define TEMPMAILPORTAL_BASE "https://api.tempmailportal.com"

static const char *tempmailportal_json_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *tempmailportal_get_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/**
 * 创建 tempmailportal 临时邮箱
 * POST /api/v2/inbox（空 JSON body）返回 address 与 token
 */
tm_email_info_t *tm_provider_tempmailportal_generate(void) {
  char url[128];
  snprintf(url, sizeof(url), "%s/api/v2/inbox", TEMPMAILPORTAL_BASE);

  tm_http_response_t *resp = tm_http_request(
      TM_HTTP_POST, url, tempmailportal_json_headers, "{}", 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("tempmailportal: 建箱失败");
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("tempmailportal: 解析建箱响应失败");
    return NULL;
  }

  const char *address =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "address"), "");
  const char *token =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "token"), "");
  if (!address[0] || !token[0]) {
    TM_LOG_ERR("tempmailportal: 响应缺少 address 或 token");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_TEMPMAILPORTAL;
  info->email = tm_strdup(address);
  info->token = tm_strdup(token);
  cJSON_Delete(root);
  return info;
}

/* 从列表/详情元素提取邮件 ID，候选字段 id/Id/slug/messageId/message_id */
static const char *tempmailportal_message_id(cJSON *msg) {
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
static int tempmailportal_get_detail(const char *token, const char *msg_id,
                                     cJSON *list_item) {
  char url[512];
  snprintf(url, sizeof(url), "%s/api/messages/%s", TEMPMAILPORTAL_BASE, msg_id);

  char auth_hdr[512];
  snprintf(auth_hdr, sizeof(auth_hdr), "Authorization: Bearer %s", token);
  const char *headers[] = {auth_hdr, tempmailportal_get_headers[0],
                           tempmailportal_get_headers[1], NULL};

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
 * 获取邮件列表：列表 + 逐封详情合并，详情失败回退列表摘要
 */
tm_email_t *tm_provider_tempmailportal_get_emails(const char *email,
                                                  const char *token,
                                                  int *count) {
  *count = 0;
  if (!token || !token[0])
    return NULL;
  if (!email || !email[0])
    return NULL;

  char url[128];
  snprintf(url, sizeof(url), "%s/api/messages", TEMPMAILPORTAL_BASE);

  char auth_hdr[512];
  snprintf(auth_hdr, sizeof(auth_hdr), "Authorization: Bearer %s", token);
  const char *headers[] = {auth_hdr, tempmailportal_get_headers[0],
                           tempmailportal_get_headers[1], NULL};

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *list = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!cJSON_IsArray(list) || cJSON_GetArraySize(list) == 0) {
    cJSON_Delete(list);
    return NULL;
  }

  int n = cJSON_GetArraySize(list);
  *count = n;
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(list);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *msg = cJSON_GetArrayItem(list, i);
    const char *msg_id = tempmailportal_message_id(msg);
    if (msg_id) {
      tempmailportal_get_detail(token, msg_id, msg);
    }
    emails[i] = tm_normalize_email(msg, email);
  }

  cJSON_Delete(list);
  return emails;
}