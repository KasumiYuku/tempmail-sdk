/**
 * mailticking 渠道 — https://www.mailticking.com
 *
 * POST JSON 协议（全局 srand 由 client.c 负责；站点在 Cloudflare 后，
 * 低频调用不受 Turnstile 影响）：
 *   - 建箱 POST /get-mailbox body {"types":["4"]}（4=独立域名）→
 *     {"success":true,"email","code"}；code 空则 token=email。
 *   - 激活 POST /activate-email body {"email","source":"api",
 *     "activate_token":<token>} → {"success":true}（失败视为邮箱不可用）。
 *   - 列信 POST /get-emails?lang=en body {"email","code":<token>} →
 *     {"success":true,"emails":[]}；success 非真且 needNewEmail=true
 *     报换箱语义错误。
 *   - 请求头：Accept/Accept-Language en-US,en;q=0.9/Content-Type
 *     application/json/UA + Referer/Origin 取当前主机。
 *   - 列表字段多候选：mail_from/from_mail/from_email/sender_address/
 *     from_address/send_addr/mail_addr/address_from/ho_from/fromname/fromS
 *     → from；sender 缺取 from；id 缺取 mail_id；date 缺取 received_at；
 *     其余字段交 tm_normalize_email 既有候选。
 *   - 平台限制：无公开读信正文端点，本渠道为「仅列表」形态
 *     （与 Go 端 NotAvailable 语义一致）。
 */
#include "tempmail_internal.h"
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define MAILTICK_BASE "https://www.mailticking.com"

static const char *mailtick_json_headers[] = {
    "Accept: application/json",
    "Accept-Language: en-US,en;q=0.9",
    "Content-Type: application/json",
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
    "Referer: " MAILTICK_BASE "/",
    "Origin: " MAILTICK_BASE,
    NULL};

/* JSON 字符串转义（简易：仅转义 " 与 \） */
static void mailtick_escape(const char *src, char *out, size_t cap) {
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

/* 通用 JSON POST：执行请求并解析响应；*out 置响应 JSON（调用方释放）。
 * 非 2xx 时优先提取响应体错误文案记日志。返回 0=成功，-1=失败。 */
static int mailtick_do(const char *path, const char *json_body, cJSON **out) {
  *out = NULL;
  char url[384];
  snprintf(url, sizeof(url), "%s%s", MAILTICK_BASE, path);

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, mailtick_json_headers, json_body, 15);
  if (!resp)
    return -1;
  cJSON *root = cJSON_Parse(resp->body ? resp->body : "");
  if (!root) {
    /* 站点偶尔返回非 JSON 网关文案 */
    if (resp->status < 200 || resp->status >= 300)
      TM_LOG_ERR("mailticking: http %ld: %s", resp->status,
                 resp->body ? resp->body : "");
    else
      TM_LOG_ERR("mailticking: 解析响应失败");
    tm_http_response_free(resp);
    return -1;
  }
  if (resp->status < 200 || resp->status >= 300) {
    const char *msg =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "error"), "");
    if (!msg[0])
      msg = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "message"), "");
    if (!msg[0])
      msg = resp->body ? resp->body : "";
    TM_LOG_ERR("mailticking: http %ld: %s", resp->status, msg);
    cJSON_Delete(root);
    tm_http_response_free(resp);
    return -1;
  }
  tm_http_response_free(resp);
  *out = root;
  return 0;
}

/* 提取响应中的错误文案（error→message→默认串） */
static const char *mailtick_err_msg(const cJSON *root) {
  const char *msg =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "error"), "");
  if (!msg[0])
    msg = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "message"), "");
  if (!msg[0])
    msg = "unknown error";
  return msg;
}

/**
 * 创建 mailticking.com 临时邮箱
 * POST /get-mailbox {"types":["4"]} → activate-email 激活会话；
 * token=code（code 空则回退 email）。
 */
tm_email_info_t *tm_provider_mailticking_generate(void) {
  cJSON *box = NULL;
  if (mailtick_do("/get-mailbox", "{\"types\":[\"4\"]}", &box) != 0)
    return NULL;

  int success = 0;
  {
    const cJSON *s = cJSON_GetObjectItemCaseSensitive(box, "success");
    if (cJSON_IsTrue(s))
      success = 1;
  }
  if (!success) {
    TM_LOG_ERR("mailticking: get-mailbox 失败: %s", mailtick_err_msg(box));
    cJSON_Delete(box);
    return NULL;
  }
  const char *email =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(box, "email"), "");
  const char *code =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(box, "code"), "");
  {
    /* 去除首尾空白以对齐 Go 端 TrimSpace 语义 */
    size_t el = strlen(email);
    while (el > 0 && (email[el - 1] == ' ' || email[el - 1] == '\t' ||
                      email[el - 1] == '\n'))
      el--;
    const char *ep = email;
    while (*ep == ' ' || *ep == '\t' || *ep == '\n')
      ep++;
    size_t cl = strlen(code);
    const char *cp = code;
    while (*cp == ' ' || *cp == '\t' || *cp == '\n')
      cp++;
    while (cl > 0 && (code[cl - 1] == ' ' || code[cl - 1] == '\t' ||
                      code[cl - 1] == '\n'))
      cl--;
    char mail[256], tok[256];
    snprintf(mail, sizeof(mail), "%.*s", (int)(el - (size_t)(ep - email)) > 0
                                             ? (int)(el - (size_t)(ep - email))
                                             : 0,
             ep);
    snprintf(tok, sizeof(tok), "%.*s",
             (int)(cl - (size_t)(cp - code)) > 0
                 ? (int)(cl - (size_t)(cp - code))
                 : 0,
             cp);
    if (!mail[0]) {
      TM_LOG_ERR("mailticking: get-mailbox 返回空 email");
      cJSON_Delete(box);
      return NULL;
    }
    /* token 必须携带 activate_token；为空时回退 email */
    {
      const char *token = tok[0] ? tok : mail;
      char e_esc[512], t_esc[512];
      mailtick_escape(mail, e_esc, sizeof(e_esc));
      mailtick_escape(token, t_esc, sizeof(t_esc));
      char body[1024];
      snprintf(body, sizeof(body),
               "{\"email\":\"%s\",\"source\":\"api\",\"activate_token\":\"%s\"}",
               e_esc, t_esc);

      cJSON *act = NULL;
      if (mailtick_do("/activate-email", body, &act) != 0) {
        cJSON_Delete(box);
        return NULL;
      }
      int as_ok = 0;
      {
        const cJSON *s = cJSON_GetObjectItemCaseSensitive(act, "success");
        if (cJSON_IsTrue(s))
          as_ok = 1;
      }
      if (!as_ok) {
        TM_LOG_ERR("mailticking: activate-email 失败: %s",
                   mailtick_err_msg(act));
        cJSON_Delete(act);
        cJSON_Delete(box);
        return NULL;
      }
      cJSON_Delete(act);

      tm_email_info_t *info = tm_email_info_new();
      if (!info) {
        cJSON_Delete(box);
        return NULL;
      }
      info->channel = CHANNEL_MAILTICKING;
      info->email = tm_strdup(mail);
      info->token = tm_strdup(token);
      cJSON_Delete(box);
      return info;
    }
  }
}

/* 多候选字段迁移：from 候选链 → from；sender 缺取 from；
 * id 缺取 mail_id；date 缺取 received_at；其余交 normalize。 */
static void mailtick_flat(cJSON *raw, const cJSON *item) {
  static const char *from_keys[] = {
      "mail_from",   "from_mail",    "from_email",  "sender_address",
      "from_address", "send_addr",    "mail_addr",    "address_from",
      "ho_from",     "fromname",     "fromS",        NULL};
  /* from 候选链：首个非空字符串命中 */
  {
    const cJSON *found = NULL;
    for (int i = 0; from_keys[i]; i++) {
      const cJSON *v = cJSON_GetObjectItemCaseSensitive(item, from_keys[i]);
      if (cJSON_IsString(v) && v->valuestring && v->valuestring[0]) {
        found = v;
        break;
      }
    }
    if (found) {
      if (cJSON_GetObjectItemCaseSensitive(raw, "from") == NULL)
        cJSON_AddStringToObject(raw, "from", found->valuestring);
    }
    /* sender 缺取 from */
    if (cJSON_GetObjectItemCaseSensitive(raw, "sender") == NULL) {
      const cJSON *fv = cJSON_GetObjectItemCaseSensitive(raw, "from");
      if (cJSON_IsString(fv) && fv->valuestring && fv->valuestring[0])
        cJSON_AddStringToObject(raw, "sender", fv->valuestring);
    }
  }
  /* id 缺取 mail_id */
  if (cJSON_GetObjectItemCaseSensitive(raw, "id") == NULL) {
    const cJSON *mid = cJSON_GetObjectItemCaseSensitive(item, "mail_id");
    if (mid)
      cJSON_AddItemReferenceToObject(raw, "id", (cJSON *)mid);
  }
  /* date 缺取 received_at */
  if (cJSON_GetObjectItemCaseSensitive(raw, "date") == NULL) {
    const cJSON *ra = cJSON_GetObjectItemCaseSensitive(item, "received_at");
    if (ra)
      cJSON_AddItemReferenceToObject(raw, "date", (cJSON *)ra);
  }
  /* 其余字段原样透传（normalize 既有候选提取） */
  {
    const cJSON *child = NULL;
    cJSON_ArrayForEach(child, item) {
      const char *key = child->string;
      if (!key)
        continue;
      if (cJSON_GetObjectItemCaseSensitive(raw, key) == NULL)
        cJSON_AddItemReferenceToObject(raw, key, child);
    }
  }
}

/**
 * 读取 mailticking.com 邮件列表
 * POST /get-emails?lang=en {"email","code"}；needNewEmail=true 报换箱
 * 语义错误；空箱返回空列表。
 */
tm_email_t *tm_provider_mailticking_get_emails(const char *email,
                                               const char *token, int *count) {
  *count = 0;
  if (!email || !email[0])
    return NULL;
  if (!token || !token[0]) {
    TM_LOG_ERR("mailticking: activate code 为空");
    return NULL;
  }

  char e_esc[512], t_esc[512];
  mailtick_escape(email, e_esc, sizeof(e_esc));
  mailtick_escape(token, t_esc, sizeof(t_esc));
  char body[1024];
  snprintf(body, sizeof(body), "{\"email\":\"%s\",\"code\":\"%s\"}", e_esc,
           t_esc);

  cJSON *list = NULL;
  if (mailtick_do("/get-emails?lang=en", body, &list) != 0)
    return NULL;

  int success = 0;
  {
    const cJSON *s = cJSON_GetObjectItemCaseSensitive(list, "success");
    if (cJSON_IsTrue(s))
      success = 1;
  }
  if (!success) {
    const cJSON *nn = cJSON_GetObjectItemCaseSensitive(list, "needNewEmail");
    if (cJSON_IsTrue(nn))
      TM_LOG_ERR("mailticking: 邮箱已过期，请重新建箱");
    else
      TM_LOG_ERR("mailticking: get-emails 失败: %s", mailtick_err_msg(list));
    cJSON_Delete(list);
    return NULL;
  }

  cJSON *emails_arr = cJSON_GetObjectItemCaseSensitive(list, "emails");
  if (!cJSON_IsArray(emails_arr) || cJSON_GetArraySize(emails_arr) == 0) {
    cJSON_Delete(list);
    return NULL;
  }

  int n = cJSON_GetArraySize(emails_arr);
  *count = n;
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(list);
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *item = cJSON_GetArrayItem(emails_arr, i);
    if (!cJSON_IsObject(item))
      continue;
    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;
    mailtick_flat(raw, item);
    /* 收件人注入 */
    if (cJSON_GetObjectItemCaseSensitive(raw, "to") == NULL)
      cJSON_AddStringToObject(raw, "to", email);
    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(list);
  return emails;
}