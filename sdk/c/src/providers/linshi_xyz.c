/**
 * linshi-xyz 渠道 — https://linshi.xyz
 *
 * 无建箱请求（全局 srand 由 client.c 负责）：
 *   - 本地随机 6 位 hex 前缀（0-9a-f，与官网 client 同格式）+ @linshi.xyz，
 *     token 复用完整地址。
 *   - 读信 GET https://linshi.xyz/api/mails/<前缀>，响应为邮件对象数组；
 *     元素 {headers:{from,to,subject,date},html} 需把 headers 平铺到顶层、
 *     无 to 时注入当前地址后归一化；响应非数组整体失败。
 */
#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define LIN_XYZ_BASE "https://linshi.xyz"
#define LIN_XYZ_DOMAIN "linshi.xyz"

static const char *lin_xyz_get_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* 生成本地随机 6 位 hex 前缀 */
static void lin_xyz_local(char *out, size_t cap) {
  static const char hexd[] = "0123456789abcdef";
  if (cap < 7)
    return;
  for (int i = 0; i < 6; i++)
    out[i] = hexd[rand() % 16];
  out[6] = '\0';
}

/**
 * 创建 linshi.xyz 临时邮箱
 * 无需建箱请求，本地随机 6 位 hex 前缀；token 复用完整地址。
 */
tm_email_info_t *tm_provider_linshi_xyz_generate(void) {
  char local[8];
  lin_xyz_local(local, sizeof(local));

  char email[128];
  snprintf(email, sizeof(email), "%s@%s", local, LIN_XYZ_DOMAIN);

  tm_email_info_t *info = tm_email_info_new();
  if (!info)
    return NULL;
  info->channel = CHANNEL_LINSHI_XYZ;
  info->email = tm_strdup(email);
  info->token = tm_strdup(email);
  return info;
}

/**
 * 读取 linshi.xyz 收件箱
 * GET /api/mails/<前缀>，响应为邮件对象数组；单封对象的 headers
 * 平铺到顶层，无 to 时注入当前地址；响应非数组整体失败。
 */
tm_email_t *tm_provider_linshi_xyz_get_emails(const char *email,
                                              const char *token, int *count) {
  *count = 0;
  (void)token;
  if (!email || !email[0] || !strchr(email, '@'))
    return NULL;

  /* 取前缀（@ 之前的本地名） */
  const char *at = strchr(email, '@');
  size_t local_len = (size_t)(at - email);
  if (local_len == 0 || local_len > 64)
    return NULL;

  char url[256];
  snprintf(url, sizeof(url), "%s/api/mails/%.*s", LIN_XYZ_BASE, (int)local_len,
           email);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, lin_xyz_get_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("linshi-xyz: 读取收件箱失败 http %ld",
               resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *list = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!cJSON_IsArray(list)) {
    /* 非数组骨架（如 {"ok":false}）：整体失败，交由上层 fallback */
    TM_LOG_ERR("linshi-xyz: 解析收件箱响应失败（响应非数组）");
    cJSON_Delete(list);
    return NULL;
  }

  int n = cJSON_GetArraySize(list);
  if (n == 0) {
    cJSON_Delete(list);
    return NULL;
  }
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(list);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *msg = cJSON_GetArrayItem(list, i);
    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;

    /* headers 平铺到顶层（from/to/subject/date） */
    cJSON *headers = cJSON_GetObjectItemCaseSensitive(msg, "headers");
    if (cJSON_IsObject(headers)) {
      const char *hk[] = {"from", "to", "subject", "date"};
      for (int k = 0; k < 4; k++) {
        const cJSON *v = cJSON_GetObjectItemCaseSensitive(headers, hk[k]);
        if (cJSON_IsString(v) && v->valuestring && v->valuestring[0])
          cJSON_AddStringToObject(raw, hk[k], v->valuestring);
      }
    }
    /* html 透传 */
    const cJSON *htmlv = cJSON_GetObjectItemCaseSensitive(msg, "html");
    if (cJSON_IsString(htmlv) && htmlv->valuestring && htmlv->valuestring[0])
      cJSON_AddStringToObject(raw, "html", htmlv->valuestring);
    /* id 透传（若存在） */
    const cJSON *idv = cJSON_GetObjectItemCaseSensitive(msg, "id");
    if (cJSON_IsString(idv) && idv->valuestring && idv->valuestring[0])
      cJSON_AddStringToObject(raw, "id", idv->valuestring);

    /* 无 to 时注入收件人地址 */
    if (cJSON_GetObjectItemCaseSensitive(raw, "to") == NULL)
      cJSON_AddStringToObject(raw, "to", email);

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(list);
  return emails;
}