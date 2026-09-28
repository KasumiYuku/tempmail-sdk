/**
 * Internxt 渠道 — https://internxt.com/temporary-email（Next.js + OpenNext）
 *
 * 调研实证结论（与 Go 端 internxt.go 一致）：
 *   - 建箱 GET /api/temp-mail/create-email，读信 GET
 *     /api/temp-mail/get-inbox?email=<e>&token=<t>，详情 GET
 *     /api/temp-mail/get-message?email=<e>&token=<t>&messageId=<id>。
 *     create-email 仅接受 GET（POST 返回 405 Method not allowed）。
 *   - CSRF：首次 GET /temporary-email 响应 Set-Cookie csrfSecret=...
 *     与 XSRF-TOKEN=...。数据接口校验请求头 csrf-token，其值必须与
 *     Cookie jar 中 XSRF-TOKEN 一致（每个 API 响应都会刷新 XSRF-TOKEN
 *     的 Set-Cookie，故每次读信前都应重取最新值）。
 *   - 建箱响应：{"address":"<前缀>@uberip.com","token":"<十六进制>"}。
 *   - get-inbox 正常返回顶层数组（空箱 []）；错误 token 返回 401。
 *
 * Cookie 策略：本模块自管静态罐（csrfSecret/XSRF-TOKEN，双桶），
 *   接口响应同名覆写；接口请求带 csrf-token 头（=罐中 XSRF-TOKEN）+
 *   Cookie 头（罐内容）。
 * token 语义：{"address","token"} JSON。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#define strcasecmp _stricmp
#define strncasecmp _strnicmp
#else
#include <strings.h>
#endif

#define INTERNXT_SITE "https://internxt.com"
#define INTERNXT_REF "https://internxt.com/temporary-email"
#define INTERNXT_API "https://internxt.com/api/temp-mail"
#define INTERNXT_UA                                                           \
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) " \
  "Chrome/154.0.0.0 Safari/537.36"

/* 页面 GET 头 */
static const char *internxt_page_headers[] = {
    "User-Agent: " INTERNXT_UA,
    "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,"
    "image/avif,image/webp,*/*;q=0.8",
    "Accept-Language: en-US,en;q=0.9",
    NULL};

/* 模块级自管 Cookie 罐（csrfSecret 恒定，XSRF-TOKEN 每个 API 响应刷新） */
static char g_xsrf[512];
static char g_secret[512];

/* 收下响应 Set-Cookie：按名覆写 g_xsrf / g_secret */
static void internxt_merge_cookies(const char *cookies) {
  if (!cookies)
    return;
  const char *p = cookies;
  while (p && *p) {
    /* 按 "<name>=<value>" 段扫描（多段以 "; " 相接） */
    const char *eq = strstr(p, "=");
    if (!eq)
      break;
    size_t nlen = (size_t)(eq - p);
    const char *val = eq + 1;
    size_t vlen = strcspn(val, ";");
    if (nlen == 10 && strncasecmp(p, "XSRF-TOKEN", 10) == 0) {
      size_t cap = sizeof(g_xsrf);
      size_t copy = vlen < cap - 1 ? vlen : cap - 1;
      memcpy(g_xsrf, val, copy);
      g_xsrf[copy] = '\0';
    } else if (nlen == 10 && strncasecmp(p, "csrfSecret", 10) == 0) {
      size_t cap = sizeof(g_secret);
      size_t copy = vlen < cap - 1 ? vlen : cap - 1;
      memcpy(g_secret, val, copy);
      g_secret[copy] = '\0';
    }
    const char *semi = strchr(val, ';');
    if (!semi)
      break;
    p = semi + 1;
    while (*p == ' ')
      p++;
  }
}

/* 组装 Cookie 请求头（罐内非空者携带），返回 malloc 串或 NULL */
static char *internxt_cookie_header(void) {
  size_t n = 0;
  if (g_secret[0])
    n += strlen(g_secret) + 11;
  if (g_xsrf[0])
    n += strlen(g_xsrf) + 11;
  if (n == 0)
    return NULL;
  char *hdr = (char *)malloc(n + 8);
  if (!hdr)
    return NULL;
  hdr[0] = '\0';
  if (g_secret[0]) {
    strcat(hdr, "csrfSecret=");
    strcat(hdr, g_secret);
  }
  if (g_xsrf[0]) {
    if (hdr[0])
      strcat(hdr, "; ");
    strcat(hdr, "XSRF-TOKEN=");
    strcat(hdr, g_xsrf);
  }
  return hdr;
}

/* 确保罐中持有 XSRF-TOKEN：无则 GET /temporary-email 夺取 */
static int internxt_prepare_xsrf(void) {
  if (g_xsrf[0])
    return 0;
  tm_http_response_t *resp = tm_http_request(TM_HTTP_GET, INTERNXT_REF,
                                             internxt_page_headers, NULL, 20);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("internxt: 夺取 XSRF-TOKEN 页面失败 http %ld",
               resp ? resp->status : -1L);
    tm_http_response_free(resp);
    return -1;
  }
  internxt_merge_cookies(resp->cookies);
  tm_http_response_free(resp);
  if (!g_xsrf[0]) {
    TM_LOG_ERR("internxt: 未取得 XSRF-TOKEN Cookie");
    return -1;
  }
  return 0;
}

/* 带 CSRF 头请求 internxt 数据接口（GET），返回响应体拷贝（需 free）；
 * 请求后罐内 XSRF-TOKEN 被同名刷新，下一次请求将从罐中重取最新值。 */
static char *internxt_api_get(const char *path, const char *query,
                              long *status_out) {
  if (internxt_prepare_xsrf() != 0)
    return NULL;

  char url[1024];
  if (query && query[0]) {
    snprintf(url, sizeof(url), "%s%s?%s", INTERNXT_API, path, query);
  } else {
    snprintf(url, sizeof(url), "%s%s", INTERNXT_API, path);
  }

  char csrf_hdr[640];
  snprintf(csrf_hdr, sizeof(csrf_hdr), "csrf-token: %s", g_xsrf);

  const char *headers[16];
  int n = 0;
  headers[n++] = "User-Agent: " INTERNXT_UA;
  headers[n++] = "Accept: application/json, text/plain, */*";
  headers[n++] = "Origin: " INTERNXT_SITE;
  headers[n++] = "Referer: " INTERNXT_REF;
  headers[n++] = csrf_hdr;
  char *cookie_hdr = internxt_cookie_header();
  if (cookie_hdr) {
    headers[n++] = cookie_hdr;
  }
  headers[n] = NULL;

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, headers, NULL, 20);
  free(cookie_hdr);
  if (!resp) {
    if (status_out)
      *status_out = -1;
    return NULL;
  }
  internxt_merge_cookies(resp->cookies);
  if (status_out)
    *status_out = resp->status;
  char *body = tm_strdup(resp->body ? resp->body : "");
  tm_http_response_free(resp);
  return body;
}

/* URL 查询参数编码（email/token/messageId 取值可含特殊字符，仅做转义
 * 预留；实测取值均为 URL 安全字符，此处直拼） */
static void internxt_qp(char *out, size_t cap, const char *name,
                        const char *value, int *first) {
  if (!value || !value[0])
    return;
  char *p = out + strlen(out);
  size_t left = cap - strlen(out);
  int w = snprintf(p, left, "%s%s=%s", *first ? "" : "&", name, value);
  if (w > 0)
    *first = 0;
}

/**
 * 创建 internxt.com 临时邮箱
 * 先确保罐中 XSRF-TOKEN，再 GET /api/temp-mail/create-email。
 * token 保存 {"address","token"} JSON。
 */
tm_email_info_t *tm_provider_internxt_generate(void) {
  long status = 0;
  char *body = internxt_api_get("/create-email", NULL, &status);
  if (!body)
    return NULL;
  if (status < 200 || status >= 300) {
    TM_LOG_ERR("internxt: /create-email 失败 http %ld", status);
    free(body);
    return NULL;
  }

  cJSON *data = cJSON_Parse(body);
  free(body);
  if (!data) {
    TM_LOG_ERR("internxt: 解析建箱响应失败");
    return NULL;
  }
  const char *address =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(data, "address"), "");
  const char *token =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(data, "token"), "");
  if (!address[0] || !token[0] || !strchr(address, '@')) {
    TM_LOG_ERR("internxt: 创建邮箱响应缺少必要字段（address/token）");
    cJSON_Delete(data);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(data);
    return NULL;
  }
  info->channel = CHANNEL_INTERNXT;
  info->email = tm_strdup(address);

  /* 会话凭据 JSON：{address, token} */
  size_t tlen = strlen(address) + strlen(token) + 32;
  info->token = (char *)malloc(tlen);
  if (info->token) {
    snprintf(info->token, tlen, "{\"address\":\"%s\",\"token\":\"%s\"}",
             address, token);
  }
  cJSON_Delete(data);
  return info;
}

/* 从列表/详情元素提取邮件 ID，候选字段 id/messageId/message_id */
static const char *internxt_message_id(cJSON *msg) {
  const char *keys[] = {"id", "messageId", "message_id"};
  for (int i = 0; i < 3; i++) {
    const cJSON *v = cJSON_GetObjectItemCaseSensitive(msg, keys[i]);
    if (cJSON_IsString(v) && v->valuestring && v->valuestring[0]) {
      return v->valuestring;
    }
  }
  return NULL;
}

/**
 * 读取 internxt.com 收件箱
 * get-inbox 返回顶层数组；逐条 get-message 拉单封全文合并（详情失败
 * 回退列表摘要）。
 */
tm_email_t *tm_provider_internxt_get_emails(const char *email,
                                            const char *token, int *count) {
  *count = 0;
  if (!token || !token[0])
    return NULL;
  if (!email || !email[0])
    return NULL;

  cJSON *sess = cJSON_Parse(token);
  if (!sess) {
    TM_LOG_ERR("internxt: 会话凭据解析失败");
    return NULL;
  }
  const char *address =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "address"), "");
  const char *api_token =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "token"), "");
  if (!address[0] || !api_token[0]) {
    TM_LOG_ERR("internxt: 会话凭据缺少必要字段");
    cJSON_Delete(sess);
    return NULL;
  }
  if (strcmp(address, email) != 0) {
    TM_LOG_ERR("internxt: 会话邮箱与查询邮箱不匹配");
    cJSON_Delete(sess);
    return NULL;
  }

  char query[1024];
  query[0] = '\0';
  int first = 1;
  internxt_qp(query, sizeof(query), "email", address, &first);
  internxt_qp(query, sizeof(query), "token", api_token, &first);

  long status = 0;
  char *body = internxt_api_get("/get-inbox", query, &status);
  if (!body) {
    cJSON_Delete(sess);
    return NULL;
  }
  if (status < 200 || status >= 300) {
    TM_LOG_ERR("internxt: /get-inbox 失败 http %ld", status);
    free(body);
    cJSON_Delete(sess);
    return NULL;
  }

  cJSON *list = NULL;
  if (body[0] && strcmp(body, "[]") != 0) {
    list = cJSON_Parse(body);
    if (!cJSON_IsArray(list)) {
      TM_LOG_ERR("internxt: 解析收件箱响应失败");
      if (list)
        cJSON_Delete(list);
      free(body);
      cJSON_Delete(sess);
      return NULL;
    }
  }
  free(body);

  int n = list ? cJSON_GetArraySize(list) : 0;
  *count = n;
  tm_email_t *emails = tm_emails_new(n > 0 ? n : 1);
  if (!emails) {
    *count = -1;
    if (list)
      cJSON_Delete(list);
    cJSON_Delete(sess);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *msg = cJSON_GetArrayItem(list, i);
    if (!cJSON_IsObject(msg))
      continue;
    const char *mid = internxt_message_id(msg);
    if (mid) {
      char dq[1024];
      dq[0] = '\0';
      int dfirst = 1;
      internxt_qp(dq, sizeof(dq), "email", address, &dfirst);
      internxt_qp(dq, sizeof(dq), "token", api_token, &dfirst);
      internxt_qp(dq, sizeof(dq), "messageId", mid, &dfirst);

      long dstatus = 0;
      char *dbody = internxt_api_get("/get-message", dq, &dstatus);
      if (dbody && dstatus >= 200 && dstatus < 300) {
        cJSON *detail = cJSON_Parse(dbody);
        if (cJSON_IsObject(detail)) {
          /* 列表字段优先，详情仅补齐缺失字段 */
          cJSON *child = NULL;
          cJSON_ArrayForEach(child, detail) {
            const char *key = child->string;
            if (!key)
              continue;
            if (cJSON_GetObjectItemCaseSensitive(msg, key) == NULL) {
              cJSON_AddItemReferenceToObject(msg, key, child);
            }
          }
          cJSON_Delete(detail);
        } else if (detail) {
          cJSON_Delete(detail);
        }
      }
      free(dbody);
    }
    emails[i] = tm_normalize_email(msg, email);
  }

  if (list)
    cJSON_Delete(list);
  cJSON_Delete(sess);
  return emails;
}