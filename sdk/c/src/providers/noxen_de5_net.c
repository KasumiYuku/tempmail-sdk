/**
 * noxen-de5-net 渠道 — https://tempmail.noxen.de5.net
 *
 * UniMail-Bot 公共实例，JWT Cookie 会话（全局 srand 由 client.c 负责）：
 *   - 登录 POST /api/login {"username":"guest","password":"123456"} →
 *     200 + Set-Cookie: iding-session=<JWT>（分号截断、前缀匹配提取）。
 *   - 建箱 GET /api/generate（带 Cookie）→ {"email","expires"(毫秒)}；
 *     token 格式 "noxen-de5-net|" + URL编码(session) + "|base=<基址>"。
 *   - 读信：GET /api/session 校验 {"authenticated":true}（失调用重登录）；
 *     GET /api/emails?mailbox=<urlenc>&limit=20 取列表（带 Cookie）；
 *     每封 GET /api/email/<id> 详情；download 非空则 GET 相对路径拉
 *     原始 EML（Accept: message/rfc822），本地解析 MIME：
 *     CRLF→LF、折行续行头、boundary 切 part、递归 multipart/*
 *     与 message/rfc822、跳过 rfc822-headers、base64/quoted-printable
 *     解码、text/html 与其余分别入槽、html 空时整体抓 <html>…</html>。
 *   - 全文解析失败时正文=合成占位（"验证码: xxx" 置顶 + preview）。
 *   - 读信 401 报「会话失效或非本会话邮箱」。
 */
#include "tempmail_internal.h"
#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#ifdef _WIN32
#define strcasecmp _stricmp
#define strncasecmp _strnicmp
#else
#include <strings.h>
#endif

#define NOXEN_BASE "https://tempmail.noxen.de5.net"
#define NOXEN_USER "guest"
#define NOXEN_PASS "123456"
#define NOXEN_TOKEN_PREFIX "noxen-de5-net|"

static const char *noxen_ua =
    "User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
    "(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36";

/* ========== 通用字符串工具 ========== */

/* 去首尾空白（原地修改，返回首地址） */
static char *noxen_trim(char *s) {
  char *p = s;
  while (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')
    p++;
  size_t n = strlen(p);
  while (n > 0 && (p[n - 1] == ' ' || p[n - 1] == '\t' || p[n - 1] == '\n' ||
                   p[n - 1] == '\r'))
    p[--n] = '\0';
  return p;
}

/* 去首尾空白的动态副本 */
static char *noxen_trim_dup(const char *s) {
  if (!s)
    return NULL;
  char *d = tm_strdup(s);
  char *t = noxen_trim(d);
  if (t != d)
    memmove(d, t, strlen(t) + 1);
  return d;
}

/* 小写化字符串（原地） */
static void noxen_lower(char *s) {
  for (; *s; s++)
    *s = (char)tolower((unsigned char)*s);
}

/* 大小写不敏感 strstr */
static const char *noxen_istrstr(const char *hay, const char *needle) {
  if (!*needle)
    return hay;
  size_t nl = strlen(needle);
  for (const char *p = hay; *p; p++) {
    if (strncasecmp(p, needle, nl) == 0)
      return p;
  }
  return NULL;
}

/* 大小写不敏感的末尾查找（最后一次出现），无匹配返回 NULL */
static const char *noxen_irfind(const char *hay, const char *needle) {
  size_t nl = strlen(needle);
  size_t hl = strlen(hay);
  if (nl > hl)
    return NULL;
  for (size_t start = hl - nl + 1; start > 0; start--) {
    if (strncasecmp(hay + start - 1, needle, nl) == 0)
      return hay + start - 1;
  }
  return NULL;
}

/* query 转义（空格→+，其余非保留字节 %XX 大写，与 Go url.QueryEscape 对齐） */
static char *noxen_query_escape(const char *s) {
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
    } else if (c == ' ') {
      out[o++] = '+';
    } else {
      out[o++] = '%';
      out[o++] = hex[(c >> 4) & 0x0F];
      out[o++] = hex[c & 0x0F];
    }
  }
  out[o] = '\0';
  return out;
}

/* query 反解（+→空格、%XX→字节、其余原样） */
static char *noxen_query_unescape(const char *s) {
  size_t n = strlen(s);
  char *out = (char *)malloc(n + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    if (s[i] == '+') {
      out[o++] = ' ';
    } else if (s[i] == '%' && i + 2 < n &&
               isxdigit((unsigned char)s[i + 1]) &&
               isxdigit((unsigned char)s[i + 2])) {
      int hv = isdigit((unsigned char)s[i + 1])
                   ? s[i + 1] - '0'
                   : (tolower((unsigned char)s[i + 1]) - 'a' + 10);
      int lv = isdigit((unsigned char)s[i + 2])
                   ? s[i + 2] - '0'
                   : (tolower((unsigned char)s[i + 2]) - 'a' + 10);
      out[o++] = (char)((hv << 4) | lv);
      i += 2;
    } else {
      out[o++] = s[i];
    }
  }
  out[o] = '\0';
  return out;
}

/* ========== 会话 ========== */

/* 组装带 Cookie 的请求头数组（动态，调用方 free 指针数组本身、
 * free 其 [2] Cookie 头）。 */
static char **noxen_mk_headers(const char *cookie) {
  int has_cookie = cookie && cookie[0];
  int n = 2 + (has_cookie ? 1 : 0);
  char **h = (char **)calloc((size_t)n + 1, sizeof(char *));
  if (!h)
    return NULL;
  h[0] = (char *)"Accept: application/json";
  h[1] = (char *)noxen_ua;
  if (has_cookie) {
    size_t len = strlen(cookie) + 16;
    h[2] = (char *)malloc(len);
    if (!h[2]) {
      free(h);
      return NULL;
    }
    snprintf(h[2], len, "Cookie: %s", cookie);
  }
  h[n] = NULL;
  return h;
}

/* 释放 mk_headers 的结果 */
static void noxen_free_headers(char **h) {
  if (!h)
    return;
  for (int i = 0; h[i]; i++) {
    if (h[i] != (char *)"Accept: application/json" && h[i] != noxen_ua)
      free(h[i]);
  }
  free(h);
}

/* 登录取得会话 Cookie（"iding-session=<JWT>"，分号截断、前缀匹配）；
 * 429 时置 rate_limited。返回 malloc 字符串（调用方 free），失败 NULL。 */
static char *noxen_login(int *rate_limited) {
  *rate_limited = 0;
  char url[128];
  snprintf(url, sizeof(url), "%s/api/login", NOXEN_BASE);
  const char *headers[] = {
      "Content-Type: application/json",
      "Accept: application/json",
      noxen_ua,
      NULL};
  const char *body =
      "{\"username\":\"" NOXEN_USER "\",\"password\":\"" NOXEN_PASS "\"}";

  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_POST, url, headers, body, 15);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    if (resp && resp->status == 429)
      *rate_limited = 1;
    tm_http_response_free(resp);
    return NULL;
  }

  /* 从 Set-Cookie 拼接串（"; " 分隔）中提取 iding-session：分号截断 +
   * 前缀匹配（http.c 已按首个分号截断 name=value） */
  char *session = NULL;
  const char *cookies = resp->cookies ? resp->cookies : "";
  const char *seg = cookies;
  for (;;) {
    const char *semi = strstr(seg, "; ");
    size_t seglen = semi ? (size_t)(semi - seg) : strlen(seg);
    const char *p = seg;
    while ((size_t)(p - seg) < seglen && (*p == ' ' || *p == '\t'))
      p++;
    const char *end = seg + seglen;
    while (end > p && (end[-1] == ' ' || end[-1] == '\t'))
      end--;
    if ((size_t)(end - p) >= strlen("iding-session=") &&
        strncmp(p, "iding-session=", strlen("iding-session=")) == 0) {
      const char *v = p + strlen("iding-session=");
      size_t vlen = (size_t)(end - v);
      session = (char *)malloc(vlen + 1);
      if (session) {
        memcpy(session, v, vlen);
        session[vlen] = '\0';
      }
      break;
    }
    if (!semi)
      break;
    seg = semi + 2;
  }
  tm_http_response_free(resp);
  if (!session || !session[0]) {
    free(session);
    TM_LOG_ERR("noxen-de5-net: 登录响应未下发会话 Cookie");
    return NULL;
  }
  return session;
}

/* 校验会话 Cookie 是否有效（GET /api/session 返回 authenticated:true） */
static int noxen_session_valid(const char *cookie) {
  if (!cookie || !cookie[0])
    return 0;
  char url[128];
  snprintf(url, sizeof(url), "%s/api/session", NOXEN_BASE);
  char **h = noxen_mk_headers(cookie);
  if (!h)
    return 0;
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  noxen_free_headers(h);
  if (!resp || resp->status != 200) {
    tm_http_response_free(resp);
    return 0;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return 0;
  int ok = 0;
  const cJSON *auth = cJSON_GetObjectItemCaseSensitive(root, "authenticated");
  if (cJSON_IsTrue(auth))
    ok = 1;
  cJSON_Delete(root);
  return ok;
}

/* ========== 建箱 ========== */

/* 校验域名是否在平台域名池内（GET /api/domains 返回字符串数组） */
static int noxen_domain_in_pool(const char *domain) {
  if (!domain || !domain[0])
    return 1;
  char url[128];
  snprintf(url, sizeof(url), "%s/api/domains", NOXEN_BASE);
  char **h = noxen_mk_headers(NULL);
  if (!h)
    return 0;
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  noxen_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return 0;
  }
  cJSON *arr = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!cJSON_IsArray(arr)) {
    cJSON_Delete(arr);
    return 0;
  }
  int found = 0;
  const cJSON *d = NULL;
  cJSON_ArrayForEach(d, arr) {
    if (cJSON_IsString(d) && d->valuestring &&
        strcasecmp(d->valuestring, domain) == 0) {
      found = 1;
      break;
    }
  }
  cJSON_Delete(arr);
  return found;
}

/**
 * 创建 tempmail.noxen.de5.net 临时邮箱
 * 登录 → GET /api/generate（Cookie）→ token 凭据串携带编码后的会话；
 * expires 毫秒原样写入 expires_at（毫秒时间戳），RFC3339 UTC 形式写入
 * created_at。
 * @param domain 可选首选域名（NULL 或空串时平台自动选域；指定但不在
 *               平台域名池内则失败）
 */
tm_email_info_t *tm_provider_noxen_de5_net_generate(const char *domain) {
  if (domain && domain[0]) {
    char *want = noxen_trim_dup(domain);
    if (!want)
      return NULL;
    if (!noxen_domain_in_pool(want)) {
      TM_LOG_ERR("noxen-de5-net: 域名 %s 不在平台域名池", want);
      free(want);
      return NULL;
    }
    free(want);
  }

  int rate_limited = 0;
  char *session = noxen_login(&rate_limited);
  if (!session) {
    TM_LOG_ERR("noxen-de5-net: 登录失败%s",
               rate_limited ? "（429 限流）" : "");
    return NULL;
  }

  char url[128];
  snprintf(url, sizeof(url), "%s/api/generate", NOXEN_BASE);
  char **h = noxen_mk_headers(session);
  if (!h) {
    free(session);
    return NULL;
  }
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  noxen_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    TM_LOG_ERR("noxen-de5-net: 建箱失败 http %ld", resp ? resp->status : -1);
    tm_http_response_free(resp);
    free(session);
    return NULL;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root) {
    free(session);
    return NULL;
  }

  const char *email =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "email"), "");
  const cJSON *exp = cJSON_GetObjectItemCaseSensitive(root, "expires");
  long long expires_ms =
      (cJSON_IsNumber(exp) && exp->valuedouble > 0)
          ? (long long)exp->valuedouble
          : 0;
  if (!email[0]) {
    TM_LOG_ERR("noxen-de5-net: 建箱响应缺少 email");
    cJSON_Delete(root);
    free(session);
    return NULL;
  }

  /* token = 前缀 + URL编码(session) + |base=<基址> */
  char *enc = noxen_query_escape(session);
  size_t need = strlen(NOXEN_TOKEN_PREFIX) + (enc ? strlen(enc) : 0) +
                strlen("|base=") + strlen(NOXEN_BASE) + 8;
  char *token = (char *)malloc(need);
  if (!token) {
    free(enc);
    cJSON_Delete(root);
    free(session);
    return NULL;
  }
  snprintf(token, need, "%s%s|base=%s", NOXEN_TOKEN_PREFIX,
           enc ? enc : session, NOXEN_BASE);
  free(enc);

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    free(token);
    cJSON_Delete(root);
    free(session);
    return NULL;
  }
  info->channel = CHANNEL_NOXEN_DE5_NET;
  info->email = tm_strdup(email);
  info->token = token;
  info->expires_at = expires_ms; /* 毫秒时间戳（C 端字段语义） */
  {
    char rfc[48];
    time_t sec = (time_t)(expires_ms / 1000);
    struct tm tmu;
#ifdef _WIN32
    if (gmtime_s(&tmu, &sec) != 0)
      memset(&tmu, 0, sizeof(tmu));
#else
    gmtime_r(&sec, &tmu);
#endif
    strftime(rfc, sizeof(rfc), "%Y-%m-%dT%H:%M:%SZ", &tmu);
    info->created_at = tm_strdup(expires_ms > 0 ? rfc : "");
  }
  free(session);
  cJSON_Delete(root);
  return info;
}

/* ========== 原始 EML 拉取与 MIME 解析 ========== */

/* 头部键值表（固定容量，val 限制 4KB 防恶意超长） */
#define NOXEN_HDR_MAX 64
typedef struct {
  char key[96];
  char val[4096];
} noxen_hdr_t;
typedef struct {
  noxen_hdr_t items[NOXEN_HDR_MAX];
  int cnt;
} noxen_hdrs_t;

/* 取头部值（小写键匹配），无则 NULL */
static const char *noxen_hdr_get(const noxen_hdrs_t *h, const char *key) {
  for (int i = 0; i < h->cnt; i++) {
    if (strcmp(h->items[i].key, key) == 0)
      return h->items[i].val;
  }
  return NULL;
}

/* 追加头部（小写键；重复键后值覆盖），返回 0=成功 */
static int noxen_hdr_set(noxen_hdrs_t *h, const char *key, const char *val) {
  char lk[96];
  snprintf(lk, sizeof(lk), "%s", key);
  noxen_lower(lk);
  for (int i = 0; i < h->cnt; i++) {
    if (strcmp(h->items[i].key, lk) == 0) {
      snprintf(h->items[i].val, sizeof(h->items[i].val), "%s", val);
      return 0;
    }
  }
  if (h->cnt >= NOXEN_HDR_MAX)
    return -1;
  snprintf(h->items[h->cnt].key, sizeof(h->items[h->cnt].key), "%s", lk);
  snprintf(h->items[h->cnt].val, sizeof(h->items[h->cnt].val), "%s", val);
  h->cnt++;
  return 0;
}

/*
 * 拆分报文为头部表与正文块（正文 malloc，调用方 free）。
 * 折行续行规则：空白开头且已有当前键时以空格拼接；跳过 mbox "From " 首行。
 * @param payload 已做 CRLF→LF 归一化的原始报文
 * @param offset  起始行号（外部 "#participant"+part 喂入时置 1）
 */
static void noxen_split_eml(const char *payload, int offset, noxen_hdrs_t *hdrs,
                            char **body_out) {
  memset(hdrs, 0, sizeof(*hdrs));
  *body_out = NULL;

  const char *line = payload;
  for (int i = 0; i < offset && *line; i++) {
    const char *nl = strchr(line, '\n');
    line = nl ? nl + 1 : line + strlen(line);
  }
  /* 跳过 mbox "From " 首行 */
  if (strncmp(line, "From ", 5) == 0) {
    const char *nl = strchr(line, '\n');
    line = nl ? nl + 1 : line + strlen(line);
  }

  char cur_key[96];
  cur_key[0] = '\0';
  for (;;) {
    if (!*line)
      break;
    const char *nl = strchr(line, '\n');
    size_t llen = nl ? (size_t)(nl - line) : strlen(line);
    if (llen == 0) {
      /* 空行：头结束，正文从下一行开始 */
      line = nl ? nl + 1 : line;
      break;
    }
    /* 折行续行：空白开头且已有当前头键时拼接到上一条 */
    if ((line[0] == ' ' || line[0] == '\t') && cur_key[0]) {
      const char *t = line;
      while ((size_t)(t - line) < llen && (*t == ' ' || *t == '\t' || *t == '\r'))
        t++;
      size_t tl = llen - (size_t)(t - line);
      while (tl > 0 &&
             (t[tl - 1] == ' ' || t[tl - 1] == '\t' || t[tl - 1] == '\r'))
        tl--;
      for (int i = 0; i < hdrs->cnt; i++) {
        if (strcmp(hdrs->items[i].key, cur_key) == 0) {
          size_t cur = strlen(hdrs->items[i].val);
          if (cur + 1 < sizeof(hdrs->items[i].val)) {
            hdrs->items[i].val[cur++] = ' ';
            size_t room = sizeof(hdrs->items[i].val) - cur - 1;
            size_t addl = tl < room ? tl : (room > 0 ? room - 0 : 0);
            if (addl > 0 && addl <= room) {
              memcpy(hdrs->items[i].val + cur, t, addl);
              cur += addl;
            }
            hdrs->items[i].val[cur] = '\0';
          }
          break;
        }
      }
    } else {
      const char *colon = memchr(line, ':', llen);
      if (colon && colon > line && (size_t)(colon - line) < 95) {
        char key[96];
        size_t kl = (size_t)(colon - line);
        memcpy(key, line, kl);
        key[kl] = '\0';
        /* key trim */
        char *kp = key;
        while (*kp == ' ' || *kp == '\t')
          kp++;
        size_t kn = strlen(kp);
        while (kn > 0 && (kp[kn - 1] == ' ' || kp[kn - 1] == '\t'))
          kp[--kn] = '\0';
        noxen_lower(kp);
        snprintf(cur_key, sizeof(cur_key), "%s", kp);

        const char *t = colon + 1;
        while ((size_t)(t - line) < llen && (*t == ' ' || *t == '\t' || *t == '\r'))
          t++;
        size_t tl = llen - (size_t)(t - line);
        while (tl > 0 &&
               (t[tl - 1] == ' ' || t[tl - 1] == '\t' || t[tl - 1] == '\r'))
          tl--;
        char val[4096];
        size_t vlen = tl < sizeof(val) - 1 ? tl : sizeof(val) - 1;
        memcpy(val, t, vlen);
        val[vlen] = '\0';
        noxen_hdr_set(hdrs, kp, val);
      } else {
        /* 无冒号行：清空当前键（防误拼续行） */
        cur_key[0] = '\0';
      }
    }
    line = nl ? nl + 1 : line + llen;
  }
  *body_out = tm_strdup(line);
}

/* 从原始 Content-Type 提取 boundary（引号可选，区分大小写） */
static char *noxen_boundary(const char *ct_raw) {
  if (!ct_raw)
    return NULL;
  const char *b = noxen_istrstr(ct_raw, "boundary");
  if (!b)
    return NULL;
  b += 8;
  while (*b == ' ' || *b == '=' || *b == '\t')
    b++;
  int quoted = 0;
  if (*b == '"') {
    quoted = 1;
    b++;
  }
  char out[512];
  size_t n = 0;
  while (b[n] && n < sizeof(out) - 1) {
    char c = b[n];
    if (quoted) {
      if (c == '"')
        break;
    } else if (c == ';' || c == ' ' || c == '\t' || c == '\n' || c == '\r') {
      break;
    }
    out[n++] = c;
  }
  out[n] = '\0';
  return tm_strdup(out);
}

/* base64 解码（跳过空白；失败返回 NULL），语义与 Go 标准库一致
 * （含尾 padding）；自实现四字节表驱动，无外部依赖。 */
static char *noxen_b64_decode(const char *src) {
  /* 'A'-'Z'=0-25、'a'-'z'=26-51、'0'-'9'=52-61、'+'=62、'/'=63，其余 -1 */
  static const signed char tbl[128] = {
      62, -1, -1, -1, 63, /* + / */
      -1, -1, -1, -1, -1, -1, -1, -1, -1, -1,
      -1, /* 残留占位：'0'-'9' 与字母段由下方循环类比映射 */
  };
  static int tbl_init = 0;
  if (!tbl_init) {
    for (int i = 0; i < 128; i++)
      ((signed char *)tbl)[i] = -1;
    for (int i = 'A'; i <= 'Z'; i++)
      ((signed char *)tbl)[i] = (signed char)(i - 'A');
    for (int i = 'a'; i <= 'z'; i++)
      ((signed char *)tbl)[i] = (signed char)(i - 'a' + 26);
    for (int i = '0'; i <= '9'; i++)
      ((signed char *)tbl)[i] = (signed char)(i - '0' + 52);
    ((signed char *)tbl)[(unsigned char)'+'] = 62;
    ((signed char *)tbl)[(unsigned char)'/'] = 63;
    tbl_init = 1;
  }

  size_t n = strlen(src);
  /* 去空白 */
  size_t clean = 0;
  for (size_t i = 0; i < n; i++) {
    char c = src[i];
    if (c != ' ' && c != '\n' && c != '\r' && c != '\t')
      clean++;
  }
  if (clean % 4 != 0)
    return NULL;
  char *joined = (char *)malloc(clean + 1);
  if (!joined)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    char c = src[i];
    if (c != ' ' && c != '\n' && c != '\r' && c != '\t')
      joined[o++] = c;
  }
  joined[o] = '\0';

  /* 统计尾部 '=' 数量（Go 标准库容忍尾部 padding 形态） */
  size_t pads = 0;
  for (size_t i = clean; i > 0 && joined[i - 1] == '='; i--)
    pads++;
  if (pads > 2) {
    free(joined);
    return NULL;
  }

  size_t out_cap = clean / 4 * 3 + 1;
  char *out = (char *)malloc(out_cap);
  if (!out) {
    free(joined);
    return NULL;
  }
  size_t wpos = 0;
  unsigned int acc = 0;
  int bits = 0;
  for (size_t i = 0; i < clean; i++) {
    char c = joined[i];
    if (c == '=')
      break;
    int v = (c >= 0 && c < 128) ? tbl[(unsigned char)c] : -1;
    if (v < 0) {
      free(joined);
      free(out);
      return NULL;
    }
    acc = (acc << 6) | (unsigned int)v;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[wpos++] = (char)((acc >> bits) & 0xFF);
    }
  }
  out[wpos] = '\0';
  free(joined);
  return out;
}

/* quoted-printable 解码（=XX hex + '=' 行尾软换行）；自实现简单版 */
static char *noxen_qp_decode(const char *src) {
  size_t n = strlen(src);
  char *out = (char *)malloc(n + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    if (src[i] == '=') {
      /* 软换行：'=' 后随换行（可选 \r） */
      if (i + 1 < n && src[i + 1] == '\n') {
        i++;
        continue;
      }
      if (i + 2 < n && src[i + 1] == '\r' && src[i + 2] == '\n') {
        i += 2;
        continue;
      }
      if (i + 2 < n && isxdigit((unsigned char)src[i + 1]) &&
          isxdigit((unsigned char)src[i + 2])) {
        int hv = isdigit((unsigned char)src[i + 1])
                     ? src[i + 1] - '0'
                     : (tolower((unsigned char)src[i + 1]) - 'a' + 10);
        int lv = isdigit((unsigned char)src[i + 2])
                     ? src[i + 2] - '0'
                     : (tolower((unsigned char)src[i + 2]) - 'a' + 10);
        out[o++] = (char)((hv << 4) | lv);
        i += 2;
        continue;
      }
      out[o++] = '=';
      continue;
    }
    out[o++] = src[i];
  }
  out[o] = '\0';
  char *t = noxen_trim(out);
  if (t != out)
    memmove(out, t, strlen(t) + 1);
  return out;
}

/* 按 Content-Transfer-Encoding 解码 part 内容（返回 malloc 副本，
 * self-owned；GBK 等非 UTF-8 字节原样输出不转码） */
static char *noxen_decode_part(const char *data, const char *cte) {
  if (!data)
    return tm_strdup("");
  if (cte && strcasecmp(cte, "base64") == 0) {
    char *d = noxen_b64_decode(data);
    if (d)
      return d;
  } else if (cte && strcasecmp(cte, "quoted-printable") == 0) {
    return noxen_qp_decode(data);
  }
  /* 7bit/8bit/binary：原样 trim */
  return noxen_trim_dup(data);
}

/* 邮件正文双槽：首个非空命中即接管 */
typedef struct {
  char *text;
  char *html;
} noxen_slot_t;

/* 按 boundary 切分 multipart 正文（每段 malloc，调用方逐段 free），
 * 与 Go SplitMultipart 一致：按 "--boundary" 切分、段首 "\n" 去除、
 * 段尾 "--\n"/"--" 去除、跳过空白段。 */
static char **noxen_split_multipart(const char *body, const char *boundary,
                                    int *out_n) {
  *out_n = 0;
  if (!body || !boundary || !boundary[0])
    return NULL;
  size_t blen = strlen(boundary);
  size_t dlen = blen + 2;
  char *delim = (char *)malloc(dlen + 1);
  if (!delim)
    return NULL;
  delim[0] = '-';
  delim[1] = '-';
  memcpy(delim + 2, boundary, blen);
  delim[dlen] = '\0';

  char **kept = NULL;
  int kn = 0, kcap = 0;
  const char *start = body;
  for (;;) {
    const char *hit = strstr(start, delim);
    size_t seglen = hit ? (size_t)(hit - start) : strlen(start);
    char *seg = (char *)malloc(seglen + 1);
    if (seg) {
      memcpy(seg, start, seglen);
      seg[seglen] = '\0';

      /* 段规整：TrimPrefix "\n"、TrimSuffix "--\n"/"--" */
      char *s = seg;
      if (s[0] == '\n')
        s++;
      size_t sl = strlen(s);
      if (sl >= 3 && strncmp(s + sl - 3, "--\n", 3) == 0) {
        s[sl - 3] = '\0';
      } else if (sl >= 2 && strncmp(s + sl - 2, "--", 2) == 0) {
        s[sl - 2] = '\0';
      }
      char *t = noxen_trim(s);
      if (s != seg || t != s) {
        if (t != seg)
          memmove(seg, t, strlen(t) + 1);
        else
          memmove(seg, t, strlen(t) + 1);
      }
      if (seg[0] != '\0') {
        if (kn == kcap) {
          kcap = kcap ? kcap * 2 : 8;
          char **np = (char **)realloc(kept, sizeof(char *) * (size_t)kcap);
          if (!np) {
            free(seg);
            free(delim);
            for (int i = 0; i < kn; i++)
              free(kept[i]);
            free(kept);
            return NULL;
          }
          kept = np;
        }
        kept[kn++] = seg;
      } else {
        free(seg);
      }
    }
    if (!hit)
      break;
    start = hit + dlen;
  }
  free(delim);
  *out_n = kn;
  return kept;
}

/* 递归解析单个 MIME 实体（上游 parseEntity 同构）。
 * text 槽与 html 槽各自取第一个非空命中；无 HTML 命中时兜底抓
 * <html>…</html> 片段。 */
static void noxen_parse_entity(const noxen_hdrs_t *hdrs, const char *body,
                               noxen_slot_t *slot, int depth);
static void noxen_guess_html(const char *body, noxen_slot_t *slot);

static void noxen_parse_entity(const noxen_hdrs_t *hdrs, const char *body,
                               noxen_slot_t *slot, int depth) {
  if (depth > 8)
    return;
  const char *ct_raw = noxen_hdr_get(hdrs, "content-type");
  const char *cte = noxen_hdr_get(hdrs, "content-transfer-encoding");
  char ct[256];
  ct[0] = '\0';
  if (ct_raw) {
    snprintf(ct, sizeof(ct), "%s", ct_raw);
    noxen_lower(ct);
  }

  /* 单体：非 multipart/ 前缀（无 Content-Type 时按纯文本处理） */
  if (strncmp(ct, "multipart/", 10) != 0) {
    char *decoded = noxen_decode_part(body, cte);
    if (strstr(ct, "text/html")) {
      if (!slot->html)
        slot->html = decoded;
      else
        free(decoded);
    } else {
      if (!slot->text)
        slot->text = decoded;
      else
        free(decoded);
    }
    return;
  }

  /* 复合：按 boundary 递归拆分 */
  char *boundary = noxen_boundary(ct_raw);
  if (boundary && boundary[0]) {
    int pn = 0;
    char **parts = noxen_split_multipart(body, boundary, &pn);
    for (int i = 0; i < pn && !(slot->text && slot->html); i++) {
      /* 外部以 "#participant"+part 形式喂入 split_eml 拆分 part 头/体 */
      size_t pl = strlen(parts[i]);
      char *wrapped = (char *)malloc(pl + 13);
      if (!wrapped) {
        free(parts[i]);
        continue;
      }
      memcpy(wrapped, "#participant\n", 12);
      memcpy(wrapped + 12, parts[i], pl + 1);
      free(parts[i]);
      parts[i] = NULL;

      noxen_hdrs_t ph;
      char *pb = NULL;
      noxen_split_eml(wrapped, 1, &ph, &pb);
      free(wrapped);
      if (!pb)
        pb = tm_strdup("");

      const char *pct_raw = noxen_hdr_get(&ph, "content-type");
      char pct[256];
      pct[0] = '\0';
      if (pct_raw) {
        snprintf(pct, sizeof(pct), "%s", pct_raw);
        noxen_lower(pct);
      }

      if (strncmp(pct, "multipart/", 10) == 0) {
        noxen_parse_entity(&ph, pb, slot, depth + 1);
      } else if (strncmp(pct, "message/rfc822", 14) == 0) {
        /* 转发的原始邮件整体作为 part：整封递归 */
        noxen_hdrs_t nh;
        char *nb = NULL;
        noxen_split_eml(pb, 0, &nh, &nb);
        if (nb) {
          noxen_parse_entity(&nh, nb, slot, depth + 1);
          free(nb);
        }
      } else if (strstr(pct, "rfc822-headers")) {
        /* 纯头部 part 跳过，正文在后续 part 中抓取 */
      } else {
        noxen_parse_entity(&ph, pb, slot, depth + 1);
      }
      free(pb);
    }
    /* 释放剩余 part 段（已在循环内释放的置 NULL 防双释放） */
    for (int i = 0; i < pn; i++)
      free(parts[i]);
    free(parts);
  }
  free(boundary);

  /* 无 HTML 命中时从整体原文兜底抓取 HTML 片段（guessHtmlFromRaw 同构） */
  if (!slot->html)
    noxen_guess_html(body, slot);
}

/* 从整体原文抓 <html>…</html>（大小写不敏感，无则试 <!doctype html） */
static void noxen_guess_html(const char *body, noxen_slot_t *slot) {
  if (!body || !body[0])
    return;
  const char *hs = noxen_istrstr(body, "<html");
  if (!hs)
    hs = noxen_istrstr(body, "<!doctype html");
  if (!hs)
    return;
  const char *he = noxen_irfind(body, "</html>");
  if (!he || he < hs)
    return;
  size_t len = (size_t)(he - hs) + 7;
  char *html = (char *)malloc(len + 1);
  if (!html)
    return;
  memcpy(html, hs, len);
  html[len] = '\0';
  char *t = noxen_trim(html);
  if (t != html)
    memmove(html, t, strlen(t) + 1);
  if (html[0]) {
    if (!slot->html)
      slot->html = html;
    else
      free(html);
  } else {
    free(html);
  }
}

/* 解析原始 EML → (纯文本正文, HTML 正文)；二者均 malloc，调用方 free，
 * 可为 NULL。 */
static void noxen_parse_eml(const char *raw, char **text_out, char **html_out) {
  *text_out = NULL;
  *html_out = NULL;
  if (!raw || !raw[0])
    return;

  /* CRLF→LF 归一化 */
  size_t n = strlen(raw);
  char *payload = (char *)malloc(n + 1);
  if (!payload)
    return;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    char c = raw[i];
    if (c == '\r') {
      if (i + 1 < n && raw[i + 1] == '\n')
        continue; /* \r\n 合并为单个 \n */
      payload[o++] = '\n';
    } else {
      payload[o++] = c;
    }
  }
  payload[o] = '\0';

  noxen_hdrs_t top;
  char *top_body = NULL;
  noxen_split_eml(payload, 0, &top, &top_body);
  free(payload);
  if (!top_body)
    return;

  noxen_slot_t slot;
  slot.text = NULL;
  slot.html = NULL;
  noxen_parse_entity(&top, top_body, &slot, 0);
  free(top_body);
  *text_out = slot.text;
  *html_out = slot.html;
}

/* ========== 读信 ========== */

/* 合成占位正文：verification_code 置顶（"验证码: xxx"），preview 附后；
 * 二者均空时输出空串（哨兵提取以 preview 实际覆盖范围为如实上限）。 */
static void noxen_placeholder(const cJSON *m, char *out, size_t cap) {
  out[0] = '\0';
  const cJSON *v = cJSON_GetObjectItemCaseSensitive(m, "verification_code");
  if (v && (cJSON_IsString(v) || cJSON_IsNumber(v))) {
    const char *code =
        cJSON_IsString(v) ? (v->valuestring ? v->valuestring : "") : NULL;
    char num[32];
    if (!cJSON_IsString(v)) {
      snprintf(num, sizeof(num), "%lld", (long long)v->valuedouble);
      code = num;
    }
    char *t = noxen_trim_dup(code);
    if (t && t[0])
      snprintf(out, cap, "%s%s", "验证码: ", t);
    free(t);
  }
  size_t used = strlen(out);
  {
    const cJSON *pv = cJSON_GetObjectItemCaseSensitive(m, "preview");
    if (cJSON_IsString(pv) && pv->valuestring) {
      char *t = noxen_trim_dup(pv->valuestring);
      if (t && t[0]) {
        if (used > 0 && used + 2 < cap) {
          out[used++] = '\n';
          out[used++] = '\n';
          out[used] = '\0';
        }
        snprintf(out + used, cap - used, "%s", t);
      }
      free(t);
    }
  }
}

/* 单封详情字段 */
typedef struct {
  char content[8192];
  char html_content[8192];
  char to_addrs[1024];
  char download[512];
} noxen_detail_t;

/* 拉取单封详情（GET /api/email/<id>，带 Cookie）；失败返回 -1 */
static int noxen_fetch_detail(const char *cookie, const char *id,
                              noxen_detail_t *d) {
  memset(d, 0, sizeof(*d));
  size_t need = strlen(id) + 128;
  char *url = (char *)malloc(need);
  if (!url)
    return -1;
  snprintf(url, need, "%s/api/email/%s", NOXEN_BASE, id);
  char **h = noxen_mk_headers(cookie);
  if (!h) {
    free(url);
    return -1;
  }
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  free(url);
  noxen_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    tm_http_response_free(resp);
    return -1;
  }
  cJSON *root = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!root)
    return -1;

  const char *content =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "content"), "");
  const char *html_content =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "html_content"), "");
  const char *to_addrs =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "to_addrs"), "");
  const char *download =
      TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(root, "download"), "");
  snprintf(d->content, sizeof(d->content), "%s", content);
  snprintf(d->html_content, sizeof(d->html_content), "%s", html_content);
  snprintf(d->to_addrs, sizeof(d->to_addrs), "%s", to_addrs);
  snprintf(d->download, sizeof(d->download), "%s", download);
  cJSON_Delete(root);
  return 0;
}

/* 拉取原始 EML（download 相对路径 → 基址拼接；Accept: message/rfc822）；
 * 失败返回 NULL */
static char *noxen_fetch_eml(const char *cookie, const char *dl_path) {
  if (!dl_path || !dl_path[0])
    return NULL;
  const char *u = dl_path;
  char full[640];
  if (strncmp(u, "http://", 7) != 0 && strncmp(u, "https://", 8) != 0) {
    snprintf(full, sizeof(full), "%s%s", NOXEN_BASE, u);
    u = full;
  }
  size_t len = strlen(cookie) + 16;
  char *cookie_hdr = (char *)malloc(len);
  if (!cookie_hdr)
    return NULL;
  snprintf(cookie_hdr, len, "Cookie: %s", cookie);
  const char *headers[] = {
      "Accept: message/rfc822, */*", noxen_ua, cookie_hdr, NULL};
  tm_http_response_t *resp = tm_http_request(TM_HTTP_GET, u, headers, NULL, 15);
  free(cookie_hdr);
  if (!resp || resp->status < 200 || resp->status >= 300 || !resp->body) {
    tm_http_response_free(resp);
    return NULL;
  }
  char *eml = tm_strdup(resp->body);
  tm_http_response_free(resp);
  return eml;
}

/**
 * 读取 tempmail.noxen.de5.net 收件箱
 * 列表只含预览：正文优先走详情 download 端点原始 EML 本地解析
 * （text/html 双槽），否则用 verification_code + preview 合成占位。
 */
tm_email_t *tm_provider_noxen_de5_net_get_emails(const char *email,
                                                 const char *token,
                                                 int *count) {
  *count = 0;
  if (!email || !email[0])
    return NULL;
  if (!token || strncmp(token, NOXEN_TOKEN_PREFIX, strlen(NOXEN_TOKEN_PREFIX)) !=
                   0) {
    TM_LOG_ERR("noxen-de5-net: token 格式错误");
    return NULL;
  }

  /* 解前缀/解码（url.PathEscape 语义）/去后缀拿 cookie */
  char *enc = noxen_query_unescape(token + strlen(NOXEN_TOKEN_PREFIX));
  if (!enc)
    return NULL;
  char suffix[128];
  snprintf(suffix, sizeof(suffix), "|base=%s", NOXEN_BASE);
  size_t sl = strlen(enc);
  size_t ss = strlen(suffix);
  if (sl >= ss && strcmp(enc + sl - ss, suffix) == 0)
    enc[sl - ss] = '\0';
  char *cookie = noxen_query_unescape(enc);
  free(enc);
  if (!cookie || !cookie[0]) {
    free(cookie);
    TM_LOG_ERR("noxen-de5-net: token 解码失败");
    return NULL;
  }

  /* 会话校验：失用时重新登录换新会话 */
  if (!noxen_session_valid(cookie)) {
    int rate_limited = 0;
    char *fresh = noxen_login(&rate_limited);
    if (fresh) {
      free(cookie);
      cookie = fresh;
    }
  }

  /* 列表：GET /api/emails?mailbox=<urlenc>&limit=20 */
  char *enc_email = noxen_query_escape(email);
  size_t need = strlen(NOXEN_BASE) + 64 + (enc_email ? strlen(enc_email) : 0);
  char *url = (char *)malloc(need);
  if (!url) {
    free(enc_email);
    free(cookie);
    return NULL;
  }
  snprintf(url, need, "%s/api/emails?mailbox=%s&limit=20", NOXEN_BASE,
           enc_email ? enc_email : "");
  free(enc_email);

  char **h = noxen_mk_headers(cookie);
  if (!h) {
    free(url);
    free(cookie);
    return NULL;
  }
  tm_http_response_t *resp =
      tm_http_request(TM_HTTP_GET, url, (const char **)h, NULL, 15);
  free(url);
  noxen_free_headers(h);
  if (!resp || resp->status < 200 || resp->status >= 300) {
    if (resp && resp->status == 401)
      TM_LOG_ERR("noxen-de5-net: 读信 401（会话失效或非本会话邮箱）");
    else
      TM_LOG_ERR("noxen-de5-net: 读信失败 http %ld",
                 resp ? resp->status : -1);
    tm_http_response_free(resp);
    free(cookie);
    return NULL;
  }
  cJSON *list = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!cJSON_IsArray(list)) {
    cJSON_Delete(list);
    free(cookie);
    return NULL;
  }

  int n = cJSON_GetArraySize(list);
  if (n == 0) {
    cJSON_Delete(list);
    free(cookie);
    return NULL;
  }
  tm_email_t *emails = tm_emails_new(n);
  if (!emails) {
    *count = -1;
    cJSON_Delete(list);
    free(cookie);
    return NULL;
  }

  int valid = 0;
  for (int i = 0; i < n; i++) {
    cJSON *m = cJSON_GetArrayItem(list, i);
    if (!cJSON_IsObject(m))
      continue;

    char idbuf[128];
    {
      const cJSON *v = cJSON_GetObjectItemCaseSensitive(m, "id");
      if (cJSON_IsString(v))
        snprintf(idbuf, sizeof(idbuf), "%s",
                 v->valuestring ? v->valuestring : "");
      else if (cJSON_IsNumber(v))
        snprintf(idbuf, sizeof(idbuf), "%lld", (long long)v->valuedouble);
      else
        idbuf[0] = '\0';
    }

    cJSON *raw = cJSON_CreateObject();
    if (!raw)
      continue;

    /* flat：from=sender、to=email、date=received_at、text=preview、
     * isRead=is_read（normalize 候选已覆盖这些键名） */
    {
      const char *sender =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "sender"), "");
      if (sender[0])
        cJSON_AddStringToObject(raw, "from", sender);
    }
    cJSON_AddStringToObject(raw, "to", email);
    {
      const char *received =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "received_at"), "");
      if (received[0])
        cJSON_AddStringToObject(raw, "date", received);
    }
    {
      const char *preview =
          TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(m, "preview"), "");
      if (preview[0])
        cJSON_AddStringToObject(raw, "text", preview);
    }
    {
      const cJSON *ir = cJSON_GetObjectItemCaseSensitive(m, "is_read");
      if (ir)
        cJSON_AddItemReferenceToObject(raw, "is_read", ir);
    }

    int full = 0;
    if (idbuf[0] && strcmp(idbuf, "0") != 0) {
      noxen_detail_t det;
      if (noxen_fetch_detail(cookie, idbuf, &det) == 0) {
        if (det.content[0])
          cJSON_AddStringToObject(raw, "content", det.content);
        if (det.html_content[0])
          cJSON_AddStringToObject(raw, "html_content", det.html_content);
        if (det.to_addrs[0])
          cJSON_AddStringToObject(raw, "to_addrs", det.to_addrs);
        /* 全文：download 端点原始 EML → 本地拆分 text/html */
        if (det.download[0]) {
          char *eml = noxen_fetch_eml(cookie, det.download);
          if (eml && eml[0]) {
            char *text = NULL;
            char *html = NULL;
            noxen_parse_eml(eml, &text, &html);
            if (text || html) {
              cJSON_ReplaceItemInObjectCaseSensitive(
                  raw, "text", cJSON_CreateString(text ? text : ""));
              cJSON_ReplaceItemInObjectCaseSensitive(
                  raw, "html_content", cJSON_CreateString(html ? html : ""));
              full = 1;
            }
            free(text);
            free(html);
          }
          free(eml);
        }
      }
    }
    if (!full) {
      char ph[4600];
      noxen_placeholder(m, ph, sizeof(ph));
      cJSON_ReplaceItemInObjectCaseSensitive(raw, "text",
                                             cJSON_CreateString(ph));
    }

    emails[valid] = tm_normalize_email(raw, email);
    cJSON_Delete(raw);
    valid++;
  }

  cJSON_Delete(list);
  free(cookie);
  if (valid == 0) {
    free(emails);
    *count = 0;
    return NULL;
  }
  *count = valid;
  return emails;
}