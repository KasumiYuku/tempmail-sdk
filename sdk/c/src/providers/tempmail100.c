/**
 * tempmail100 渠道 — https://tempmail100.com
 *
 * 裸 token Authorization（全局 srand 由 client.c 负责）：
 *   - 建箱 POST /init（无 body）→ {"code":0,"data":{"token"}}；
 *     POST /web/generate 带 Authorization: <裸 token，无 Bearer> →
 *     {"code":0,"data":{"address"}}。
 *   - 读信 GET /web/emails（Authorization 裸 token）→
 *     {"code","data":{"list":[{uuid,subject,fromAddress,toAddress,fromName,
 *     content,timestamp,read}],"total"}}；code!=0 报错；list 为 null 返回空。
 *   - 每项归一：id=uuid、fromName 非空且 != fromAddress 且 fromAddress 含 @
 *     时合成 "Name <address>"、to=toAddress、subject、content（平台恒空，
 *     如实留空）、timestamp 毫秒、isRead=read（bool/数字/字符串
 *     "true"|"1" 兼容）。
 *   - 平台限制：正文端点不存在（content 恒空），本渠道客观为
 *     「列表-only」。
 */
#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#define strcasecmp _stricmp
#else
#include <strings.h>
#endif

#define TMP100_BASE "https://tempmail100.com"

/* 组配带裸 token Authorization 的请求头（无 Bearer；无 token 时仅公共头） */
static char **tmp100_auth_headers(const char *token) {
  int has = token && token[0];
  const char **h = (const char **)calloc((size_t)(3 + (has ? 1 : 0)),
                                         sizeof(char *));
  if (!h)
    return NULL;
  h[0] = "Accept: application/json";
  h[1] =
      "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
      "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 "
      "Safari/537.36";
  if (has) {
    size_t len = strlen(token) + 32;
    char *auth = (char *)malloc(len);
    if (!auth) {
      free(h);
      return NULL;
    }
    snprintf(auth, len, "Authorization: %s", token);
    h[2] = auth;
  }
  h[(has ? 3 : 2)] = NULL;
  return (char **)h;
}

/* 释放 auth_headers */
static void tmp100_free_headers(char **h) {
  if (!h)
    return;
  if (h[2])
    free(h[2]);
  free(h);
}

/* 将接口字段值安全转换为字符串：nil/非 string/非 number 返回空串 */
static void tmp100_str_field(const cJSON *item, const char *key, char *out,
                             size_t cap) {
  out[0] = '\0';
  const cJSON *v = cJSON_GetObjectItemCaseSensitive(item, key);
  if (cJSON_IsString(v) && v->valuestring) {
    snprintf(out, cap, "%s", v->valuestring);
  } else if (cJSON_IsNumber(v)) {
    if (v->valuedouble == (long long)v->valuedouble)
      snprintf(out, cap, "%lld", (long long)v->valuedouble);
    else
      snprintf(out, cap, "%g", v->valuedouble);
  }
}

/* read 字段归一为布尔：bool / 数字(0|1) / 字符串("true"|"1") 兼容 */
static int tmp100_read(const cJSON *item) {
  const cJSON *v = cJSON_GetObjectItemCaseSensitive(item, "read");
  if (cJSON_IsBool(v))
    return cJSON_IsTrue(v) ? 1 : 0;
  if (cJSON_IsNumber(v))
    return v->valuedouble != 0 ? 1 : 0;
  if (cJSON_IsString(v) && v->valuestring) {
    if (strcasecmp(v->valuestring, "true") == 0 ||
        strcmp(v->valuestring, "1") == 0)
      return 1;
  }
  return 0;
}

/**
 * 创建 tempmail100.com 临时邮箱
 * POST /init 取 JWT，再 POST /web/generate（Authorization 裸 token）
 * 创建地址；token 为初始化 JWT。
 */
tm_email_info_t *tm_provider_tempmail100_generate(void) {
  char url[128];
  /* 第一步：初始化取得 token */
  snprintf(url, sizeof(url), "%s/init", TMP100_BASE);
  char **h = tmp100_auth_headers(NULL);
  if (!h)
    return NULL;
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, (const char **)h, NULL, 15);
  tmp100_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("tempmail100: 初始化失败 http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("tempmail100: 解析初始化响应失败");
    return NULL;
  }
  {
    const cJSON *code = cJSON_GetObjectItemCaseSensitive(root, "code");
    const cJSON *data = cJSON_GetObjectItemCaseSensitive(root, "data");
    const char *token =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(data, "token"), "");
    if (!cJSON_IsNumber(code) || code->valueint != 0 || !token[0]) {
      TM_LOG_ERR("tempmail100: 初始化响应异常");
      cJSON_Delete(root);
      return NULL;
    }
    /* 第二步：创建随机地址 */
    char *jwt = tm_strdup(token);

    snprintf(url, sizeof(url), "%s/web/generate", TMP100_BASE);
    char **h2 = tmp100_auth_headers(jwt);
    if (!h2) {
      free(jwt);
      cJSON_Delete(root);
      return NULL;
    }
    tm_http_response_t *resp2 =
        tm_http_request(TM_HTTP_POST, url, (const char **)h2, NULL, 15);
    tmp100_free_headers(h2);
    if (!resp2 || resp2->status < 200 || resp2->status >= 300) {
      TM_LOG_ERR("tempmail100: 创建地址失败 http %ld",
                 resp2 ? resp2->status : -1);
      tm_http_response_free(resp2);
      free(jwt);
      cJSON_Delete(root);
      return NULL;
    }
    cJSON *gen = cJSON_Parse(resp2->body);
    tm_http_response_free(resp2);
    if (!gen) {
      free(jwt);
      cJSON_Delete(root);
      return NULL;
    }
    const cJSON *code2 = cJSON_GetObjectItemCaseSensitive(gen, "code");
    const cJSON *data2 = cJSON_GetObjectItemCaseSensitive(gen, "data");
    const char *address =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(data2, "address"), "");
    if (!cJSON_IsNumber(code2) || code2->valueint != 0 || !address[0] ||
        !strchr(address, '@')) {
      TM_LOG_ERR("tempmail100: 创建地址响应异常");
      cJSON_Delete(gen);
      free(jwt);
      cJSON_Delete(root);
      return NULL;
    }
    tm_email_info_t *info = tm_email_info_new();
    if (!info) {
      cJSON_Delete(gen);
      free(jwt);
      cJSON_Delete(root);
      return NULL;
    }
    info->channel = CHANNEL_TEMPMAIL100;
    info->email = tm_strdup(address);
    info->token = jwt; /* 转移所有权 */
    cJSON_Delete(gen);
    cJSON_Delete(root);
    return info;
  }
}

/**
 * 读取 tempmail100.com 邮件列表
 * GET /web/emails（Authorization 裸 token）→ code/data.list/data.total；
 * code!=0 报错；list 为 null 返回空；content 平台恒空如实留空。
 */
tm_email_t *tm_provider_tempmail100_get_emails(const char *email,
                                               const char *token,
                                               int *count) {
  *count = 0;
  (void)email;
  if (!token || !token[0])
    return NULL;

  char url[128];
  snprintf(url, sizeof(url), "%s/web/emails", TMP100_BASE);
  char **h = tmp100_auth_headers(token);
  if (!h)
    return NULL;
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  tmp100_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("tempmail100: 获取邮件列表失败 http %ld",
               resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("tempmail100: 解析邮件列表失败");
    return NULL;
  }

  {
    const cJSON *code = cJSON_GetObjectItemCaseSensitive(root, "code");
    if (!cJSON_IsNumber(code) || code->valueint != 0) {
      TM_LOG_ERR("tempmail100: 获取邮件列表响应异常: %s",
                 TM_JSON_STR(
                     cJSON_GetObjectItemCaseSensitive(root, "message"), ""));
      cJSON_Delete(root);
      return NULL;
    }
  }

  cJSON *data = cJSON_GetObjectItemCaseSensitive(root, "data");
  cJSON *list = cJSON_GetObjectItemCaseSensitive(data, "list");
  if (!cJSON_IsArray(list) || cJSON_GetArraySize(list) == 0) {
    cJSON_Delete(root);
    return NULL;
  }

  int n = cJSON_GetArraySize(list);
  *count = n;
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(root);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *item = cJSON_GetArrayItem(list, i);
    if (!cJSON_IsObject(item))
      continue;
    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;

    /* id=uuid */
    {
      char buf[256];
      tmp100_str_field(item, "uuid", buf, sizeof(buf));
      if (buf[0])
        cJSON_AddStringToObject(raw, "id", buf);
    }

    /* fromName + fromAddress 合成 "Name <address>"（条件同 Go） */
    {
      char name[256], addr[512];
      tmp100_str_field(item, "fromName", name, sizeof(name));
      tmp100_str_field(item, "fromAddress", addr, sizeof(addr));
      if (name[0] && strcmp(name, addr) != 0 && strchr(addr, '@')) {
        char composed[768];
        snprintf(composed, sizeof(composed), "%s <%s>", name, addr);
        cJSON_AddStringToObject(raw, "from", composed);
      } else if (addr[0]) {
        cJSON_AddStringToObject(raw, "from", addr);
      }
    }

    /* to=toAddress */
    {
      char buf[512];
      tmp100_str_field(item, "toAddress", buf, sizeof(buf));
      if (buf[0])
        cJSON_AddStringToObject(raw, "to", buf);
    }

    /* subject */
    {
      char buf[1024];
      tmp100_str_field(item, "subject", buf, sizeof(buf));
      if (buf[0])
        cJSON_AddStringToObject(raw, "subject", buf);
    }

    /* content 平台恒空（正文端点不存在），如实留空；
     * 非空时亦如实透传 */
    {
      char buf[8192];
      tmp100_str_field(item, "content", buf, sizeof(buf));
      if (buf[0])
        cJSON_AddStringToObject(raw, "content", buf);
    }

    /* timestamp 毫秒透传（normalize 按 >1e12 毫秒解析） */
    {
      const cJSON *ts = cJSON_GetObjectItemCaseSensitive(item, "timestamp");
      if (ts)
        cJSON_AddItemReferenceToObject(raw, "timestamp", (cJSON *)ts);
    }

    /* isRead=read（bool/数字/字符串兼容） */
    {
      cJSON *is_read = cJSON_CreateBool(tmp100_read(item) ? 1 : 0);
      cJSON_AddItemToObject(raw, "isRead", is_read);
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}