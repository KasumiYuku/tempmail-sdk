/**
 * temporarymail-com 渠道 — https://temporarymail.com
 *
 * 无认证 REST（key 为空即随机建箱，全局 srand 由 client.c 负责）：
 *   - 建箱 GET /api/?action=requestEmailAccess&key=&value=random，
 *     铺浏览器形态头（Accept/Accept-Language/Sec-Fetch 系列头/Referer/Origin/UA），
 *     响应 {"address","secretKey"}，secretKey 用于后续 checkInbox。
 *   - 读信 GET /api/?action=checkInbox&value=<secretKey>，响应双形态：
 *     空箱 []，有信为 map[id]→元数据对象；按序输出。
 *   - 逐封 POST /api/?action=getEmail&value=<id> 覆盖真实 subject/from
 *     （失败兜底不阻断）；再 GET /view/?i=<id>&width=800 取渲染端点全文，
 *     本地剥标签还原纯文本（<br>/<p> 保换行、实体反转义）。
 *   - 风控策略：读信 403/404 换备用 UA 重试一次；429 记日志提示限流。
 */
#include "tempmail_internal.h"
#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#define strncasecmp _strnicmp
#else
#include <strings.h>
#endif

#define TMPM_BASE "https://temporarymail.com"
#define TMPM_MAIN_UA                                                            \
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "              \
  "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
#define TMPM_ALT_UA                                                             \
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "              \
  "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

/* URL 编码（RFC3986，未保留字符原样） */
static char *tmpm_url_encode(const char *s) {
  if (!s)
    return NULL;
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

/* 将任意 JSON 值规整为字符串（nil/非标量→空串） */
static void tmpm_json_str(const cJSON *v, char *out, size_t cap) {
  out[0] = '\0';
  if (!v)
    return;
  if (cJSON_IsString(v)) {
    snprintf(out, cap, "%s", v->valuestring ? v->valuestring : "");
  } else if (cJSON_IsNumber(v)) {
    if (v->valuedouble == (long long)v->valuedouble)
      snprintf(out, cap, "%lld", (long long)v->valuedouble);
    else
      snprintf(out, cap, "%g", v->valuedouble);
  } else if (cJSON_IsTrue(v)) {
    snprintf(out, cap, "%s", "true");
  } else if (cJSON_IsFalse(v)) {
    snprintf(out, cap, "%s", "false");
  }
}

/* 拉取 checkInbox 响应体（成功返回 strdup 的 body）。
 * 403/404 换备用 UA 重试一次；429 置 rate_limited 并由上层记日志。 */
static char *tmpm_fetch_inbox(const char *token, int *rate_limited) {
  *rate_limited = 0;
  if (!token)
    return NULL;
  char *enc = tmpm_url_encode(token);
  if (!enc)
    return NULL;
  char url[768];
  snprintf(url, sizeof(url), "%s/api/?action=checkInbox&value=%s", TMPM_BASE,
           enc);
  free(enc);

  char ua0[96], ua1[96];
  snprintf(ua0, sizeof(ua0), "User-Agent: %s", TMPM_MAIN_UA);
  snprintf(ua1, sizeof(ua1), "User-Agent: %s", TMPM_ALT_UA);
  const char *uas[2] = {ua0, ua1};
  for (int i = 0; i < 2; i++) {
    const char *headers[] = {
        "Accept: application/json, text/plain, */*",
        "Accept-Language: en-US,en;q=0.9",
        "Sec-Fetch-Site: same-origin",
        "Sec-Fetch-Mode: cors",
        "Sec-Fetch-Dest: empty",
        "Referer: " TMPM_BASE "/",
        "Origin: " TMPM_BASE,
        uas[i],
        NULL};

    tm_http_response_t *resp =
        tm_http_request(TM_HTTP_GET, url, headers, NULL, 15);
    if (!resp)
      return NULL;
    if (resp->status == 429) {
      *rate_limited = 1;
      tm_http_response_free(resp);
      return NULL;
    }
    if (resp->status >= 200 && resp->status < 300) {
      char *body = resp->body ? tm_strdup(resp->body) : NULL;
      tm_http_response_free(resp);
      return body;
    }
    long last = resp->status;
    tm_http_response_free(resp);
    /* 非 403/404 直接失败；403/404 换备用 UA 重试一次后仍失败 */
    if (last != 403 && last != 404)
      return NULL;
  }
  return NULL;
}

/* 拉取详情（POST /api/?action=getEmail），覆盖真实 subject/from；失败返回 -1 */
static int tmpm_fetch_detail(const char *id, char *subject, size_t sub_cap,
                             char *from, size_t from_cap) {
  subject[0] = '\0';
  from[0] = '\0';
  char *enc = tmpm_url_encode(id);
  if (!enc)
    return -1;
  char url[768];
  snprintf(url, sizeof(url), "%s/api/?action=getEmail&value=%s", TMPM_BASE,
           enc);
  free(enc);
  const char *headers[] = {
      "Accept: application/json, text/plain, */*",
      "Accept-Language: en-US,en;q=0.9",
      "Sec-Fetch-Site: same-origin",
      "Sec-Fetch-Mode: cors",
      "Sec-Fetch-Dest: empty",
      "Referer: " TMPM_BASE "/",
      "Origin: " TMPM_BASE,
      "Content-Type: application/json",
      "User-Agent: " TMPM_MAIN_UA,
      NULL};

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return -1;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return -1;

  /* 响应为 {id: {...}} 单元素对象：取第一个对象子节点 */
  cJSON *det = NULL;
  if (cJSON_IsObject(root)) {
    cJSON *first = root->child;
    if (first && cJSON_IsObject(first))
      det = first;
  }
  if (det) {
    const char *s =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(det, "subject"), "");
    const char *f =
        TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(det, "from"), "");
    if (s[0])
      snprintf(subject, sub_cap, "%s", s);
    if (f[0])
      snprintf(from, from_cap, "%s", f);
  }
  cJSON_Delete(root);
  return 0;
}

/* 格式化 JSON 中不符合 C11 语法移植性问题的辅助：
 * 大小写不敏感 strstr（提供跨平台稳定实现，不依赖 GNU strcasestr） */
static const char *tmpm_istrstr(const char *hay, const char *needle) {
  if (!*needle)
    return hay;
  size_t nl = strlen(needle);
  for (const char *p = hay; *p; p++) {
    if (strncasecmp(p, needle, nl) == 0)
      return p;
  }
  return NULL;
}

/* HTML 实体反转义（常用命名实体 + &#NN; 数字实体） */
static char *tmpm_unescape(const char *s) {
  size_t n = strlen(s);
  char *out = (char *)malloc(n + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    if (s[i] != '&') {
      out[o++] = s[i];
      continue;
    }
    if (strncmp(s + i, "&amp;", 5) == 0) {
      out[o++] = '&';
      i += 4;
    } else if (strncmp(s + i, "&lt;", 4) == 0) {
      out[o++] = '<';
      i += 3;
    } else if (strncmp(s + i, "&gt;", 4) == 0) {
      out[o++] = '>';
      i += 3;
    } else if (strncmp(s + i, "&quot;", 6) == 0) {
      out[o++] = '"';
      i += 5;
    } else if (strncmp(s + i, "&apos;", 6) == 0) {
      out[o++] = '\'';
      i += 5;
    } else if (strncmp(s + i, "&nbsp;", 6) == 0) {
      out[o++] = ' ';
      i += 5;
    } else if (s[i + 1] == '#' && isdigit((unsigned char)s[i + 2])) {
      /* &#NN; 数字实体 */
      int v = 0;
      size_t k = i + 2;
      while (s[k] && isdigit((unsigned char)s[k]) && k - i < 12)
        v = v * 10 + (int)(s[k++] - '0');
      if (s[k] == ';' && v >= 32 && v <= 126) {
        out[o++] = (char)v;
        i = k;
      } else {
        out[o++] = s[i];
      }
    } else {
      out[o++] = s[i];
    }
  }
  out[o] = '\0';
  return out;
}

/* 剥标签还原纯文本：<br> 三变体与 <p>/</p>→\n，
 * <script>/<style> 整块删除，其余标签→空格；反转义实体后逐行 trim。 */
static char *tmpm_view_to_text(const char *src, size_t *out_len) {
  *out_len = 0;
  if (!src || !src[0])
    return NULL;
  size_t n = strlen(src);
  char *stage = (char *)malloc(n + 1);
  char *final = (char *)malloc(n + 1);
  if (!stage || !final) {
    free(stage);
    free(final);
    return NULL;
  }

  /* 阶段一：块标签删除 + <br>/<p> 换行 + 其余标签→空格 */
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    if (src[i] == '<') {
      /* script/style 整块：跳到闭合标签之后 */
      if (strncasecmp(src + i, "<script", 7) == 0 ||
          strncasecmp(src + i, "<style", 6) == 0) {
        const char *e1 = tmpm_istrstr(src + i, "</script>");
        const char *e2 = tmpm_istrstr(src + i, "</style>");
        const char *endp = NULL;
        if (e1 && e2)
          endp = (e1 < e2) ? e1 : e2;
        else
          endp = e1 ? e1 : e2;
        if (endp) {
          i = (size_t)(endp - src) + 8; /* 越过 </script>（含 >） */
          stage[o++] = ' ';
          continue;
        }
        /* 无闭合标签：按普通标签处理（吞到 >） */
      }
      if (strncmp(src + i, "<br />", 6) == 0) {
        stage[o++] = '\n';
        i += 5;
        continue;
      }
      if (strncmp(src + i, "<br/>", 5) == 0) {
        stage[o++] = '\n';
        i += 4;
        continue;
      }
      if (strncmp(src + i, "<br>", 4) == 0) {
        stage[o++] = '\n';
        i += 3;
        continue;
      }
      if (strncasecmp(src + i, "<p>", 3) == 0) {
        stage[o++] = '\n';
        i += 2;
        continue;
      }
      if (strncasecmp(src + i, "</p>", 4) == 0) {
        stage[o++] = '\n';
        i += 3;
        continue;
      }
      /* 其余标签：吞到 > */
      while (i < n && src[i] != '>')
        i++;
      stage[o++] = ' ';
      continue;
    }
    /* 文本区：常见实体预替换（nbsp/gt/lt/amp/quot，与 Go replacer 对齐） */
    if (src[i] == '&') {
      if (strncmp(src + i, "&nbsp;", 6) == 0) {
        stage[o++] = ' ';
        i += 5;
        continue;
      }
      if (strncmp(src + i, "&gt;", 4) == 0) {
        stage[o++] = '>';
        i += 3;
        continue;
      }
      if (strncmp(src + i, "&lt;", 4) == 0) {
        stage[o++] = '<';
        i += 3;
        continue;
      }
      if (strncmp(src + i, "&amp;", 5) == 0) {
        stage[o++] = '&';
        i += 4;
        continue;
      }
      if (strncmp(src + i, "&quot;", 6) == 0) {
        stage[o++] = '"';
        i += 5;
        continue;
      }
    }
    stage[o++] = src[i];
  }
  stage[o] = '\0';

  /* 阶段二：反转义剩余实体，逐行 trim 输出 */
  char *u = tmpm_unescape(stage);
  free(stage);
  if (!u)
    return NULL;
  {
    size_t f = 0;
    char *line_start = u;
    for (char *p = u;; p++) {
      if (*p == '\n' || *p == '\0') {
        char save = *p;
        *p = '\0';
        char *ls = line_start;
        while (*ls == ' ' || *ls == '\t')
          ls++;
        char *le = ls + strlen(ls);
        while (le > ls && (le[-1] == ' ' || le[-1] == '\t'))
          *--le = '\0';
        if (le > ls) {
          if (f > 0)
            final[f++] = '\n';
          size_t ll = (size_t)(le - ls);
          memcpy(final + f, ls, ll);
          f += ll;
        }
        if (save == '\0')
          break;
        line_start = p + 1;
      }
    }
    final[f] = '\0';
    *out_len = f;
  }
  free(u);
  return final;
}

/* 拉取 /view/ 渲染端点全文并还原为纯文本；失败返回 NULL */
static char *tmpm_fetch_view(const char *id, size_t *out_len) {
  *out_len = 0;
  char *enc = tmpm_url_encode(id);
  if (!enc)
    return NULL;
  char url[768];
  snprintf(url, sizeof(url), "%s/view/?i=%s&width=800", TMPM_BASE, enc);
  free(enc);
  const char *headers[] = {
      "Accept: text/html, */*",
      "User-Agent: " TMPM_MAIN_UA,
      "Referer: " TMPM_BASE "/",
      NULL};
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return NULL;
  }
  char *text = tmpm_view_to_text(resp->body ? resp->body : "", out_len);
  tm_http_response_free(resp);
  return text;
}

/**
 * 创建 temporarymail.com 临时邮箱
 * GET /api/?action=requestEmailAccess&key=&value=random，token 复用 secretKey
 */
tm_email_info_t *tm_provider_temporarymail_com_generate(void) {
  char url[256];
  snprintf(url, sizeof(url),
           "%s/api/?action=requestEmailAccess&key=&value=random", TMPM_BASE);
  const char *headers[] = {
      "Accept: application/json, text/plain, */*",
      "Accept-Language: en-US,en;q=0.9",
      "Sec-Fetch-Site: same-origin",
      "Sec-Fetch-Mode: cors",
      "Sec-Fetch-Dest: empty",
      "Referer: " TMPM_BASE "/",
      "Origin: " TMPM_BASE,
      "User-Agent: " TMPM_MAIN_UA,
      NULL};

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, headers, NULL, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("temporarymail-com: 建箱失败 http %ld",
               resp ? resp->status : -1);
    tm_http_response_free(resp);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    TM_LOG_ERR("temporarymail-com: 解析建箱响应失败");
    return NULL;
  }
  const char *address =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "address"), "");
  const char *secret =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "secretKey"), "");
  if (!address[0] || !secret[0]) {
    TM_LOG_ERR("temporarymail-com: 建箱响应缺少 address/secretKey");
    cJSON_Delete(root);
    return NULL;
  }
  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    cJSON_Delete(root);
    return NULL;
  }
  info->channel = CHANNEL_TEMPORARYMAIL_COM;
  info->email = tm_strdup(address);
  info->token = tm_strdup(secret);
  cJSON_Delete(root);
  return info;
}

/**
 * 读取 temporarymail.com 收件箱
 * checkInbox 双形态响应（[] 空箱 / map[id]→对象），逐封详情覆盖
 * subject/from、/view/ 端点补全文；失败各自兜底不阻断。
 */
tm_email_t *tm_provider_temporarymail_com_get_emails(const char *email,
                                                     const char *token,
                                                     int *count) {
  *count = 0;
  if (!email || !email[0] || !token || !token[0])
    return NULL;

  int rate_limited = 0;
  char *body = tmpm_fetch_inbox(token, &rate_limited);
  if (!body) {
    if (rate_limited)
      TM_LOG_ERR("temporarymail-com: 读取收件箱 429 限流，请拉大轮询间隔");
    return NULL;
  }
  cJSON *root = cJSON_Parse(body);
  free(body);
  if (!root)
    return NULL;

  /* 双形态归一为数组视图：[] 直接用；对象则按序遍历并入临时数组 */
  cJSON *list = NULL;
  int list_is_root = 0;
  if (cJSON_IsArray(root)) {
    list = root;
    list_is_root = 1;
  } else if (cJSON_IsObject(root)) {
    list = cJSON_CreateArray();
    if (list) {
      cJSON *it = NULL;
      cJSON_ArrayForEach(it, root) { cJSON_AddItemReferenceToArray(list, it); }
    }
  }
  if (!list) {
    TM_LOG_ERR("temporarymail-com: 解析收件箱响应失败（secretKey 可能已失效）");
    cJSON_Delete(root);
    return NULL;
  }

  int n = cJSON_GetArraySize(list);
  if (n == 0) {
    cJSON_Delete(list);
    if (!list_is_root)
      cJSON_Delete(root);
    return NULL;
  }
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    cJSON_Delete(list);
    if (!list_is_root)
      cJSON_Delete(root);
    *count = -1;
    return NULL;
  }

  for (int i = 0; i < n; i++) {
    cJSON *msg = cJSON_GetArrayItem(list, i);
    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;

    /* 收件人注入（平台列表元素无 to 字段） */
    cJSON_AddStringToObject(raw, "to", email);

    char idbuf[128];
    tmpm_json_str(cJSON_GetObjectItemCaseSensitive(msg, "id"), idbuf,
                  sizeof(idbuf));

    if (idbuf[0]) {
      cJSON_AddStringToObject(raw, "id", idbuf);
      /* 详情覆盖真实 subject/from（失败不致命） */
      char dsub[512], dfrom[512];
      if (tmpm_fetch_detail(idbuf, dsub, sizeof(dsub), dfrom,
                            sizeof(dfrom)) == 0) {
        if (dsub[0])
          cJSON_AddStringToObject(raw, "subject", dsub);
        if (dfrom[0])
          cJSON_AddStringToObject(raw, "from", dfrom);
      }
      /*/view/ 渲染端点全文（失败不致命） */
      size_t tlen = 0;
      char *text = tmpm_fetch_view(idbuf, &tlen);
      if (text && tlen > 0)
        cJSON_AddStringToObject(raw, "text", text);
      free(text);
    }

    /* 列表元数据兜底（详情未覆盖时） */
    if (cJSON_GetObjectItemCaseSensitive(raw, "subject") == NULL) {
      const char *subj =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "subject"), "");
      if (subj[0])
        cJSON_AddStringToObject(raw, "subject", subj);
    }
    if (cJSON_GetObjectItemCaseSensitive(raw, "from") == NULL) {
      const char *fromv =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "from"), "");
      if (fromv[0])
        cJSON_AddStringToObject(raw, "from", fromv);
    }
    {
      const char *datev =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(msg, "date"), "");
      if (datev[0])
        cJSON_AddStringToObject(raw, "date", datev);
    }

    emails[i] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
  }

  cJSON_Delete(list);
  if (!list_is_root)
    cJSON_Delete(root);
  return emails;
}