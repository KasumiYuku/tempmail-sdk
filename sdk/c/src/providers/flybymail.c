/**
 * flybymail 渠道 — https://flybymail.com
 *
 * 无认证 REST（全局 srand 由 client.c 负责）：
 *   - 建箱 POST /api/recipients（空 JSON body {}）→
 *     {id,email,createdAt,expiresAt(毫秒)}，token=id；
 *     expiresAt/1000 转秒语义存入 expires_at（C 端毫秒字段则原样毫秒）。
 *   - 读信 GET /api/recipients/<urlenc 完整地址>/emails → {"emails":[...]}；
 *     元素 {id,from,to,subject,body,htmlBody,preview,time,read,attachments}
 *     归一：text=body、html=htmlBody、id 数字转字符串、
 *     timestamp=time（毫秒）。
 *   - 平台已知限制：MX 静默不落件（与 Go 端结论一致，读信路径保持原样）。
 */
#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define FLYBY_BASE "https://flybymail.com"

static const char *flyby_json_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *flyby_get_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* URL 编码（RFC3986，未保留字符原样） */
static char *flyby_url_encode(const char *s) {
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

/* 从 JSON 元素提取任意值并转为字符串（string 原样、number 转整数，
 * 其余为空串）；id 为数字时转字符串与 Go flybymailAnyString 对齐。 */
static char *flyby_any_string(const cJSON *m, const char *key) {
  const cJSON *v = cJSON_GetObjectItemCaseSensitive(m, key);
  if (cJSON_IsString(v) && v->valuestring && v->valuestring[0])
    return tm_strdup(v->valuestring);
  if (cJSON_IsNumber(v))
    return tm_strdup(cJSON_PrintUnformatted(v)); /* 整数无小数 */
  return tm_strdup("");
}

/* 提取 time/date 时间（毫秒）放入 timestamp 槽 */
static const cJSON *flyby_timestamp(const cJSON *m) {
  const cJSON *v = cJSON_GetObjectItemCaseSensitive(m, "time");
  if (v)
    return v;
  return cJSON_GetObjectItemCaseSensitive(m, "date");
}

/**
 * 创建 flybymail.com 临时邮箱
 * POST /api/recipients（空 JSON body）→ id/email/createdAt/expiresAt
 * （expiresAt 为毫秒，约 4 小时）。
 */
tm_email_info_t *tm_provider_flybymail_generate(void) {
  char url[128];
  snprintf(url, sizeof(url), "%s/api/recipients", FLYBY_BASE);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, flyby_json_headers, "{}", 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("flybymail: 建箱失败 http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("flybymail: 解析建箱响应失败");
    return NULL;
  }

  const char *id =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "id"), "");
  const char *email =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "email"), "");
  const cJSON *exp = cJSON_GetObjectItemCaseSensitive(root, "expiresAt");
  long long expires_ms =
      (cJSON_IsNumber(exp) && exp->valuedouble > 0)
          ? (long long)exp->valuedouble
          : 0;
  if (!id[0] || !email[0] || !strchr(email, '@')) {
    TM_LOG_ERR("flybymail: 建箱响应缺少必要字段");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_FLYBYMAIL;
  info->email = tm_strdup(email);
  info->token = tm_strdup(id);
  /* expiresAt 毫秒原样存入 C 端毫秒字段（Go 端转秒仅为 EmailInfo 统一展示） */
  info->expires_at = expires_ms;
  cJSON_Delete(root);
  return info;
}

/**
 * 读取 flybymail.com 收件箱
 * GET /api/recipients/{email}/emails（按地址查询），元素按多候选归一
 * （text=body、html=htmlBody、timestamp=time 毫秒）。
 */
tm_email_t *tm_provider_flybymail_get_emails(const char *email,
                                             const char *token, int *count) {
  *count = 0;
  (void)token;
  if (!email || !email[0] || !strchr(email, '@'))
    return NULL;

  char *enc = flyby_url_encode(email);
  if (!enc)
    return NULL;
  size_t need = strlen(FLYBY_BASE) + strlen(enc) + 32;
  char *url = (char *)malloc(need);
  if (!url) {
    free(enc);
    return NULL;
  }
  snprintf(url, need, "%s/api/recipients/%s/emails", FLYBY_BASE, enc);
  free(enc);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, flyby_get_headers, NULL, 15);
  free(url);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("flybymail: 读取邮件列表失败 http %ld",
               resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("flybymail: 解析邮件列表失败");
    return NULL;
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
    cJSON *m = cJSON_GetArrayItem(emails_arr, i);
    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;

    /* 字段映射：from/to/subject/body→text、htmlBody→html、
     * time→timestamp（normalize 的 timestamp 候选按毫秒解析） */
    const char *keys_map[][2] = {
        {"from", "from"}, {"to", "to"},         {"subject", "subject"},
        {"body", "text"}, {"htmlBody", "html"}, {"preview", "preview"},
        {"read", "read"},
    };
    for (int k = 0; k < 7; k++) {
      char *v = flyby_any_string(m, keys_map[k][0]);
      if (v[0])
        cJSON_AddStringToObject(raw, keys_map[k][1], v);
      free(v);
    }

    /* id 数字转字符串 */
    {
      char *idv = flyby_any_string(m, "id");
      if (idv[0])
        cJSON_AddStringToObject(raw, "id", idv);
      free(idv);
    }

    /* timestamp=time（毫秒），normalize 自动毫秒→RFC3339 */
    {
      const cJSON *ts = flyby_timestamp(m);
      if (ts)
        cJSON_AddItemReferenceToObject(raw, "timestamp", (cJSON *)ts);
    }

    /* attachments 透传（normalize 解析 attachments 数组） */
    {
      const cJSON *att = cJSON_GetObjectItemCaseSensitive(m, "attachments");
      if (att)
        cJSON_AddItemReferenceToObject(raw, "attachments", (cJSON *)att);
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}