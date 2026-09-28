/**
 * nukemail 渠道 — https://nukemail.app
 *
 * SHA-256 Proof-of-Work 建箱（全局 srand 由 client.c 负责）：
 *   1. GET /api/pow/challenge?difficulty=4 → {"id","challenge","difficulty"}
 *      （difficulty<=0 时取 4）。
 *   2. 本地求最小 nonce（自 0 递增）使 SHA-256("<challenge><nonce>") 的
 *      十六进制（小写）以 difficulty 个 "0" 开头（与前端 solvePow 逐字对齐，
 *      使用 OpenSSL SHA256，参照 mail_td.c）。
 *   3. GET /api/domains → {"domains":[{"domain","is_premium_only"}]} 取首个
 *      非 premium。
 *   4. POST /api/inbox/create body {"address":"nuke"+10 位随机 [a-z0-9],
 *      "domain","pow_id","pow_nonce":<十进制字符串>} → {"token":"NUKE-xxx",
 *      "email"}。
 *
 * 读信：GET /api/inbox 带 Cookie: nukemail_token=<token>；
 *   state=="expired" 或空或请求失败时 POST /api/inbox/resume
 *   {"accessCode":token} 后重试一次；messages 元素归一：
 *   to=email 注入、text 无则取 body_text、html 无则取 body_html、
 *   date=received_at、read=read、from=sender_email=sender
 *   （normalize 候选含 sender）。
 */
#include "tempmail_internal.h"
#include <openssl/sha.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define NUKE_BASE "https://nukemail.app"

static const char *nuke_get_headers[] = {
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

static const char *nuke_json_headers[] = {
    "Content-Type: application/json",
    "Accept: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    NULL};

/* 字节数组转小写十六进制（out 容量 >= len*2+1） */
static void nuke_hex(const unsigned char *data, size_t len, char *out) {
  static const char *hexchars = "0123456789abcdef";
  for (size_t i = 0; i < len; i++) {
    out[i * 2] = hexchars[(data[i] >> 4) & 0xF];
    out[i * 2 + 1] = hexchars[data[i] & 0xF];
  }
  out[len * 2] = '\0';
}

/* 求解 PoW：返回使 SHA-256(challenge+nonce) 十六进制前 difficulty 位为 0
 * 的最小 nonce（自 0 递增，无随机起点），与前端 solvePow 逐字对齐。
 * @param challenge 挑战串（不可空）
 * @param difficulty 难度（十六进制前缀 0 位数）
 * @param nonce_out 输出 nonce 十进制字符串（缓冲 >= 32）
 * @return 成功 1，失败 0 */
static int nuke_solve_pow(const char *challenge, int difficulty,
                          char *nonce_out) {
  if (!challenge || difficulty <= 0 || difficulty > 64)
    return 0;
  size_t clen = strlen(challenge);
  for (long long nonce = 0; nonce < 100000000; nonce++) {
    char num[32];
    snprintf(num, sizeof(num), "%lld", nonce);
    size_t nlen = strlen(num);

    /* 拼接 challenge + nonce（十进制字符串，与前端模板字符串一致） */
    char *input = (char *)malloc(clen + nlen + 1);
    if (!input)
      return 0;
    memcpy(input, challenge, clen);
    memcpy(input + clen, num, nlen + 1);

    unsigned char hash[SHA256_DIGEST_LENGTH];
    SHA256((const unsigned char *)input, clen + nlen, hash);
    free(input);

    char hexstr[SHA256_DIGEST_LENGTH * 2 + 1];
    nuke_hex(hash, SHA256_DIGEST_LENGTH, hexstr);
    int ok = 1;
    for (int d = 0; d < difficulty; d++) {
      if (hexstr[d] != '0') {
        ok = 0;
        break;
      }
    }
    if (ok) {
      snprintf(nonce_out, 32, "%lld", nonce);
      return 1;
    }
  }
  return 0;
}

/* 生成随机本地名："nuke"+10 位 [a-z0-9] */
static void nuke_random_address(char *out, size_t cap) {
  static const char chars[] = "abcdefghijklmnopqrstuvwxyz0123456789";
  if (cap < 16)
    return;
  out[0] = 'n';
  out[1] = 'u';
  out[2] = 'k';
  out[3] = 'e';
  for (int i = 0; i < 10; i++)
    out[4 + i] = chars[rand() % (int)(sizeof(chars) - 1)];
  out[14] = '\0';
}

/* 取首个非 premium 域名（GET /api/domains）；失败置 *out 空串 */
static void nuke_domain(char *out, size_t cap) {
  out[0] = '\0';
  char url[128];
  snprintf(url, sizeof(url), "%s/api/domains", NUKE_BASE);
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, nuke_get_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return;
  cJSON *domains = cJSON_GetObjectItemCaseSensitive(root, "domains");
  if (cJSON_IsArray(domains)) {
    const cJSON *d = NULL;
    cJSON_ArrayForEach(d, domains) {
      const cJSON *prem = cJSON_GetObjectItemCaseSensitive(d, "is_premium_only");
      if (cJSON_IsTrue(prem) || (cJSON_IsNumber(prem) && prem->valueint != 0))
        continue;
      const char *dm =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(d, "domain"), "");
      if (dm[0]) {
        snprintf(out, cap, "%s", dm);
        break;
      }
    }
  }
  cJSON_Delete(root);
}

/**
 * 创建 nukemail.app 临时邮箱（PoW 建箱）
 * token 为平台返回的 NUKE-<随机> 访问码，读信时转成
 * nukemail_token Cookie 携带。
 */
tm_email_info_t *tm_provider_nukemail_generate(void) {
  char url[128];
  /* 1) 取 PoW 挑战 */
  snprintf(url, sizeof(url), "%s/api/pow/challenge?difficulty=4", NUKE_BASE);
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, nuke_get_headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("nukemail: 取 PoW 挑战失败 http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *ch = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!ch)
    return NULL;

  const char *ch_id =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(ch, "id"), "");
  const char *challenge =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(ch, "challenge"), "");
  int difficulty = 4;
  {
    const cJSON *dv = cJSON_GetObjectItemCaseSensitive(ch, "difficulty");
    if (cJSON_IsNumber(dv) && dv->valueint > 0)
      difficulty = dv->valueint;
  }
  if (!ch_id[0] || !challenge[0]) {
    TM_LOG_ERR("nukemail: challenge 响应缺少 id/challenge");
    cJSON_Delete(ch);
    return NULL;
  }

  /* 2) 本地求 PoW 解（难度 4 实测通常 < 1s） */
  char nonce_str[32];
  if (!nuke_solve_pow(challenge, difficulty, nonce_str)) {
    TM_LOG_ERR("nukemail: PoW 求解失败");
    cJSON_Delete(ch);
    return NULL;
  }

  /* 3) 取域名并建箱 */
  char domain[128];
  nuke_domain(domain, sizeof(domain));
  if (!domain[0]) {
    TM_LOG_ERR("nukemail: 无可用非 premium 域名");
    cJSON_Delete(ch);
    return NULL;
  }

  char address[80];
  nuke_random_address(address, sizeof(address));

  cJSON *body_obj = cJSON_CreateObject();
  if (!body_obj) {
    cJSON_Delete(ch);
    return NULL;
  }
  cJSON_AddStringToObject(body_obj, "address", address);
  cJSON_AddStringToObject(body_obj, "domain", domain);
  cJSON_AddStringToObject(body_obj, "pow_id", ch_id);
  cJSON_AddStringToObject(body_obj, "pow_nonce", nonce_str);
  char *body = cJSON_PrintUnformatted(body_obj);
  cJSON_Delete(body_obj);
  cJSON_Delete(ch);
  if (!body)
    return NULL;

  snprintf(url, sizeof(url), "%s/api/inbox/create", NUKE_BASE);
  tm_http_response_t *resp2 =
      tm_http_request(TM_HTTP_POST, url, nuke_json_headers, body, 15);
  free(body);
  if (!resp2 || resp2->status < 200 || resp2->status >= 300) {
    TM_LOG_ERR("nukemail: 建箱失败 http %ld", resp2 ? resp2->status : -1);
    tm_http_response_free(resp2);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp2->body);
  tm_http_response_free(resp2);
  if (!root)
    return NULL;

  const char *token =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "token"), "");
  const char *email =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "email"), "");
  if (!token[0] || !email[0]) {
    TM_LOG_ERR("nukemail: 建箱响应缺少 token/email");
    cJSON_Delete(root);
    return NULL;
  }

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_NUKEMAIL;
  info->email = tm_strdup(email);
  info->token = tm_strdup(token);
  cJSON_Delete(root);
  return info;
}

/* 单次读信（GET /api/inbox 带 Cookie），返回浏览响应 JSON；失败 NULL */
static cJSON *nuke_do_inbox(const char *cookie, char *state_out, size_t cap) {
  state_out[0] = '\0';
  char url[128];
  snprintf(url, sizeof(url), "%s/api/inbox", NUKE_BASE);
  size_t len = strlen(cookie) + 16;
  char *cookie_hdr = (char *)malloc(len);
  if (!cookie_hdr)
    return NULL;
  snprintf(cookie_hdr, len, "Cookie: %s", cookie);
  const char *headers[] = {"Accept: application/json",
                           nuke_get_headers[1], cookie_hdr, NULL};
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, headers, NULL, 15);
  free(cookie_hdr);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return NULL;
  const char *state =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "state"), "");
  snprintf(state_out, cap, "%s", state);
  return root;
}

/**
 * 读取 nukemail.app 收件箱
 * GET /api/inbox 带 Cookie: nukemail_token=<token>；state 为 expired/空
 * 或请求失败时经 POST /api/inbox/resume 恢复会话后重试一次。
 */
tm_email_t *tm_provider_nukemail_get_emails(const char *email,
                                            const char *token, int *count) {
  *count = 0;
  if (!email || !email[0] || !token || !token[0])
    return NULL;

  size_t len = strlen(token) + 24;
  char *cookie = (char *)malloc(len);
  if (!cookie)
    return NULL;
  snprintf(cookie, len, "nukemail_token=%s", token);

  char state[64];
  cJSON *root = nuke_do_inbox(cookie, state, sizeof(state));
  if (!root || strcmp(state, "expired") == 0 || state[0] == '\0') {
    cJSON_Delete(root);
    /* 会话恢复：POST /api/inbox/resume {"accessCode":token}（结果忽略） */
    char url[128];
    snprintf(url, sizeof(url), "%s/api/inbox/resume", NUKE_BASE);
    size_t blen = strlen(token) + 32;
    char *body = (char *)malloc(blen);
    if (body) {
      snprintf(body, blen, "{\"accessCode\":\"%s\"}", token);
      tm_http_response_t *rr =
          tm_http_request(TM_HTTP_POST, url, nuke_json_headers, body, 15);
      tm_http_response_free(rr);
      free(body);
    }
    root = nuke_do_inbox(cookie, state, sizeof(state));
  }
  free(cookie);
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
    cJSON *m = cJSON_GetArrayItem(messages, i);
    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;

    /* 收件人注入 */
    cJSON_AddStringToObject(raw, "to", email);

    /* 平台消息字段为 body_html/body_text：text 无则取 body_text、
     * html 无则取 body_html（normalize 候选已含 body_html） */
    const char *textv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "text"), "");
    if (!textv[0]) {
      textv = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "body_text"), "");
      if (textv[0])
        cJSON_AddStringToObject(raw, "text", textv);
    } else {
      cJSON_AddStringToObject(raw, "text", textv);
    }
    const char *htmlv =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "html"), "");
    if (!htmlv[0]) {
      htmlv = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "body_html"), "");
      if (htmlv[0])
        cJSON_AddStringToObject(raw, "html", htmlv);
    } else {
      cJSON_AddStringToObject(raw, "html", htmlv);
    }

    /* from=sender_email=sender（normalize 的 from 候选含 sender） */
    {
      const char *sender =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "sender"), "");
      if (sender[0])
        cJSON_AddStringToObject(raw, "sender", sender);
    }
    {
      const char *sender_name = TM_JSON_STR(
          cJSON_GetObjectItemCaseSensitive(m, "sender_name"), "");
      if (sender_name[0])
        cJSON_AddStringToObject(raw, "sender_name", sender_name);
    }
    /* subject / date=received_at / read=read */
    {
      const char *subj =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "subject"), "");
      if (subj[0])
        cJSON_AddStringToObject(raw, "subject", subj);
    }
    {
      const char *received =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "received_at"), "");
      if (received[0])
        cJSON_AddStringToObject(raw, "date", received);
    }
    {
      const cJSON *rd = cJSON_GetObjectItemCaseSensitive(m, "read");
      if (rd)
        cJSON_AddItemReferenceToObject(raw, "read", rd);
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(root);
  return emails;
}