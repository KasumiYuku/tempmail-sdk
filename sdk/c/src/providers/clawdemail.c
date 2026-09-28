/**
 * clawdemail 渠道 — https://api.clawdemail.com
 *
 * Bearer 认证（全局 srand 由 client.c 负责）：
 *   - 建箱 POST /register body {"name":""} → {"success","email","token"}。
 *   - 读信 GET /inbox?limit=50（Authorization: Bearer <token>）→
 *     {"success","email","count","unread","emails":[]}；success 非真报 error。
 *   - 每封 GET /email/<id>（Bearer）取详情；详情含 email 嵌套对象
 *     （from_addr/subject/body_text/received_at/read）时提升该嵌套对象，
 *     缺字段才合并进列表项，详情失败用列表摘要。
 */
#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define CLAWD_BASE "https://api.clawdemail.com"

static const char *clawd_post_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* 组配带 Bearer 的 GET 头数组（动态，调用方 free 数组与 auth 槽） */
static char **clawd_auth_headers(const char *token) {
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
static void clawd_free_headers(char **h) {
  if (!h)
    return;
  free(h[2]);
  free(h);
}

/* 从列表/详情元素提取邮件 ID（候选 id/messageId） */
static const char *clawd_message_id(cJSON *msg) {
  const char *keys[] = {"id", "messageId"};
  for (int i = 0; i < 2; i++) {
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(msg, keys[i]);
    if (cJSON_IsString(v) && v->valuestring && v->valuestring[0])
      return v->valuestring;
  }
  return NULL;
}

/* 拉取单封详情（GET /email/{id} Bearer）；含 email 嵌套对象时提升之。
 * 失败返回 NULL */
static cJSON *clawd_fetch_detail(const char *token, const char *msg_id) {
  size_t need = strlen(CLAWD_BASE) + strlen(msg_id) + 32;
  char *url = (char *)malloc(need);
  if (!url)
    return NULL;
  snprintf(url, need, "%s/email/%s", CLAWD_BASE, msg_id);

  char **h = clawd_auth_headers(token);
  if (!h) {
    free(url);
    return NULL;
  }
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  free(url);
  clawd_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *detail = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!detail)
    return NULL;

  /* 详情为 {success,email:{...},code,links} 时提升嵌套 email 对象 */
  cJSON *nested = cJSON_GetObjectItemCaseSensitive(detail, "email");
  if (cJSON_IsObject(nested)) {
    /* 提升：拷贝嵌套对象（释放原根） */
    cJSON *promoted = cJSON_Duplicate(nested, 1);
    cJSON_Delete(detail);
    return promoted;
  }
  return detail;
}

/**
 * 创建 clawdemail.com 临时邮箱
 * POST /register body {"name":""} → {success,email,token}。
 */
tm_email_info_t *tm_provider_clawdemail_generate(void) {
  char url[128];
  snprintf(url, sizeof(url), "%s/register", CLAWD_BASE);

  tm_http_response_t *resp = tm_http_request(TM_HTTP_POST, url,
                                             clawd_post_headers,
                                             "{\"name\":\"\"}", 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("clawdemail: 建箱失败 http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("clawdemail: 解析建箱响应失败");
    return NULL;
  }
  const char *email =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "email"), "");
  const char *token =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "token"), "");
  if (!email[0] || !token[0] || !strchr(email, '@')) {
    TM_LOG_ERR("clawdemail: 建箱响应缺少必要字段");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_CLAWDEMAIL;
  info->email = tm_strdup(email);
  info->token = tm_strdup(token);
  cJSON_Delete(root);
  return info;
}

/**
 * 读取 clawdemail.com 邮件列表
 * GET /inbox?limit=50（Bearer）→ {"success","emails":[...]}；success 非真
 * 报 error；逐封 GET /email/{id} 合并详情（提升嵌套 email 对象，
 * 缺字段才覆盖），详情失败回退列表摘要。
 */
tm_email_t *tm_provider_clawdemail_get_emails(const char *email,
                                              const char *token, int *count) {
  *count = 0;
  if (!email || !email[0] || !token || !token[0])
    return NULL;

  char url[128];
  snprintf(url, sizeof(url), "%s/inbox?limit=50", CLAWD_BASE);
  char **h = clawd_auth_headers(token);
  if (!h)
    return NULL;
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  clawd_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("clawdemail: 获取邮件列表失败 http %ld",
               resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("clawdemail: 解析邮件列表失败");
    return NULL;
  }

  {
    const cJSON *success = cJSON_GetObjectItemCaseSensitive(root, "success");
    if (!cJSON_IsTrue(success)) {
      TM_LOG_ERR("clawdemail: 读取收件箱失败: %s",
                 TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "error"),
                             ""));
      cJSON_Delete(root);
      return NULL;
    }
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
    /* 收件人注入（to 缺失时 normalize 自动落 recipient） */
    if (cJSON_GetObjectItemCaseSensitive(msg, "to") == NULL)
      cJSON_AddStringToObject(msg, "to", email);

    const char *msg_id = clawd_message_id(msg);
    if (msg_id) {
      cJSON *detail = clawd_fetch_detail(token, msg_id);
      if (detail) {
        /* 详情键只补列表项缺失者（from_addr/body_text 等命中
         * normalize 的 from/text 候选） */
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