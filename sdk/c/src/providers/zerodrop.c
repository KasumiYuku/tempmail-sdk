/**
 * zerodrop 渠道 — https://zerodrop.dev
 *
 * 无认证 REST：建箱无需请求，本地生成 "sdk"+8 位随机名，
 * 地址为 <名>@zerodrop-sandbox.online；读信 GET /api/inbox/{name}
 * ?source=sdk，响应形如 {"emails":[...],"count":N}。
 * 平台邮件对象只有 id/from/to/subject/receivedAt/raw/otp/magicLink：
 * 正文仅存在于 raw（完整 MIME 原文，头部与 body 以空行分隔），
 * 无 text/html 字段，故从 raw 中剥离头部提取纯文本正文填入 text。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define ZERODROP_BASE "https://zerodrop.dev"
#define ZERODROP_DOMAIN "zerodrop-sandbox.online"

static const char *zerodrop_get_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* 多候选字段取字符串（非空即返回） */
static const char *zerodrop_pick_str(cJSON *m, const char **keys, int n) {
  for (int i = 0; i < n; i++) {
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(m, keys[i]);
    if (cJSON_IsString(v) && v->valuestring && v->valuestring[0])
      return v->valuestring;
  }
  return NULL;
}

/* 生成 "sdk"+8 位随机本地名 */
static void zerodrop_local_name(char *out, size_t cap) {
  static const char chars[] = "abcdefghijklmnopqrstuvwxyz0123456789";
  size_t o = 0;
  memcpy(out, "sdk", 3);
  o += 3;
  for (int i = 0; i < 8 && o + 1 < cap; i++) {
    out[o++] = chars[rand() % (sizeof(chars) - 1)];
  }
  out[o] = '\0';
}

/*
 * 从 raw（完整 MIME 原文）提取纯文本正文：
 * 定位首个空行（RFC 5322 头部/正文分隔，\r\n\r\n 或 \n\n），
 * 其后部分即 body。
 */
static const char *zerodrop_raw_body(const char *raw) {
  const char *p = strstr(raw, "\r\n\r\n");
  if (p)
    return p + 4;
  p = strstr(raw, "\n\n");
  if (p)
    return p + 2;
  return NULL;
}

/**
 * 创建临时邮箱
 * 建箱无需请求，本地生成随机名，token 复用完整地址
 */
tm_email_info_t *tm_provider_zerodrop_generate(void) {
  char local[32];
  zerodrop_local_name(local, sizeof(local));

  char email[96];
  snprintf(email, sizeof(email), "%s@%s", local, ZERODROP_DOMAIN);

  tm_email_info_t *info = tm_email_info_new();
  if (!info)
    return NULL;
  info->channel = CHANNEL_ZERODROP;
  info->email = tm_strdup(email);
  info->token = tm_strdup(email);
  return info;
}

/**
 * 读取收件箱
 * GET /api/inbox/{name}?source=sdk，多候选字段交归一化处理
 */
tm_email_t *tm_provider_zerodrop_get_emails(const char *email,
                                            const char *token, int *count) {
  *count = 0;
  (void)token;
  if (!email || !email[0])
    return NULL;

  /* 校验域名并拆出 localpart */
  const char *at = strchr(email, '@');
  if (!at || strcmp(at + 1, ZERODROP_DOMAIN) != 0 || at == email) {
    TM_LOG_ERR("zerodrop: 非 %s 域邮箱地址", ZERODROP_DOMAIN);
    return NULL;
  }

  char url[256];
  snprintf(url, sizeof(url), "%s/api/inbox/%.*s?source=sdk", ZERODROP_BASE,
           (int)(at - email), email);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, zerodrop_get_headers, NULL, 15);
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

    const char *id_keys[] = {"id"};
    const char *idv = zerodrop_pick_str(msg, id_keys, 1);
    if (idv)
      cJSON_AddStringToObject(raw, "id", idv);

    const char *from_keys[] = {"from"};
    const char *fromv = zerodrop_pick_str(msg, from_keys, 1);
    if (fromv)
      cJSON_AddStringToObject(raw, "from", fromv);

    /* 平台响应无 to 字段，收件人固定为当前邮箱 */
    cJSON_AddStringToObject(raw, "to", email);

    const char *subj_keys[] = {"subject"};
    const char *subjv = zerodrop_pick_str(msg, subj_keys, 1);
    if (subjv)
      cJSON_AddStringToObject(raw, "subject", subjv);

    const char *date_keys[] = {"receivedAt"};
    const char *datev = zerodrop_pick_str(msg, date_keys, 1);
    if (datev)
      cJSON_AddStringToObject(raw, "date", datev);

    /* 正文仅存在于 raw（完整 MIME 原文）：提取纯文本 body 作 text */
    const char *raw_keys[] = {"raw"};
    const char *rawv = zerodrop_pick_str(msg, raw_keys, 1);
    if (rawv) {
      const char *body = zerodrop_raw_body(rawv);
      if (body && body[0]) {
        cJSON_AddStringToObject(raw, "text", body);
      }
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}