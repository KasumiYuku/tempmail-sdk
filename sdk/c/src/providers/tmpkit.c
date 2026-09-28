/**
 * Tmpkit 渠道 — https://tmpkit.com（Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）
 *
 * 调研实证结论（与 Go 端 tmpkit.go 一致）：
 *   - POST /api/rpc/tempmail/initSession，body 为 tRPC 包裹 {"json":{}}，
 *     免凭据。响应 {"json":{"session":{"sessionId","email",...}},"..."}，
 *     同时 Set-Cookie: tempmail_session=<sessionId>; Max-Age=3600（与
 *     sessionId 同值）。
 *   - POST /api/rpc/tempmail/getEmails，body {"json":{"offset":0,
 *     "limit":20}}，需带 tempmail_session Cookie。不带 Cookie 时 200
 *     但 emails 为空、session 为 null。列表元素键为 mailId/from/subject/
 *     excerpt/date/timestamp/hasAttach/isRead（无 id 键）。
 *   - POST /api/rpc/tempmail/getEmailDetail，body
 *     {"json":{"mailId":<数字>}}（mailId 为数字，字符串会 zod 400）。
 *     响应为单封详情对象（body 为含 <br> 的纯文本正文）。
 *
 * Cookie 策略：token 保存 sessionId（tempMailSession=<sid>），读信时
 *   显式携带 Cookie: tempmail_session=<sid> 头，防并行会话串箱。
 */

#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define TMPKIT_BASE "https://tmpkit.com"
#define TMPKIT_RPC "https://tmpkit.com/api/rpc/tempmail"
#define TMPKIT_UA                                                             \
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) " \
  "Chrome/154.0.0.0 Safari/537.36"

/* 从 token 中切出会话 id：兼容 "tempMailSession=<sid>" 前缀形态与
 * "tempmail_session=<sid>; ..." Cookie 串形态 */
static void tmpkit_session_id(const char *token, char *out, size_t cap) {
  out[0] = '\0';
  if (!token)
    return;
  const char *prefixes[] = {"tempMailSession=", "tempmail_session="};
  for (size_t p = 0; p < 2; p++) {
    const char *hit = strstr(token, prefixes[p]);
    if (hit) {
      const char *val = hit + strlen(prefixes[p]);
      size_t n = 0;
      while (val[n] && val[n] != ';' && val[n] != ' ' && n + 1 < cap)
        n++;
      if (n > 0) {
        memcpy(out, val, n);
        out[n] = '\0';
        return;
      }
    }
  }
}

/* 对 tmpkit 发起 rpc 调用，返回 {"json":{...}} 载荷；失败返回 NULL */
static cJSON *tmpkit_rpc(const char *procedure, cJSON *req_obj,
                         const char *cookie) {
  cJSON *outer_body = cJSON_CreateObject();
  if (!outer_body) {
    if (req_obj)
      cJSON_Delete(req_obj);
    return NULL;
  }
  if (req_obj) {
    cJSON_AddItemToObject(outer_body, "json", req_obj);
  } else {
    cJSON_AddItemToObject(outer_body, "json", cJSON_CreateObject());
  }
  char *body = cJSON_PrintUnformatted(outer_body);
  cJSON_Delete(outer_body);
  if (!body)
    return NULL;

  const char *headers[16];
  int n = 0;
  headers[n++] = "User-Agent: " TMPKIT_UA;
  headers[n++] = "Accept: */*";
  headers[n++] = "Content-Type: application/json";
  headers[n++] = "Origin: " TMPKIT_BASE;
  headers[n++] = "Referer: " TMPKIT_BASE "/en";
  char cookie_hdr[512];
  if (cookie && cookie[0]) {
    snprintf(cookie_hdr, sizeof(cookie_hdr),
             "Cookie: tempmail_session=%s", cookie);
    headers[n++] = cookie_hdr;
  }
  headers[n] = NULL;

  char url[128];
  snprintf(url, sizeof(url), "%s/%s", TMPKIT_RPC, procedure);

  tm_http_response_t *resp = tm_http_request(TM_HTTP_POST, url, headers, body, 15);
  free(body);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("tmpkit: %s 失败 http %ld", procedure, resp ? resp->status : -1L);
    tm_http_response_free(resp);
    return NULL;
  }

  cJSON *outer = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!outer) {
    TM_LOG_ERR("tmpkit: 解析 %s 响应失败", procedure);
    return NULL;
  }
  cJSON *payload = cJSON_DetachItemFromObjectCaseSensitive(outer, "json");
  cJSON_Delete(outer);
  if (!cJSON_IsObject(payload)) {
    if (payload)
      cJSON_Delete(payload);
    TM_LOG_ERR("tmpkit: %s 响应缺 json 载荷", procedure);
    return NULL;
  }
  return payload;
}

/**
 * 创建 tmpkit.com 临时邮箱
 * 调 initSession（{"json":{}}），token 约定为 tempMailSession=<sessionId>。
 */
tm_email_info_t *tm_provider_tmpkit_generate(void) {
  cJSON *data = tmpkit_rpc("initSession", cJSON_CreateObject(), NULL);
  if (!data)
    return NULL;
  cJSON *sess = cJSON_GetObjectItemCaseSensitive(data, "session");
  if (!cJSON_IsObject(sess)) {
    TM_LOG_ERR("tmpkit: 创建会话响应缺 session 字段");
    cJSON_Delete(data);
    return NULL;
  }
  const char *email =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "email"), "");
  const char *session_id =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "sessionId"), "");
  if (!email[0] || !session_id[0] || !strchr(email, '@')) {
    TM_LOG_ERR("tmpkit: 创建会话响应缺少必要字段（email/sessionId）");
    cJSON_Delete(data);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(data);
    return NULL;
  }
  info->channel = CHANNEL_TMPKIT;
  info->email = tm_strdup(email);
  size_t tlen = strlen(session_id) + 32;
  info->token = (char *)malloc(tlen);
  if (info->token)
    snprintf(info->token, tlen, "tempMailSession=%s", session_id);
  cJSON_Delete(data);
  return info;
}

/**
 * 读取 tmpkit.com 收件箱
 * getEmails（offset 0 / limit 20）取摘要，逐封 getEmailDetail 拉详情并
 * 将详情键并入摘要；详情失败回退列表摘要。getEmails 返回的 session.email
 * 与收信邮箱不符时报错（session 为 null 视为会话失效）。
 */
tm_email_t *tm_provider_tmpkit_get_emails(const char *email,
                                          const char *token, int *count) {
  *count = 0;
  if (!token || !token[0])
    return NULL;
  if (!email || !email[0])
    return NULL;

  char mailbox[256];
  tmpkit_session_id(token, mailbox, sizeof(mailbox));
  if (!mailbox[0]) {
    TM_LOG_ERR("tmpkit: 会话 token 为空");
    return NULL;
  }

  cJSON *req = cJSON_CreateObject();
  if (!req)
    return NULL;
  cJSON_AddNumberToObject(req, "offset", 0);
  cJSON_AddNumberToObject(req, "limit", 20);
  cJSON *data = tmpkit_rpc("getEmails", req, mailbox);
  if (!data)
    return NULL;

  /* 会话指向校验：session 非 null 对象时比对 email，否则会话失效 */
  cJSON *sess = cJSON_GetObjectItemCaseSensitive(data, "session");
  if (cJSON_IsObject(sess)) {
    const char *got =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "email"), "");
    if (got[0] && strcmp(got, email) != 0) {
      TM_LOG_ERR("tmpkit: 会话邮箱不匹配（响应 %s，请求 %s）", got, email);
      cJSON_Delete(data);
      return NULL;
    }
  } else {
    TM_LOG_ERR("tmpkit: 会话已失效（getEmails 返回空会话）");
    cJSON_Delete(data);
    return NULL;
  }

  cJSON *list = cJSON_GetObjectItemCaseSensitive(data, "emails");
  if (!cJSON_IsArray(list)) {
    TM_LOG_ERR("tmpkit: 邮件列表响应缺 emails 字段");
    cJSON_Delete(data);
    return NULL;
  }

  int n = cJSON_GetArraySize(list);
  *count = n;
  tm_email_t *emails = tm_emails_new(n > 0 ? n : 1);
  if (!emails) {
    *count = -1;
    cJSON_Delete(data);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *msg = cJSON_GetArrayItem(list, i);
    if (!cJSON_IsObject(msg))
      continue;
    /* mailId 为数字：合法时逐封拉详情（详情键并入摘要，跳过会话类键） */
    cJSON *mid = cJSON_GetObjectItemCaseSensitive(msg, "mailId");
    if (cJSON_IsNumber(mid) && mid->valuedouble > 0) {
      cJSON *dreq = cJSON_CreateObject();
      if (dreq) {
        cJSON_AddNumberToObject(dreq, "mailId", (int)mid->valuedouble);
        cJSON *detail = tmpkit_rpc("getEmailDetail", dreq, mailbox);
        if (detail) {
          cJSON *child = NULL;
          cJSON_ArrayForEach(child, detail) {
            const char *key = child->string;
            if (!key)
              continue;
            if (strcmp(key, "session") == 0 || strcmp(key, "emails") == 0 ||
                strcmp(key, "total") == 0 || strcmp(key, "error") == 0)
              continue;
            cJSON_ReplaceItemInObjectCaseSensitive(msg, key,
                                                   cJSON_Duplicate(child, 1));
          }
          cJSON_Delete(detail);
        }
      }
    }
    emails[i] = tm_normalize_email(msg, email);
  }
  cJSON_Delete(data);
  return emails;
}