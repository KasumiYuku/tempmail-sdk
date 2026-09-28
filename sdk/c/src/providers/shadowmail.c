/**
 * shadowmail 渠道 — https://shadowmail.win
 *
 * 注册/登录 sessionId 会话（全局 srand 由 client.c 负责）：
 *   - 注册 POST /api/register {"email":"sdk"+8 位随机 [a-z]+"@gmail.com",
 *     "password":固定密码}（幂等：回 "Email already in use" 视为已存在）。
 *   - 登录 POST /api/login（同 body）→ "Successfull Login"（沿用 Go 拼写）
 *     + Set-Cookie: sessionId=<uuid>（HttpOnly）提取纯 uuid。
 *   - 建箱 POST /api/new-address body {} 带 Cookie: sessionId=<uuid> →
 *     {"address":"<id>@shadowmail.win"}。
 *   - token 格式 "shadowmail|<account>|<password>|<sessionId>"（按 | 拆 3 段）。
 *   - 读信 POST /api/get-emails {"address":email} 带 Cookie；401/404 时用
 *     凭据重新登录换新 sessionId 重试一次；响应 message 必须为
 *     "Emails read"；mails 元素 id/address_id/sender/subject/body/
 *     created_at 归一：from=sender、to=email、date=created_at、text=body。
 */
#include "tempmail_internal.h"
#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define SHADOW_BASE "https://shadowmail.win"
#define SHADOW_PW "Abcd1234!"
#define SHADOW_DOMAIN "shadowmail.win"
#define SHADOW_TOKEN_PREFIX "shadowmail|"

/* 生成随机注册邮箱前缀（sdk+8 位小写字母） */
static void shadow_random_account(char *out, size_t cap) {
  static const char chars[] = "abcdefghijklmnopqrstuvwxyz";
  if (cap < 12) {
    out[0] = '\0';
    return;
  }
  out[0] = 's';
  out[1] = 'd';
  out[2] = 'k';
  for (int i = 0; i < 8; i++)
    out[3 + i] = chars[rand() % 26];
  out[11] = '\0';
}

/* JSON 字符串转义（简易：仅转义 " 与 \） */
static void shadow_escape(const char *src, char *out, size_t cap) {
  size_t o = 0;
  for (size_t i = 0; src[i] && o + 2 < cap; i++) {
    if (src[i] == '"' || src[i] == '\\') {
      out[o++] = '\\';
      out[o++] = src[i];
    } else {
      out[o++] = src[i];
    }
  }
  out[o] = '\0';
}

/* 从 Set-Cookie 拼接串中提取 sessionId 值（纯 uuid，不含键名）；
 * 返回静态缓冲（单调用点场景），无匹配返回 NULL。 */
static const char *shadow_extract_session(const char *cookies) {
  if (!cookies)
    return NULL;
  const char *p = strstr(cookies, "sessionId=");
  if (!p)
    return NULL;
  p += strlen("sessionId=");
  size_t n = 0;
  static char buf[128];
  while (p[n] && p[n] != ';' && n < sizeof(buf) - 1) {
    buf[n] = p[n];
    n++;
  }
  buf[n] = '\0';
  return buf;
}

/* shadow_extract_session 的线程安全动态副本（调用方 free） */
static char *shadow_dup_session(const char *cookies) {
  const char *s = shadow_extract_session(cookies);
  if (!s || !s[0])
    return NULL;
  return tm_strdup(s);
}

/* 通用 JSON POST：携带显式 Cookie 头（可选），返回响应体与状态码。
 * 成功时 *body_out 为 malloc 副本（调用方 free）、*status_out 为状态码、
 * *cookies_out 为 Set-Cookie 副本（无则 NULL，调用方 free）。
 * 返回 0=HTTP 完成，-1=网络失败。 */
static int shadow_post(const char *path, const char *json_body,
                       const char *cookie, char **body_out, long *status_out,
                       char **cookies_out) {
  *body_out = NULL;
  *status_out = 0;
  if (cookies_out)
    *cookies_out = NULL;
  char url[256];
  snprintf(url, sizeof(url), "%s%s", SHADOW_BASE, path);

  static const char *statics[3] = {
      "Content-Type: application/json",
      "Accept: application/json",
      "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
      "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 "
      "Safari/537.36"};
  char cookie_hdr[512] = {0};
  const char **headers = (const char **)malloc(sizeof(char *) * 5);
  if (!headers)
    return -1;
  int n = 0;
  headers[n++] = statics[0];
  headers[n++] = statics[1];
  headers[n++] = statics[2];
  if (cookie && cookie[0]) {
    snprintf(cookie_hdr, sizeof(cookie_hdr), "Cookie: %s", cookie);
    headers[n++] = cookie_hdr;
  }
  headers[n] = NULL;

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, headers, json_body, 15);
  free(headers);
  if (!resp)
    return -1;
  *status_out = resp->status;
  *body_out = resp->body ? tm_strdup(resp->body) : NULL;
  if (cookies_out && resp->cookies)
    *cookies_out = tm_strdup(resp->cookies);
  tm_http_response_free(resp);
  return 0;
}

/* 注册或登录（register/login），返回会话 Cookie（纯 uuid，不含键名）；
 * 失败返回 NULL。登录必须回 "Successfull Login"（沿用 Go 拼写），
 * 注册幂等接受 "Successfully Registered" / "Email already in use"。 */
static char *shadow_register_login(const char *account, const char *password,
                                   int is_login) {
  char acc_esc[256], pw_esc[128];
  shadow_escape(account, acc_esc, sizeof(acc_esc));
  shadow_escape(password, pw_esc, sizeof(pw_esc));
  char body[512];
  snprintf(body, sizeof(body), "{\"email\":\"%s\",\"password\":\"%s\"}",
           acc_esc, pw_esc);

  char *raw = NULL;
  long status = 0;
  char *cookies = NULL;
  if (shadow_post(is_login ? "/api/login" : "/api/register", body, NULL, &raw,
                  &status, &cookies) != 0) {
    return NULL;
  }
  if (status < 200 || status >= 300) {
    free(raw);
    free(cookies);
    return NULL;
  }

  cJSON *root = cJSON_Parse(raw ? raw : "");
  free(raw);
  if (!root) {
    free(cookies);
    return NULL;
  }
  const char *message =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "message"), "");
  int ok = is_login ? (strcmp(message, "Successfull Login") == 0)
                    : (strcmp(message, "Successfully Registered") == 0 ||
                       strcmp(message, "Email already in use") == 0);
  cJSON_Delete(root);
  if (!ok) {
    free(cookies);
    return NULL;
  }

  char *session = NULL;
  if (is_login) {
    session = shadow_dup_session(cookies);
  } else {
    /* 注册阶段会话非必需（后续 login 重新下发） */
    session = tm_strdup("");
  }
  free(cookies);
  return session;
}

/**
 * 创建 shadowmail.win 临时邮箱
 * 注册（幂等）→ 登录取得 sessionId → 建箱（Cookie 显式携带）。
 * token = "shadowmail|<account>|<password>|<sessionId>"。
 */
tm_email_info_t *tm_provider_shadowmail_generate(void) {
  char account[128];
  shadow_random_account(account, sizeof(account));
  strncat(account, "@gmail.com", sizeof(account) - strlen(account) - 1);

  /* 1) 注册（幂等：已存在同名账号则跳过） */
  {
    char *reg = shadow_register_login(account, SHADOW_PW, 0);
    if (!reg) {
      TM_LOG_ERR("shadowmail: 注册失败");
      return NULL;
    }
    free(reg);
  }
  /* 2) 登录取得 sessionId（纯 uuid，不合成键值对） */
  char *session = shadow_register_login(account, SHADOW_PW, 1);
  if (!session || !session[0]) {
    TM_LOG_ERR("shadowmail: 登录失败");
    free(session);
    return NULL;
  }

  /* 3) 创建地址（每账号 12 槽）：显式 Cookie 头传 sessionId */
  char cookie[256];
  snprintf(cookie, sizeof(cookie), "sessionId=%s", session);
  char *raw = NULL;
  long status = 0;
  int rc = shadow_post("/api/new-address", "{}", cookie, &raw, &status, NULL);
  if (rc != 0 || status < 200 || status >= 300) {
    TM_LOG_ERR("shadowmail: new-address 失败 http %ld", status);
    free(raw);
    free(session);
    return NULL;
  }
  cJSON *root = cJSON_Parse(raw ? raw : "");
  free(raw);
  if (!root) {
    free(session);
    return NULL;
  }
  const char *address =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "address"), "");
  cJSON_Delete(root);
  if (!address[0] || strstr(address, SHADOW_DOMAIN) == NULL) {
    TM_LOG_ERR("shadowmail: new-address 响应缺少有效地址");
    free(session);
    return NULL;
  }

  /* token 持久化：account|password|sessionId（sessionId 为 uuid，无分隔冲突） */
  size_t need = strlen(SHADOW_TOKEN_PREFIX) + strlen(account) +
                strlen(SHADOW_PW) + strlen(session) + 4;
  char *token = (char *)malloc(need);
  if (!token) {
    free(session);
    return NULL;
  }
  snprintf(token, need, "%s%s|%s|%s", SHADOW_TOKEN_PREFIX, account, SHADOW_PW,
           session);
  free(session);

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    free(token);
    return NULL;
  }
  /* 平台地址统一小写（与 Go 端对齐） */
  {
    size_t al = strlen(address);
    char *lower = (char *)malloc(al + 1);
    if (lower) {
      for (size_t i = 0; i < al; i++)
        lower[i] = (char)tolower((unsigned char)address[i]);
      lower[al] = '\0';
      info->email = lower;
    } else {
      info->email = tm_strdup(address);
    }
  }
  info->channel = CHANNEL_SHADOWMAIL;
  info->token = token;
  if (!info->email) {
    tm_free_email_info(info);
    return NULL;
  }
  return info;
}

/**
 * 读取 shadowmail.win 收件箱
 * POST /api/get-emails {"address":email} 带 Cookie；401/404 用凭据重新
 * 登录换 sessionId 重试一次；message 必须为 "Emails read"。
 */
tm_email_t *tm_provider_shadowmail_get_emails(const char *email,
                                              const char *token, int *count) {
  *count = 0;
  if (!email || !email[0])
    return NULL;
  if (!token ||
      strncmp(token, SHADOW_TOKEN_PREFIX, strlen(SHADOW_TOKEN_PREFIX)) != 0) {
    TM_LOG_ERR("shadowmail: token 格式错误");
    return NULL;
  }

  /* 按 | 拆 3 段：account / password / sessionId */
  const char *body_part = token + strlen(SHADOW_TOKEN_PREFIX);
  char p1[256], p2[128], p3[128];
  p1[0] = p2[0] = p3[0] = '\0';
  {
    const char *a = body_part;
    const char *b = strchr(a, '|');
    const char *c = b ? strchr(b + 1, '|') : NULL;
    if (!b || !c || c[1] == '\0') {
      TM_LOG_ERR("shadowmail: token 字段缺失");
      return NULL;
    }
    snprintf(p1, sizeof(p1), "%.*s", (int)(b - a), a);
    snprintf(p2, sizeof(p2), "%.*s", (int)(c - b - 1), b + 1);
    snprintf(p3, sizeof(p3), "%s", c + 1);
  }
  if (!p1[0] || !p2[0] || !p3[0]) {
    TM_LOG_ERR("shadowmail: token 凭据字段为空");
    return NULL;
  }

  /* 读信请求体 */
  char addr_esc[512];
  shadow_escape(email, addr_esc, sizeof(addr_esc));
  char req_body[640];
  snprintf(req_body, sizeof(req_body), "{\"address\":\"%s\"}", addr_esc);

  char cookie[256];
  snprintf(cookie, sizeof(cookie), "sessionId=%s", p3);
  char *raw = NULL;
  long status = 0;
  int rc = shadow_post("/api/get-emails", req_body, cookie, &raw, &status,
                       NULL);

  /* sessionId 最长 1 小时（Max-Age 3600），过期后凭据重登录重试一次 */
  if (rc != 0 || status == 401 || status == 404) {
    free(raw);
    raw = NULL;
    char *fresh = shadow_register_login(p1, p2, 1);
    if (!fresh || !fresh[0]) {
      free(fresh);
      return NULL;
    }
    snprintf(cookie, sizeof(cookie), "sessionId=%s", fresh);
    free(fresh);
    rc = shadow_post("/api/get-emails", req_body, cookie, &raw, &status, NULL);
  }
  if (rc != 0 || status < 200 || status >= 300) {
    TM_LOG_ERR("shadowmail: get-emails 失败 http %ld", status);
    free(raw);
    return NULL;
  }

  cJSON *root = cJSON_Parse(raw ? raw : "");
  free(raw);
  if (!root)
    return NULL;
  const char *message =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "message"), "");
  if (strcmp(message, "Emails read") != 0) {
    TM_LOG_ERR("shadowmail: get-emails 响应异常: %s", message);
    cJSON_Delete(root);
    return NULL;
  }

  cJSON *mails = cJSON_GetObjectItemCaseSensitive(root, "mails");
  if (!cJSON_IsArray(mails) || cJSON_GetArraySize(mails) == 0) {
    cJSON_Delete(root);
    return NULL;
  }

  int n = cJSON_GetArraySize(mails);
  *count = n;
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(root);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *m = cJSON_GetArrayItem(mails, i);
    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;

    /* from=sender、to=email、date=created_at、text=body */
    {
      const char *sender =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "sender"), "");
      if (sender[0])
        cJSON_AddStringToObject(raw, "from", sender);
    }
    cJSON_AddStringToObject(raw, "to", email);
    {
      const char *created =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "created_at"), "");
      if (created[0])
        cJSON_AddStringToObject(raw, "date", created);
    }
    {
      const char *bodyv =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "body"), "");
      if (bodyv[0])
        cJSON_AddStringToObject(raw, "text", bodyv);
    }
    /* 平台无 text/html 区分：body 默认按纯文本处理 */
    {
      const char *subj =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "subject"), "");
      if (subj[0])
        cJSON_AddStringToObject(raw, "subject", subj);
    }
    {
      const char *idv =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "id"), "");
      if (idv[0])
        cJSON_AddStringToObject(raw, "id", idv);
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}