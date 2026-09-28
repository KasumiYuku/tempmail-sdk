/**
 * mtempmail 渠道 — https://mtempmail.com（公共 key 认证）
 *
 * 建箱: POST /api/emails/{apiKey}（body {}）→
 *   {"status":true,"data":{"email":"xxx@domain","email_token":"..."}}
 * 读信: GET /api/messages/{apiKey}/{email} →
 *   {"status":true,"mailbox":"..","email_token":"..","messages":[]}
 * 消息列表元素为 mailgun 入站 webhook 风格：
 *   from 为 [{"full":"Sender <a@b.com>"}]、body 为
 *   [{content_type:"text/html",value:".."}]、created_at 为时间串。
 * token 元数据仅为校验，不参与请求。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define MTEMPMAIL_BASE "https://mtempmail.com"
#define MTEMPMAIL_PUBLIC_KEY "pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw"

static const char *mtempmail_json_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *mtempmail_get_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* 清洗主题前导分隔符（后台可能以 "• " 开头拼接微件） */
static void mtempmail_clean_subject(const char *src, char *out, size_t cap) {
  while ((*src == ' ' || *src == '\t') && *src)
    src++;
  while ((src[0] & 0xFF) == 0xE2 && (src[1] & 0xFF) == 0x80 &&
         ((src[2] & 0xFF) == 0xA2 || (src[2] & 0xFF) == 0xB7)) {
    /* UTF-8 "•" 与 "·" 前缀剥离 */
    src += 3;
    while ((*src == ' ' || *src == '\t') && *src)
      src++;
  }
  snprintf(out, cap, "%s", src);
}

/* 拼接正文纯文本（body[].value 按序，换行分隔） */
static void mtempmail_body_text(const cJSON *body_arr, char *out, size_t cap) {
  out[0] = '\0';
  if (!cJSON_IsArray(body_arr))
    return;
  size_t used = 0;
  const cJSON *seg;
  cJSON_ArrayForEach(seg, body_arr) {
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(seg, "value");
    if (!cJSON_IsString(v) || !v->valuestring)
      continue;
    size_t len = strlen(v->valuestring);
    if (used + len + 2 >= cap)
      break;
    memcpy(out + used, v->valuestring, len);
    used += len;
    out[used++] = '\n';
  }
  /* 去尾部换行 */
  while (used > 0 && out[used - 1] == '\n')
    used--;
  out[used] = '\0';
}

/* 提取首个 text/html 段 */
static void mtempmail_body_html(const cJSON *body_arr, char *out, size_t cap) {
  out[0] = '\0';
  if (!cJSON_IsArray(body_arr))
    return;
  const cJSON *seg;
  cJSON_ArrayForEach(seg, body_arr) {
    const char *ct =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(seg, "content_type"), "");
    if (strcmp(ct, "text/html") != 0)
      continue;
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(seg, "value");
    if (cJSON_IsString(v) && v->valuestring) {
      snprintf(out, cap, "%s", v->valuestring);
      return;
    }
  }
}

/**
 * 创建 mtempmail 临时邮箱
 * POST /api/emails/{apiKey}（空 JSON body）
 */
tm_email_info_t *tm_provider_mtempmail_generate(void) {
  char url[256];
  snprintf(url, sizeof(url), "%s/api/emails/%s", MTEMPMAIL_BASE,
           MTEMPMAIL_PUBLIC_KEY);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, mtempmail_json_headers, "{}", 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("mtempmail: 建箱失败");
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("mtempmail: 解析建箱响应失败");
    return NULL;
  }

  cJSON *status = cJSON_GetObjectItemCaseSensitive(root, "status");
  cJSON *data = cJSON_GetObjectItemCaseSensitive(root, "data");
  const char *email = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(data, "email"),
                                  "");
  const char *email_token =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(data, "email_token"), "");

  if (!cJSON_IsTrue(status) || !email[0]) {
    TM_LOG_ERR("mtempmail: 响应缺少邮箱");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_MTEMPMAIL;
  info->email = tm_strdup(email);
  info->token = tm_strdup(email_token);
  cJSON_Delete(root);
  return info;
}

/**
 * 读取 mtempmail 收件箱
 * GET /api/messages/{apiKey}/{email}；token 元数据仅为校验，不参与请求
 */
tm_email_t *tm_provider_mtempmail_get_emails(const char *email,
                                             const char *token, int *count) {
  *count = 0;
  if (!token || !token[0])
    return NULL;
  if (!email || !email[0])
    return NULL;

  size_t need = strlen(email) + 128;
  char *url = (char *)malloc(need);
  if (!url)
    return NULL;
  snprintf(url, need, "%s/api/messages/%s/%s", MTEMPMAIL_BASE,
           MTEMPMAIL_PUBLIC_KEY, email);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, mtempmail_get_headers, NULL, 15);
  free(url);
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

    const char *idv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "id"), "");
    if (idv[0])
      cJSON_AddStringToObject(raw, "id", idv);

    /* 收件人固定为当前邮箱 */
    cJSON_AddStringToObject(raw, "to", email);

    /* from 为数组：[{"full":"Sender <a@b.com>"},...] */
    cJSON *from_arr = cJSON_GetObjectItemCaseSensitive(msg, "from");
    if (cJSON_IsArray(from_arr) && cJSON_GetArraySize(from_arr) > 0) {
      const cJSON *first = cJSON_GetArrayItem(from_arr, 0);
      const char *full =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(first, "full"), "");
      if (full[0])
        cJSON_AddStringToObject(raw, "from", full);
    } else {
      const char *fromv =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "from"), "");
      if (fromv[0])
        cJSON_AddStringToObject(raw, "from", fromv);
    }

    /* 主题清洗前导分隔符 */
    const char *subjv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "subject"), "");
    if (subjv[0]) {
      char cleaned[1024];
      mtempmail_clean_subject(subjv, cleaned, sizeof(cleaned));
      cJSON_AddStringToObject(raw, "subject", cleaned);
    }

    /* body 分段：拼接纯文本 / 提取 text/html */
    cJSON *body_arr = cJSON_GetObjectItemCaseSensitive(msg, "body");
    if (cJSON_IsArray(body_arr)) {
      char buf[64 * 1024];
      mtempmail_body_text(body_arr, buf, sizeof(buf));
      if (buf[0])
        cJSON_AddStringToObject(raw, "text", buf);
      mtempmail_body_html(body_arr, buf, sizeof(buf));
      if (buf[0])
        cJSON_AddStringToObject(raw, "html", buf);
    } else {
      const char *textv =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "text"), "");
      if (textv[0])
        cJSON_AddStringToObject(raw, "text", textv);
      const char *htmlv =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "html"), "");
      if (htmlv[0])
        cJSON_AddStringToObject(raw, "html", htmlv);
    }

    const char *datev =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "created_at"), "");
    if (datev[0])
      cJSON_AddStringToObject(raw, "date", datev);

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}