/**
 * tenmin-app 渠道 — https://tenmin.app（真实 API 域 api.tenmin.app）
 *
 * 建箱+读信共用 GET /api/inbox/{localpart}（Accept: application/json）。
 * localpart 为随机 6 位小写十六进制串（首访即建箱，无显式创建接口）；
 * 响应：{"inboxId":..,"address":"..@tenmin.app","ttl":600,"count":0,
 * "messages":[]}，messages[] 元素字段：id/from/subject/text/html/
 * receivedAt（from 为 {name,address} 对象）。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define TENMIN_APP_BASE "https://api.tenmin.app"

static const char *tenmin_app_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* 生成 6 位小写十六进制随机 localpart */
static void tenmin_app_local(char *out, size_t cap) {
  static const char hex[] = "0123456789abcdef";
  for (int i = 0; i < 6 && (size_t)(i + 1) < cap; i++) {
    out[i] = hex[rand() % 16];
  }
  out[6] = '\0';
}

/* 请求 /api/inbox/{localpart} 并返回解析后的收件箱 JSON */
static cJSON *tenmin_app_fetch_inbox(const char *localpart) {
  char url[128];
  snprintf(url, sizeof(url), "%s/api/inbox/%s", TENMIN_APP_BASE, localpart);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, tenmin_app_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("tenmin-app: inbox http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *data = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  return data;
}

/**
 * 创建 tenmin.app 临时邮箱
 * 首次 GET 随机 localpart 即自动建箱（10 分钟 TTL），token 存储 localpart
 */
tm_email_info_t *tm_provider_tenmin_app_generate(void) {
  char local[16];
  tenmin_app_local(local, sizeof(local));

  cJSON *data = tenmin_app_fetch_inbox(local);
  if (!data)
    return NULL;

  const char *address =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(data, "address"), "");
  char email[96];
  if (!address[0]) {
    snprintf(email, sizeof(email), "%s@tenmin.app", local);
    address = email;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(data);
    return NULL;
  }
  info->channel = CHANNEL_TENMIN_APP;
  info->email = tm_strdup(address);
  info->token = tm_strdup(local);
  cJSON_Delete(data);
  return info;
}

/**
 * 读取 tenmin.app 收件箱
 * 复用建箱同一 localpart 轮询；from 为 {name,address} 对象时拆出地址字段
 */
tm_email_t *tm_provider_tenmin_app_get_emails(const char *email,
                                              const char *token, int *count) {
  *count = 0;
  if (!token || !token[0])
    return NULL;
  if (!email || !email[0])
    return NULL;

  cJSON *data = tenmin_app_fetch_inbox(token);
  if (!data)
    return NULL;

  cJSON *messages = cJSON_GetObjectItemCaseSensitive(data, "messages");
  if (!cJSON_IsArray(messages) || cJSON_GetArraySize(messages) == 0) {
    cJSON_Delete(data);
    return NULL;
  }

  int n = cJSON_GetArraySize(messages);
  *count = n;
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(data);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *msg = cJSON_GetArrayItem(messages, i);
    cJSON *raw = cJSON_CreateObject();

    const char *idv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "id"), "");
    if (idv[0])
      cJSON_AddStringToObject(raw, "id", idv);

    /* from 为对象（{name,address}）时拆出地址字段 */
    cJSON *from_obj = cJSON_GetObjectItemCaseSensitive(msg, "from");
    if (cJSON_IsObject(from_obj)) {
      const char *addr =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(from_obj, "address"),
                      "");
      const char *name =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(from_obj, "name"), "");
      if (addr[0] && name[0]) {
        char from_buf[512];
        snprintf(from_buf, sizeof(from_buf), "%s <%s>", name, addr);
        cJSON_AddStringToObject(raw, "from", from_buf);
      } else {
        cJSON_AddStringToObject(raw, "from", addr[0] ? addr : name);
      }
    } else {
      const char *fromv =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "from"), "");
      if (fromv[0])
        cJSON_AddStringToObject(raw, "from", fromv);
    }

    cJSON_AddStringToObject(raw, "to", email);

    const char *subjv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "subject"), "");
    if (subjv[0])
      cJSON_AddStringToObject(raw, "subject", subjv);

    const char *textv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "text"), "");
    if (textv[0])
      cJSON_AddStringToObject(raw, "text", textv);

    const char *htmlv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "html"), "");
    if (htmlv[0])
      cJSON_AddStringToObject(raw, "html", htmlv);

    const char *datev =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "receivedAt"), "");
    if (datev[0])
      cJSON_AddStringToObject(raw, "date", datev);

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(data);
  return emails;
}