/**
 * nowtempmail 渠道 — https://nowtempmail.com
 *
 * JWT Bearer 认证（全局 srand 由 client.c 负责）：
 *   - 建箱 POST /mailbox（无 body，Content-Type application/json）→
 *     {token(JWT), mailbox}。
 *   - 读信 GET /messages 带 Authorization: Bearer <token> →
 *     {"messages":[...]}；每封 GET /message/<id>（Bearer）合并详情
 *     （缺字段才覆盖），详情失败兜底列表摘要。
 *   - 列表元素按多候选字段归一（from 可为 "Name <email>" 或裸地址）。
 */
#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define NOWTEMP_BASE "https://nowtempmail.com"

static const char *nowtemp_post_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* 组配带 Bearer 的 GET 头数组（动态，调用方 free 数组与 auth 槽） */
static char **nowtemp_auth_headers(const char *token) {
  const char **h = (const char **)calloc(4, sizeof(char *));
  if (!h)
    return NULL;
  h[0] = "Accept: application/json";
  h[1] =
      "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
      "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 "
      "Safari/537.36";
  size_t len = strlen(token) + 32;
  char *auth = (char *)malloc(len);
  if (!auth) {
    free(h);
    return NULL;
  }
  snprintf(auth, len, "Authorization: Bearer %s", token);
  h[2] = auth;
  h[3] = NULL;
  return (char **)h;
}

/* 释放 auth_headers */
static void nowtemp_free_headers(char **h) {
  if (!h)
    return;
  free(h[2]);
  free(h);
}

/* 从列表/详情元素提取邮件 ID（候选 id/Id/slug/messageId/message_id） */
static const char *nowtemp_message_id(cJSON *msg) {
  const char *keys[] = {"id", "Id", "slug", "messageId", "message_id"};
  for (int i = 0; i < 5; i++) {
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(msg, keys[i]);
    if (cJSON_IsString(v) && v->valuestring && v->valuestring[0])
      return v->valuestring;
  }
  return NULL;
}

/* 拉取单封详情（GET /message/{id} Bearer）；失败返回 NULL */
static cJSON *nowtemp_fetch_detail(const char *token, const char *msg_id) {
  size_t need = strlen(NOWTEMP_BASE) + strlen(msg_id) + 32;
  char *url = (char *)malloc(need);
  if (!url)
    return NULL;
  snprintf(url, need, "%s/message/%s", NOWTEMP_BASE, msg_id);

  char **h = nowtemp_auth_headers(token);
  if (!h) {
    free(url);
    return NULL;
  }
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  free(url);
  nowtemp_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *detail = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  return detail;
}

/**
 * 创建 nowtempmail.com 临时邮箱
 * POST /mailbox（空 body）→ {token(JWT), mailbox}。
 */
tm_email_info_t *tm_provider_nowtempmail_generate(void) {
  char url[128];
  snprintf(url, sizeof(url), "%s/mailbox", NOWTEMP_BASE);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, nowtemp_post_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("nowtempmail: 建箱失败 http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("nowtempmail: 解析建箱响应失败");
    return NULL;
  }
  const char *token =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "token"), "");
  const char *mailbox =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "mailbox"), "");
  if (!token[0] || !mailbox[0] || !strchr(mailbox, '@')) {
    TM_LOG_ERR("nowtempmail: 建箱响应缺少必要字段");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_NOWTEMPMAIL;
  info->email = tm_strdup(mailbox);
  info->token = tm_strdup(token);
  cJSON_Delete(root);
  return info;
}

/**
 * 读取 nowtempmail.com 邮件列表
 * GET /messages（Bearer）→ {"messages":[...]}；逐封 GET /message/{id}
 * 合并详情（详情键仅补列表项缺失者）；详情失败回退列表摘要。
 */
tm_email_t *tm_provider_nowtempmail_get_emails(const char *email,
                                               const char *token,
                                               int *count) {
  *count = 0;
  if (!email || !email[0] || !token || !token[0])
    return NULL;

  char url[128];
  snprintf(url, sizeof(url), "%s/messages", NOWTEMP_BASE);
  char **h = nowtemp_auth_headers(token);
  if (!h)
    return NULL;
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  nowtemp_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("nowtempmail: 获取邮件列表失败 http %ld",
               resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("nowtempmail: 解析邮件列表失败");
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
    /* 收件人注入（to 缺失时 normalize 自动落 recipient） */
    if (cJSON_GetObjectItemCaseSensitive(msg, "to") == NULL)
      cJSON_AddStringToObject(msg, "to", email);

    const char *msg_id = nowtemp_message_id(msg);
    if (msg_id) {
      cJSON *detail = nowtemp_fetch_detail(token, msg_id);
      if (detail) {
        /* 详情键只补列表项缺失者 */
        cJSON *child = NULL;
        cJSON_ArrayForEach(child, detail) {
          const char *key = child->string;
          if (!key)
            continue;
          if (cJSON_GetObjectItemCaseSensitive(msg, key) == NULL)
            cJSON_AddItemReferenceToObject(msg, key, child);
        }
        cJSON_Delete(detail);
      }
    }

    emails[i] = tm_normalize_email(msg, email);
  }

  cJSON_Delete(root);
  return emails;
}