/**
 * temp-mail-gg 渠道 — https://temp-mail.gg
 *
 * Laravel Livewire v3 会话（共享 http.c 无 Cookie 罐，本文件内维护静态
 * Cookie 桶；Laravel 每次 livewire/update 轮换会话 Cookie，必须以响应
 * Set-Cookie 同名覆写后回填下一次请求，否则 419）：
 *   - GET / 首页取 data-csrf 与初始 wire:snapshot（属性值为 HTML 实体
 *     转义态，需反转义），XSRF-TOKEN / tempmail_session Cookie 同步落桶。
 *   - POST /livewire/update 是唯一边界：JSON 顶层 _token（=data-csrf），
 *     components[0]={snapshot, updates:{}, calls:[]}；generateEmail 建箱
 *     并回传新 snapshot（data.email 输出邮箱），calls 为空的 update 即
 *     平台 20 秒轮询刷信形态。
 *   - 列表解析轮询响应 effects.html：条目容器
 *     <div wire:click="selectEmail(<数字id>)">，其内 h3=发件人、
 *     p(text-zinc-300)=主题、p(line-clamp-2)=预览、span(text-xs)=相对时间。
 *   - 详情：calls=[{method:"selectEmail",params:[<数字id>]}]，响应
 *     effects.html 模态框含 From / text-xl 主题 / x-show text 纯文本。
 *
 * Token：前缀 + JSON {email, csrf, snapshot}；读信复用快照并断言响应
 * data.email 仍指向本邮箱（防会话被并行 Generate 覆盖后串箱）。
 * 免登录配额每时段 5 个邮箱，邮箱 30 分钟无活动过期。
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

#define GG_BASE "https://temp-mail.gg"
#define GG_UA                                                                 \
  "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) " \
  "Chrome/154.0.0.0 Safari/537.36"
#define GG_TOKEN_PREFIX "temp-mail-gg|"
#define GG_COOKIE_CAP 8192
#define GG_SNAP_STATIC_CAP 65536 /* 快照静态缓冲（超出转堆） */
#define GG_MAX_ROWS 128          /* 列表条目上限（平台一屏 5 封，留余量） */
#define GG_PREFIX_LEN (sizeof(GG_TOKEN_PREFIX) - 1)

/* 首页 GET 浏览器头 */
static const char *gg_html_headers[] = {
    "User-Agent: " GG_UA,
    "Accept: text/html,application/xhtml+xml,application/xml;q=0.9,"
    "image/avif,image/webp,*/*;q=0.8",
    "Accept-Language: en-US,en;q=0.9",
    NULL};

/* livewire/update 同站 fetch 全套头 */
static const char *gg_update_headers[] = {
    "User-Agent: " GG_UA,
    "Accept: text/html, application/xhtml+xml",
    "Accept-Language: en-US,en;q=0.9",
    "Content-Type: application/json",
    "X-Livewire:",
    "X-Requested-With: XMLHttpRequest",
    "Origin: " GG_BASE,
    "Referer: " GG_BASE "/",
    NULL};

/* ========== 字符串工具 ========== */

/* 去首尾空白（原串原地修改，返回首地址） */
static char *gg_trim(char *s) {
  char *p = s;
  while (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')
    p++;
  size_t n = strlen(p);
  while (n > 0 && (p[n - 1] == ' ' || p[n - 1] == '\t' || p[n - 1] == '\n' ||
                   p[n - 1] == '\r'))
    p[--n] = '\0';
  return p;
}

/* 限长大小写不敏感子串查找（在 hay[0, limit) 内，needle 须完全落入） */
static const char *gg_cfind(const char *hay, const char *needle, size_t limit) {
  size_t nlen = strlen(needle);
  if (nlen == 0)
    return hay;
  if (limit < nlen)
    return NULL;
  size_t end = limit - nlen;
  for (size_t x = 0; x <= end; x++) {
    if (strncasecmp(hay + x, needle, nlen) == 0)
      return hay + x;
  }
  return NULL;
}

/* HTML 实体反转义（命名实体 + &#NNN; / &#xHH; 数字实体） */
static char *gg_html_unescape(const char *src) {
  if (!src)
    return NULL;
  size_t n = strlen(src);
  char *out = (char *)malloc(n + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    if (src[i] != '&') {
      out[o++] = src[i];
      continue;
    }
    const char *ent = NULL;
    char rep = 0;
    if (strncmp(src + i, "&amp;", 5) == 0) {
      ent = "&amp;", rep = '&';
    } else if (strncmp(src + i, "&lt;", 4) == 0) {
      ent = "&lt;", rep = '<';
    } else if (strncmp(src + i, "&gt;", 4) == 0) {
      ent = "&gt;", rep = '>';
    } else if (strncmp(src + i, "&quot;", 6) == 0) {
      ent = "&quot;", rep = '"';
    } else if (strncmp(src + i, "&#39;", 5) == 0) {
      ent = "&#39;", rep = '\'';
    } else if (strncmp(src + i, "&apos;", 6) == 0) {
      ent = "&apos;", rep = '\'';
    } else if (strncmp(src + i, "&nbsp;", 6) == 0) {
      ent = "&nbsp;", rep = ' ';
    } else if (strncmp(src + i, "&#", 2) == 0) {
      long code = 0;
      const char *q = src + i + 2;
      int ok = 0;
      if (*q == 'x' || *q == 'X') {
        q++;
        while (isxdigit((unsigned char)*q)) {
          int d = isdigit((unsigned char)*q)
                      ? *q - '0'
                      : (tolower(*q) - 'a' + 10);
          if (code < 0x110000)
            code = code * 16 + d;
          q++;
          ok = 1;
        }
      } else {
        while (isdigit((unsigned char)*q)) {
          if (code < 0x110000)
            code = code * 10 + (*q - '0');
          q++;
          ok = 1;
        }
      }
      if (ok && *q == ';') {
        if (code >= 0x20 && code <= 255)
          out[o++] = (char)code;
        i = (size_t)(q - src);
        continue;
      }
    }
    if (ent) {
      out[o++] = rep;
      i += strlen(ent) - 1;
      continue;
    }
    out[o++] = src[i];
  }
  out[o] = '\0';
  return out;
}

/* HTML 特殊字符转义（pre 包装正文用） */
static char *gg_html_escape(const char *src) {
  if (!src)
    return NULL;
  size_t n = strlen(src);
  char *out = (char *)malloc(n * 6 + 1);
  if (!out)
    return NULL;
  size_t o = 0;
  for (size_t i = 0; i < n; i++) {
    switch (src[i]) {
    case '&':
      memcpy(out + o, "&amp;", 5);
      o += 5;
      break;
    case '<':
      memcpy(out + o, "&lt;", 4);
      o += 4;
      break;
    case '>':
      memcpy(out + o, "&gt;", 4);
      o += 4;
      break;
    case '"':
      memcpy(out + o, "&quot;", 6);
      o += 6;
      break;
    case '\'':
      memcpy(out + o, "&#39;", 5);
      o += 5;
      break;
    default:
      out[o++] = src[i];
    }
  }
  out[o] = '\0';
  return out;
}

/* 判断 src 是否以 "<tag" 起始（"<tag" / "<tag>" / "<tag "） */
static int gg_is_open_at(const char *src, const char *tag) {
  size_t tlen = strlen(tag);
  if (*src != '<' || strncasecmp(src + 1, tag, tlen) != 0)
    return 0;
  char c = src[1 + tlen];
  return c == '>' || c == '/' || c == ' ' || c == '\t' || c == '\n' ||
         c == '\r';
}

/* HTML 转纯文本（去 script/style/标签 + 反转义 + 合并空白） */
static char *gg_html_to_text(const char *src) {
  if (!src)
    return NULL;
  size_t n = strlen(src);
  char *buf = (char *)malloc(n + 1);
  if (!buf)
    return NULL;

  /* 第一遍：剔除 <script>...</script> 与 <style>...</style> 内容 */
  size_t o = 0;
  for (size_t i = 0; i < n;) {
    if (gg_is_open_at(src + i, "script") || gg_is_open_at(src + i, "style")) {
      const char *close = NULL;
      const char *e1 = strcasestr(src + i + 1, "</script>");
      const char *e2 = strcasestr(src + i + 1, "</style>");
      if (e1 && e2)
        close = (e1 < e2) ? e1 : e2;
      else
        close = e1 ? e1 : e2;
      if (close) {
        buf[o++] = ' ';
        i = (size_t)(close - src) + strlen(close);
        continue;
      }
    }
    buf[o++] = src[i++];
  }
  buf[o] = '\0';

  /* 第二遍：剥标签（'<' 至 '>' 置换为空白） */
  size_t o2 = 0;
  int in_tag = 0;
  for (size_t i = 0; buf[i]; i++) {
    if (buf[i] == '<') {
      in_tag = 1;
      continue;
    }
    if (in_tag && buf[i] == '>') {
      in_tag = 0;
      buf[o2++] = ' ';
      continue;
    }
    if (!in_tag)
      buf[o2++] = buf[i];
  }
  buf[o2] = '\0';

  char *unesc = gg_html_unescape(buf);
  free(buf);
  if (!unesc)
    return NULL;

  /* 第三遍：合并空白并收尾 trim */
  char *out = (char *)malloc(strlen(unesc) + 1);
  if (!out) {
    free(unesc);
    return NULL;
  }
  size_t uo = 0;
  int ws = 0;
  for (size_t i = 0; unesc[i]; i++) {
    char c = unesc[i];
    if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
      ws = 1;
      continue;
    }
    if (ws && uo > 0)
      out[uo++] = ' ';
    ws = 0;
    out[uo++] = c;
  }
  out[uo] = '\0';
  free(unesc);
  return out;
}

/* class 属性值是否包含指定词（按空白分词的精确 token 匹配） */
static int gg_class_has(const char *clsval, const char *word) {
  size_t wlen = strlen(word);
  const char *p = clsval;
  while (p && *p) {
    while (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r')
      p++;
    const char *end = p;
    while (*end && *end != ' ' && *end != '\t' && *end != '\n' && *end != '\r')
      end++;
    if ((size_t)(end - p) == wlen && strncmp(p, word, wlen) == 0)
      return 1;
    p = end;
  }
  return 0;
}

/* 在 tag 范围 [lt, gt) 提取 attr 的引号值（返回指针与长度，不拷贝） */
static int gg_attr_span(const char *lt, const char *gt, const char *attr,
                        const char **vout, size_t *vlen) {
  *vout = NULL;
  *vlen = 0;
  size_t alen = strlen(attr);
  const char *p = lt;
  while (p < gt) {
    const char *found = gg_cfind(p, attr, (size_t)(gt - p));
    if (!found)
      return -1;
    const char *q = found + alen;
    while (q < gt && (*q == ' ' || *q == '\t' || *q == '\n' || *q == '\r'))
      q++;
    if (q >= gt || *q != '=') {
      p = found + alen;
      continue;
    }
    q++;
    while (q < gt && (*q == ' ' || *q == '\t' || *q == '\n' || *q == '\r'))
      q++;
    if (q >= gt || (*q != '"' && *q != '\'')) {
      p = found + alen;
      continue;
    }
    char quote = *q++;
    const char *end = q;
    while (end < gt && *end != quote)
      end++;
    *vout = q;
    *vlen = (size_t)(end - q);
    return 0;
  }
  return -1;
}

/* 提取 tag 范围 [lt, gt) 内指定属性的引号值拷贝；未找到返回 -1 */
static int gg_tag_attr(const char *lt, const char *gt, const char *attr,
                       char *out, size_t cap) {
  out[0] = '\0';
  const char *v;
  size_t vl;
  if (gg_attr_span(lt, gt, attr, &v, &vl) != 0)
    return -1;
  if (vl >= cap)
    vl = cap - 1;
  memcpy(out, v, vl);
  out[vl] = '\0';
  return 0;
}

/* ========== 会话 Cookie 桶 ========== */

static char gg_cookie_buf[GG_COOKIE_CAP];
static char *gg_cookie_heap = NULL; /* 超过静态容量时的堆外扩（NULL=未用） */

/* 清空 Cookie 桶（Generate 建立全新会话前调用） */
static void gg_cookie_clear(void) {
  gg_cookie_buf[0] = '\0';
  if (gg_cookie_heap) {
    free(gg_cookie_heap);
    gg_cookie_heap = NULL;
  }
}

/* 覆写/追加一个 "name=value" 对（同名覆写并保持既有位置，异名追加尾部） */
static void gg_cookie_set(const char *name, const char *value) {
  if (!name || !name[0] || !value)
    return;
  const char *cur = gg_cookie_heap ? gg_cookie_heap : gg_cookie_buf;
  size_t nlen = strlen(name);

  /* 先扫描既有同名段 */
  const char *match = NULL;
  const char *match_end = NULL;
  const char *p = cur;
  while (p && *p) {
    const char *pend = strstr(p, "; ");
    size_t slen = pend ? (size_t)(pend - p) : strlen(p);
    if (slen > nlen && strncmp(p, name, nlen) == 0 && p[nlen] == '=') {
      match = p;
      match_end = pend ? pend : p + slen;
      break;
    }
    if (!pend)
      break;
    p = pend + 2;
  }

  size_t need = strlen(cur) + nlen + strlen(value) + 8;
  char *nb = (char *)malloc(need);
  if (!nb)
    return;
  if (!match) {
    snprintf(nb, need, "%s%s%s=%s", cur, cur[0] ? "; " : "", name, value);
  } else {
    /* 同名覆写：前段 + 新值 + 后段 */
    size_t pre_len = (size_t)(match - cur);
    const char *suf = match_end;
    if (suf[0] == ';')
      suf += 2; /* 越过 "; " 分隔 */
    snprintf(nb, need, "%.*s%s=%s%s%s", (int)pre_len, cur, name, value,
             suf[0] ? "; " : "", suf);
  }
  /* 结果放回：小串落静态桶，大串放堆 */
  if (strlen(nb) < sizeof(gg_cookie_buf) - 1) {
    snprintf(gg_cookie_buf, sizeof(gg_cookie_buf), "%s", nb);
    if (gg_cookie_heap) {
      free(gg_cookie_heap);
      gg_cookie_heap = NULL;
    }
    free(nb);
  } else {
    if (gg_cookie_heap)
      free(gg_cookie_heap);
    gg_cookie_heap = nb;
  }
}

/* 吸收响应 Set-Cookie 合并串（多组 "name=value" 以 "; " 分隔，逐组覆写） */
static void gg_cookie_absorb(const char *cookies) {
  if (!cookies || !cookies[0])
    return;
  const char *p = cookies;
  while (p && *p) {
    const char *pend = strstr(p, "; ");
    size_t slen = pend ? (size_t)(pend - p) : strlen(p);
    const char *eq = memchr(p, '=', slen);
    if (eq) {
      char name[256];
      size_t nlen = (size_t)(eq - p);
      if (nlen >= sizeof(name))
        nlen = sizeof(name) - 1;
      memcpy(name, p, nlen);
      name[nlen] = '\0';
      char value[GG_COOKIE_CAP];
      size_t vlen = slen - nlen - 1;
      if (vlen >= sizeof(value))
        vlen = sizeof(value) - 1;
      memcpy(value, eq + 1, vlen);
      value[vlen] = '\0';
      gg_cookie_set(name, value);
    }
    if (!pend)
      break;
    p = pend + 2;
  }
}

/* ========== 快照缓存（本文件内静态，线程模型与共享 http.c 一致） ========== */

static char gg_snap_static[GG_SNAP_STATIC_CAP];
static char *gg_snap_heap = NULL; /* 快照超过静态容量时转堆 */

/* 清空快照缓存（Generate 建立全新会话前调用） */
static void gg_snapshot_clear(void) {
  gg_snap_static[0] = '\0';
  if (gg_snap_heap) {
    free(gg_snap_heap);
    gg_snap_heap = NULL;
  }
}

/* 保存快照文本（内存不足保留旧值，读信继续用旧快照） */
static void gg_snapshot_save(const char *body) {
  size_t n = body ? strlen(body) : 0;
  if (n < sizeof(gg_snap_static)) {
    memcpy(gg_snap_static, body ? body : "", n + 1);
    if (gg_snap_heap) {
      free(gg_snap_heap);
      gg_snap_heap = NULL;
    }
  } else {
    char *nh = (char *)malloc(n + 1);
    if (!nh)
      return;
    memcpy(nh, body, n + 1);
    if (gg_snap_heap)
      free(gg_snap_heap);
    gg_snap_heap = nh;
  }
}

/* 读取当前快照文本副本（调用方释放） */
static char *gg_snapshot_load(void) {
  const char *cur = gg_snap_heap ? gg_snap_heap : gg_snap_static;
  if (!cur[0])
    return NULL;
  return tm_strdup(cur);
}

/* ========== 请求封装 ========== */

/* 带会话 Cookie 头的请求（桶空时不附加 Cookie 头） */
static tm_http_response_t *gg_do(tm_http_method_t method, const char *url,
                                 const char **base, const char *body) {
  const char *cur = gg_cookie_heap ? gg_cookie_heap : gg_cookie_buf;
  if (cur[0] == '\0')
    return tm_http_request(method, url, base, body, 15);

  size_t hdr_cap = strlen(cur) + 16;
  char *cookie_hdr = (char *)malloc(hdr_cap);
  if (!cookie_hdr)
    return tm_http_request(method, url, base, body, 15);
  snprintf(cookie_hdr, hdr_cap, "Cookie: %s", cur);

  size_t n = 0;
  while (base[n])
    n++;
  const char **headers = (const char **)malloc(sizeof(char *) * (n + 2));
  if (!headers) {
    free(cookie_hdr);
    return tm_http_request(method, url, base, body, 15);
  }
  for (size_t k = 0; k < n; k++)
    headers[k] = base[k];
  headers[n] = cookie_hdr;
  headers[n + 1] = NULL;

  tm_http_response_t *resp = tm_http_request(method, url, headers, body, 15);
  free(headers);
  free(cookie_hdr);
  return resp;
}

/* POST /livewire/update（Cookie 桶由调用方经 gg_update_ok 吸收） */
static tm_http_response_t *gg_update_do(const char *payload) {
  char url[96];
  snprintf(url, sizeof(url), "%s/livewire/update", GG_BASE);
  return gg_do(TM_HTTP_POST, url, gg_update_headers, payload);
}

/* 校验 update 响应状态并吸收会话 Cookie；失败记日志并释放，返回 -1 */
static int gg_update_ok(tm_http_response_t *resp) {
  if (!resp || resp->status < 200 || resp->status >= 300) {
    if (resp && resp->status == 419) {
      TM_LOG_ERR("temp-mail-gg: livewire 会话过期（419），请重新 Generate");
    } else {
      TM_LOG_ERR("temp-mail-gg: livewire/update http %ld",
                 resp ? resp->status : -1);
    }
    tm_http_response_free(resp);
    return -1;
  }
  gg_cookie_absorb(resp->cookies);
  return 0;
}

/*
 * 执行一次 livewire/update 并返回响应 JSON 的 components[0]
 * （jresp 归调用方释放；快照已回填文件内缓存）
 * @param snapshot 当前组件快照 JSON 串
 * @param csrf     data-csrf 令牌（顶层 _token）
 * @param method   组件方法（NULL/"": 纯轮询，即平台 20s 自动刷形态）
 * @param param    方法参数（method 非空时使用）
 */
static cJSON *gg_update_component(const char *snapshot, const char *csrf,
                                  const char *method, long param) {
  cJSON *payload = cJSON_CreateObject();
  cJSON *comps = cJSON_CreateArray();
  cJSON *comp = cJSON_CreateObject();
  cJSON *calls = cJSON_CreateArray();
  cJSON *upd = cJSON_CreateObject();
  if (!payload || !comps || !comp || !calls || !upd) {
    cJSON_Delete(payload);
    return NULL;
  }
  cJSON_AddStringToObject(payload, "_token", csrf ? csrf : "");
  if (method && method[0]) {
    cJSON *call = cJSON_CreateObject();
    cJSON *params = cJSON_CreateArray();
    if (call && params) {
      cJSON_AddStringToObject(call, "path", "");
      cJSON_AddStringToObject(call, "method", method);
      cJSON_AddItemToArray(params, cJSON_CreateNumber((double)param));
      cJSON_AddItemToObject(call, "params", params);
      cJSON_AddItemToArray(calls, call);
    }
  }
  cJSON_AddStringToObject(comp, "snapshot", snapshot ? snapshot : "");
  cJSON_AddItemToObject(comp, "updates", upd);
  cJSON_AddItemToObject(comp, "calls", calls);
  cJSON_AddItemToArray(comps, comp);
  cJSON_AddItemToObject(payload, "components", comps);

  char *ps = cJSON_PrintUnformatted(payload);
  cJSON_Delete(payload);
  if (!ps)
    return NULL;

  tm_http_response_t *resp = gg_update_do(ps);
  free(ps);
  if (gg_update_ok(resp) != 0)
    return NULL;

  cJSON *jresp = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!jresp) {
    TM_LOG_ERR("temp-mail-gg: 解析 update 响应失败");
    return NULL;
  }

  cJSON *jcomp = cJSON_GetArrayItem(
      cJSON_GetObjectItemCaseSensitive(jresp, "components"), 0);
  if (!cJSON_IsObject(jcomp)) {
    TM_LOG_ERR("temp-mail-gg: 轮询响应异常（components 缺失）");
    cJSON_Delete(jresp);
    return NULL;
  }

  /* 响应快照回填缓存（后续请求以最新快照为准） */
  cJSON *js = cJSON_GetObjectItemCaseSensitive(jcomp, "snapshot");
  if (cJSON_IsString(js) && js->valuestring && js->valuestring[0]) {
    char *s2 = cJSON_PrintUnformatted(js);
    if (s2) {
      gg_snapshot_save(s2);
      free(s2);
    }
  }
  return jresp;
}

/* ========== 首页属性提取 ========== */

/* 首页提取 data-csrf / wire:snapshot（快照为实体转义态，返回已反转义） */
static int gg_home_extract(const char *page, char *csrf, size_t csrf_cap,
                           char *snap, size_t snap_cap) {
  csrf[0] = '\0';
  snap[0] = '\0';
  const char *cur = page;
  while ((cur = strstr(cur, "<")) != NULL) {
    const char *gt = strchr(cur, '>');
    if (!gt)
      break;
    const char *v;
    size_t vl;
    if (!csrf[0] && gg_attr_span(cur, gt, "data-csrf", &v, &vl) == 0) {
      if (vl >= csrf_cap)
        vl = csrf_cap - 1;
      memcpy(csrf, v, vl);
      csrf[vl] = '\0';
    }
    if (!snap[0] && gg_attr_span(cur, gt, "wire:snapshot", &v, &vl) == 0) {
      char *tmp = (char *)malloc(vl + 1);
      if (tmp) {
        memcpy(tmp, v, vl);
        tmp[vl] = '\0';
        char *u = gg_html_unescape(tmp);
        free(tmp);
        if (u) {
          snprintf(snap, snap_cap, "%s", u);
          free(u);
        }
      }
    }
    if (csrf[0] && snap[0])
      return 0;
    cur = gt + 1;
  }
  return -1; /* 缺任一即无法建箱，由调用方输出渠道错误 */
}

/* ========== 相对时间 / UTC ========== */

/* 解析相对时间 "N second(s)/minute(s)/hour(s)/day(s) ago" 为 UTC RFC3339 */
static void gg_relative_to_utc(const char *when, char *out, size_t cap) {
  time_t val = time(NULL); /* 解析失败以当前 UTC 兜底 */
  char unitstr[24];
  long num = 0;
  int got = sscanf(when, "%ld %23s", &num, unitstr);
  if (got == 2) {
    long unit = 0;
    if (strncasecmp(unitstr, "second", 6) == 0)
      unit = 1;
    else if (strncasecmp(unitstr, "minute", 6) == 0)
      unit = 60;
    else if (strncasecmp(unitstr, "hour", 4) == 0)
      unit = 3600;
    else if (strncasecmp(unitstr, "day", 3) == 0)
      unit = 86400;
    /* 量级防御：0 <= N <= 366 天（慢钟/脏数据兜底为 now） */
    if (unit > 0 && num >= 0 && num <= 86400 * 366)
      val = time(NULL) - num * unit;
  }
  struct tm tmu;
#ifdef _WIN32
  if (gmtime_s(&tmu, &val) != 0)
    memset(&tmu, 0, sizeof(tmu));
#else
  gmtime_r(&val, &tmu);
#endif
  strftime(out, cap, "%Y-%m-%dT%H:%M:%SZ", &tmu);
}

/* ========== 列表 / 详情 HTML 解析 ========== */

/* 列表条目行 */
typedef struct {
  char id[64];
  char from[768];
  char subject[2048];
  char preview[8192];
  char when[64];
} gg_row_t;

/*
 * 在 [start, end) 内查首个 "<tag ...>" 且 class 含 cls 的元素，
 * 取其内容文本（剥标签合并空白）。cls 为 NULL 时跳过 class 过滤。
 */
static int gg_elem_text(const char *start, const char *end, const char *tag,
                        const char *cls, char *out, size_t cap) {
  out[0] = '\0';
  const char *cur = start;
  while (cur < end) {
    const char *lt = gg_cfind(cur, "<", (size_t)(end - cur));
    if (!lt)
      break;
    if (!gg_is_open_at(lt, tag)) {
      cur = lt + 1;
      continue;
    }
    const char *gt = strchr(lt, '>');
    if (!gt || gt >= end)
      break;
    if (cls) {
      char clsval[2048];
      if (gg_tag_attr(lt, gt, "class", clsval, sizeof(clsval)) != 0 ||
          !gg_class_has(clsval, cls)) {
        cur = gt + 1;
        continue;
      }
    }
    char closebuf[32];
    snprintf(closebuf, sizeof(closebuf), "</%s>", tag);
    const char *ce = gg_cfind(gt + 1, closebuf, (size_t)(end - (gt + 1)));
    const char *fend = ce ? ce : end;
    char *frag = (char *)malloc((size_t)(fend - (gt + 1)) + 1);
    char *text = NULL;
    if (frag) {
      memcpy(frag, gt + 1, (size_t)(fend - (gt + 1)));
      frag[fend - (gt + 1)] = '\0';
      text = gg_html_to_text(frag);
      free(frag);
    }
    if (text) {
      if (text[0])
        snprintf(out, cap, "%s", text);
      free(text);
    }
    if (out[0])
      return 0;
    cur = gt + 1;
  }
  return -1;
}

/*
 * 解析轮询响应 effects.html 的收件箱条目。
 * 条目容器 wire:click="selectEmail(<数字id>)"，块内
 * h3(font-semibold)=发件人、p(text-zinc-300)=主题、
 * p(line-clamp-2)=预览、span(text-xs)=相对时间。
 */
static gg_row_t *gg_parse_list(const char *block, int *out_n) {
  *out_n = 0;
  gg_row_t *rows = (gg_row_t *)calloc(GG_MAX_ROWS, sizeof(gg_row_t));
  if (!rows)
    return NULL;

  static const char marker[] = "wire:click=\"selectEmail(";
  size_t mlen = sizeof(marker) - 1;
  size_t blen = strlen(block);
  const char *cur = block;

  while (*out_n < GG_MAX_ROWS) {
    const char *pos = gg_cfind(cur, marker, blen - (size_t)(cur - block));
    if (!pos)
      break;
    /* 数字 ID（平台此值为数字） */
    const char *p = pos + mlen;
    size_t o = 0;
    while (p < block + blen && o + 1 < sizeof(rows[0].id) &&
           isdigit((unsigned char)*p))
      rows[*out_n].id[o++] = *p++;
    rows[*out_n].id[o] = '\0';
    if (!rows[*out_n].id[0]) {
      cur = pos + mlen;
      continue;
    }
    /* 条目块：本条目 div 起点 -> 下一个 marker（或块尾） */
    const char *lt = pos;
    while (lt > cur && lt[-1] != '<')
      lt--;
    const char *next = gg_cfind(pos + mlen, marker,
                                blen - (size_t)(pos + mlen - block));
    const char *end = next ? next : block + blen;

    gg_elem_text(lt, end, "h3", "font-semibold", rows[*out_n].from,
                 sizeof(rows[0].from));
    gg_elem_text(lt, end, "p", "text-zinc-300", rows[*out_n].subject,
                 sizeof(rows[0].subject));
    gg_elem_text(lt, end, "p", "line-clamp-2", rows[*out_n].preview,
                 sizeof(rows[0].preview));
    gg_elem_text(lt, end, "span", "text-xs", rows[*out_n].when,
                 sizeof(rows[0].when));

    (*out_n)++;
    cur = pos + mlen;
  }
  return rows;
}

/*
 * 解析 selectEmail 详情视图（effects.html 模态框）：
 * h3(text-xl)=主题、span 文本 "From:" 前缀=发件人、
 * div(x-show 含 activeTab === 'text')=纯文本。各字段取首命中。
 */
static void gg_parse_detail(const char *block, char *from_out, size_t fcap,
                            char *subject_out, size_t scap, char *text_out,
                            size_t tcap) {
  from_out[0] = '\0';
  subject_out[0] = '\0';
  text_out[0] = '\0';
  size_t blen = strlen(block);
  const char *cur = block;
  int have_from = 0, have_subject = 0, have_text = 0;
  while (cur < block + blen && (!have_from || !have_subject || !have_text)) {
    const char *lt = gg_cfind(cur, "<", (size_t)(block + blen - cur));
    if (!lt)
      break;
    const char *gt = strchr(lt, '>');
    if (!gt)
      break;

    if (!have_subject && gg_is_open_at(lt, "h3")) {
      char clsval[2048];
      if (gg_tag_attr(lt, gt, "class", clsval, sizeof(clsval)) == 0 &&
          gg_class_has(clsval, "text-xl")) {
        if (gg_elem_text(lt, block + blen, "h3", "text-xl", subject_out,
                         scap) == 0)
          have_subject = 1;
      }
    } else if (!have_text && gg_is_open_at(lt, "div")) {
      char xshow[4096];
      if (gg_tag_attr(lt, gt, "x-show", xshow, sizeof(xshow)) == 0 &&
          strstr(xshow, "activeTab === 'text'")) {
        char frag[65536];
        /* 提取 div 内容文本：闭合 </div> 定位（嵌套 div 取到首个闭合） */
        const char *ce = gg_cfind(gt + 1, "</div>",
                                  (size_t)(block + blen - (gt + 1)));
        const char *fend = ce ? ce : block + blen;
        size_t flen = (size_t)(fend - (gt + 1));
        if (flen < sizeof(frag)) {
          memcpy(frag, gt + 1, flen);
          frag[flen] = '\0';
          char *text = gg_html_to_text(frag);
          if (text) {
            if (text[0])
              snprintf(text_out, tcap, "%s", text);
            free(text);
            if (text_out[0])
              have_text = 1;
          }
        }
      }
    } else if (!have_from && gg_is_open_at(lt, "span")) {
      char frag[65536];
      const char *ce = gg_cfind(gt + 1, "</span>",
                                (size_t)(block + blen - (gt + 1)));
      const char *fend = ce ? ce : block + blen;
      size_t flen = (size_t)(fend - (gt + 1));
      if (flen < sizeof(frag)) {
        memcpy(frag, gt + 1, flen);
        frag[flen] = '\0';
        char *text = gg_html_to_text(frag);
        if (text) {
          char *t = gg_trim(text);
          if (strncmp(t, "From:", 5) == 0) {
            char *fname = gg_trim(t + 5);
            snprintf(from_out, fcap, "%s", fname);
            have_from = 1;
          }
          free(text);
        }
      }
    }
    cur = gt + 1;
  }
}

/* ========== 对外接口 ========== */

/*
 * 创建 temp-mail.gg 临时邮箱
 * GET 首页取 data-csrf + wire:snapshot → update calls=generateEmail →
 * 响应快照 data.email 输出邮箱；凭据串 {email,csrf,snapshot} 落 token。
 */
tm_email_info_t *tm_provider_temp_mail_gg_generate(void) {
  gg_cookie_clear();
  gg_snapshot_clear();

  /* 步骤 1：GET / 首页建立会话并提取 CSRF + 初始快照 */
  char url[64];
  snprintf(url, sizeof(url), "%s", GG_BASE);
  tm_http_response_t *home = gg_do(TM_HTTP_GET, url, gg_html_headers, NULL);
  if (!home || home->status < 200 || home->status >= 300) {
    TM_LOG_ERR("temp-mail-gg: 首页 http %ld", home ? home->status : -1);
    tm_http_response_free(home);
    return NULL;
  }
  gg_cookie_absorb(home->cookies);

  char csrf[2048];
  char snap[GG_SNAP_STATIC_CAP];
  if (gg_home_extract(home->body ? home->body : "", csrf, sizeof(csrf), snap,
                      sizeof(snap)) != 0 ||
      !csrf[0] || !snap[0]) {
    TM_LOG_ERR("temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱");
    tm_http_response_free(home);
    return NULL;
  }
  tm_http_response_free(home);

  /* 步骤 2：update calls=generateEmail 建箱 */
  size_t payload_cap = strlen(snap) * 2 + 256;
  char *payload = (char *)malloc(payload_cap);
  if (!payload)
    return NULL;
  snprintf(payload, payload_cap,
           "{\"_token\":\"%s\",\"components\":[{\"snapshot\":%s,"
           "\"updates\":{},\"calls\":[{\"path\":\"\",\"method\":"
           "\"generateEmail\",\"params\":[]}]}]}",
           csrf, snap);
  tm_http_response_t *resp = gg_update_do(payload);
  free(payload);
  if (gg_update_ok(resp) != 0)
    return NULL;
  cJSON *jresp = cJSON_Parse(resp->body);
  tm_http_response_free(resp);
  if (!jresp) {
    TM_LOG_ERR("temp-mail-gg: 解析 update 响应失败");
    return NULL;
  }
  cJSON *jcomp = cJSON_GetArrayItem(
      cJSON_GetObjectItemCaseSensitive(jresp, "components"), 0);
  if (!cJSON_IsObject(jcomp)) {
    TM_LOG_ERR("temp-mail-gg: generateEmail 响应异常（components 缺失）");
    cJSON_Delete(jresp);
    return NULL;
  }
  cJSON *jsnap = cJSON_GetObjectItemCaseSensitive(jcomp, "snapshot");
  if (!cJSON_IsString(jsnap) || !jsnap->valuestring || !jsnap->valuestring[0]) {
    TM_LOG_ERR("temp-mail-gg: generateEmail 响应异常（components 缺失）");
    cJSON_Delete(jresp);
    return NULL;
  }
  /* 以重序列化取《原始 JSON 文本》（字段保序不变量，与原串一致） */
  char *snap2 = cJSON_PrintUnformatted(jsnap);
  if (!snap2) {
    cJSON_Delete(jresp);
    return NULL;
  }
  /* 解析快照文本取 data.email */
  char *email_copy = NULL;
  {
    cJSON *sn = cJSON_Parse(snap2);
    if (!sn) {
      TM_LOG_ERR("temp-mail-gg: 解析快照失败");
      free(snap2);
      cJSON_Delete(jresp);
      return NULL;
    }
    const char *email = TM_JSON_STR(
        cJSON_GetObjectItemCaseSensitive(
            cJSON_GetObjectItemCaseSensitive(sn, "data"), "email"),
        "");
    char tmp[512];
    snprintf(tmp, sizeof(tmp), "%s", email);
    char *et = gg_trim(tmp);
    if (!et[0]) {
      TM_LOG_ERR("temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录"
                 "配额（每时段 5 个）");
      cJSON_Delete(sn);
      free(snap2);
      cJSON_Delete(jresp);
      return NULL;
    }
    email_copy = tm_strdup(et);
    cJSON_Delete(sn);
    if (!email_copy) {
      free(snap2);
      cJSON_Delete(jresp);
      return NULL;
    }
  }
  cJSON_Delete(jresp);

  /* 快照落文件内缓存（读信复用本轮凭据快照） */
  gg_snapshot_save(snap2);

  /* 步骤 3：token = 前缀 + JSON {email, csrf, snapshot} */
  cJSON *sess = cJSON_CreateObject();
  if (!sess) {
    free(snap2);
    free(email_copy);
    return NULL;
  }
  cJSON_AddStringToObject(sess, "email", email_copy);
  cJSON_AddStringToObject(sess, "csrf", csrf);
  cJSON_AddStringToObject(sess, "snapshot", snap2);
  char *sess_str = cJSON_PrintUnformatted(sess);
  cJSON_Delete(sess);
  free(snap2);
  if (!sess_str) {
    free(email_copy);
    return NULL;
  }

  size_t tcap = GG_PREFIX_LEN + strlen(sess_str) + 1;
  char *tok = (char *)malloc(tcap);
  if (!tok) {
    free(sess_str);
    free(email_copy);
    return NULL;
  }
  snprintf(tok, tcap, "%s%s", GG_TOKEN_PREFIX, sess_str);
  free(sess_str);

  tm_email_info_t *info = tm_email_info_new();
  if (!info) {
    free(tok);
    free(email_copy);
    return NULL;
  }
  info->channel = CHANNEL_TEMP_MAIL_GG;
  info->email = email_copy; /* 转移所有权 */
  info->token = tok;
  /* 30 分钟无活动过期的时效语义（毫秒时间戳） */
  info->expires_at = ((long long)time(NULL) + 1800) * 1000;
  return info;
}

/* 解析凭据串（前缀校验 + JSON 解析 + 快照落缓存）；失败 -1 */
static int gg_decode_session(const char *token, char *email_out, size_t ecap,
                             char *csrf_out, size_t ccap) {
  email_out[0] = '\0';
  csrf_out[0] = '\0';
  if (!token || strncmp(token, GG_TOKEN_PREFIX, GG_PREFIX_LEN) != 0) {
    TM_LOG_ERR("temp-mail-gg: 凭据串前缀不符，请重新 Generate");
    return -1;
  }
  cJSON *sess = cJSON_Parse(token + GG_PREFIX_LEN);
  if (!sess) {
    TM_LOG_ERR("temp-mail-gg: 解析凭据串失败");
    return -1;
  }
  const char *e = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "email"), "");
  const char *c = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "csrf"), "");
  const char *s = TM_JSON_STR(cJSON_GetObjectItemCaseSensitive(sess, "snapshot"), "");
  if (!e[0] || !s[0]) {
    TM_LOG_ERR("temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate");
    cJSON_Delete(sess);
    return -1;
  }
  {
    char tmp[512];
    snprintf(tmp, sizeof(tmp), "%s", e);
    snprintf(email_out, ecap, "%s", gg_trim(tmp));
  }
  snprintf(csrf_out, ccap, "%s", c);
  /* 快照随会话立即入缓存（覆盖上一轮旧值） */
  gg_snapshot_save(s);
  cJSON_Delete(sess);
  return 0;
}

/*
 * 读取 temp-mail.gg 收件箱
 * 轮询 update（calls 空）取列表 → 逐封 selectEmail 提取详情正文。
 * 轮询响应快照 data.email 与请求邮箱不一致即返回错误（防会话串箱）。
 */
tm_email_t *tm_provider_temp_mail_gg_get_emails(const char *email,
                                                const char *token,
                                                int *count) {
  *count = 0;
  if (!token || !token[0]) {
    *count = -1;
    return NULL;
  }
  if (!email || !email[0]) {
    TM_LOG_ERR("temp-mail-gg: 邮箱为空，请重新 Generate");
    *count = -1;
    return NULL;
  }

  char sess_email[512];
  char csrf[2048];
  if (gg_decode_session(token, sess_email, sizeof(sess_email), csrf,
                        sizeof(csrf)) != 0) {
    *count = -1;
    return NULL;
  }
  if (strcasecmp(sess_email, gg_trim((char *)email)) != 0) {
    TM_LOG_ERR("temp-mail-gg: 邮箱与凭据不匹配");
    *count = -1;
    return NULL;
  }

  /* 1) 轮询 update（calls 空 = 平台 20 秒自动刷新形态） */
  char *snap = gg_snapshot_load();
  if (!snap) {
    *count = -1;
    return NULL;
  }
  {
    cJSON *jresp = gg_update_component(snap, csrf, NULL, 0);
    free(snap);
    snap = NULL;
    if (!jresp) {
      *count = -1;
      return NULL;
    }

    cJSON *jcomp = cJSON_GetArrayItem(
        cJSON_GetObjectItemCaseSensitive(jresp, "components"), 0);
    /* 会话切换断言：响应快照 data.email 非空且 != 请求邮箱 */
    {
      char *s2 = gg_snapshot_load();
      if (s2) {
        cJSON *sn = cJSON_Parse(s2);
        free(s2);
        if (sn) {
          const char *cur_email = TM_JSON_STR(
              cJSON_GetObjectItemCaseSensitive(
                  cJSON_GetObjectItemCaseSensitive(sn, "data"), "email"),
              "");
          char tmp[512];
          snprintf(tmp, sizeof(tmp), "%s", cur_email);
          char *ct = gg_trim(tmp);
          if (ct[0] && strcasecmp(ct, sess_email) != 0) {
            cJSON_Delete(sn);
            cJSON_Delete(jresp);
            TM_LOG_ERR("temp-mail-gg: 会话已被切换");
            *count = -1;
            return NULL;
          }
          cJSON_Delete(sn);
        }
      }
    }

    /* effects.html 空则会话可能已失效 */
    cJSON *effects =
        cJSON_GetObjectItemCaseSensitive(jcomp, "effects");
    const char *html = TM_JSON_STR(
        cJSON_GetObjectItemCaseSensitive(effects, "html"), "");
    size_t hlen = strlen(html);
    char *html_copy = (char *)malloc(hlen + 1);
    if (!html_copy) {
      cJSON_Delete(jresp);
      *count = -1;
      return NULL;
    }
    memcpy(html_copy, html, hlen + 1);
    char *ht = gg_trim(html_copy);
    if (!ht[0]) {
      free(html_copy);
      cJSON_Delete(jresp);
      TM_LOG_ERR("temp-mail-gg: 轮询响应无 effects.html，会话可能已失效");
      *count = -1;
      return NULL;
    }

    /* 2) 列表解析 */
    int nrows = 0;
    gg_row_t *rows = gg_parse_list(ht, &nrows);
    free(html_copy);
    cJSON_Delete(jresp);
    if (!rows || nrows == 0) {
      free(rows);
      return NULL; /* 空箱，count 保持 0 */
    }

    *count = nrows;
    tm_email_t *emails = tm_emails_new(nrows);
    if (!emails) {
      *count = -1;
      free(rows);
      return NULL;
    }

    /* 3) 逐封 selectEmail 拉详情；失败回退列表字段 */
    for (int i = 0; i < nrows; i++) {
      cJSON *raw = cJSON_CreateObject();
      if (!raw)
        continue;
      cJSON_AddStringToObject(raw, "id", rows[i].id);
      if (rows[i].from[0])
        cJSON_AddStringToObject(raw, "from", rows[i].from);
      if (rows[i].subject[0])
        cJSON_AddStringToObject(raw, "subject", rows[i].subject);
      char datebuf[48];
      gg_relative_to_utc(rows[i].when, datebuf, sizeof(datebuf));
      cJSON_AddStringToObject(raw, "date", datebuf);

      /* 归一基础项（text/html 由下方按 Go 端语义手工兜底，不走
       * normalize 的互兜底，避免 html 兜底用列表预览而非详情正文） */
      emails[i] = tm_normalize_email(raw, email);
      cJSON_Delete(raw);
      free(emails[i].text);
      free(emails[i].html);
      emails[i].text = tm_strdup(rows[i].preview);
      emails[i].html = NULL;

      /* 详情二拉（数字 ID；成功后覆盖 from/subject/text） */
      {
        long idnum = strtol(rows[i].id, NULL, 10);
        if (idnum > 0) {
          char *csnap = gg_snapshot_load();
          if (csnap) {
            cJSON *jresp2 = gg_update_component(csnap, csrf, "selectEmail",
                                                idnum);
            free(csnap);
            if (jresp2) {
              cJSON *jcomp2 = cJSON_GetArrayItem(
                  cJSON_GetObjectItemCaseSensitive(jresp2, "components"), 0);
              if (cJSON_IsObject(jcomp2)) {
                cJSON *eff2 =
                    cJSON_GetObjectItemCaseSensitive(jcomp2, "effects");
                int is_str = 0;
                const char *html2 = TM_JSON_STR(
                    cJSON_GetObjectItemCaseSensitive(eff2, "html"), "");
                is_str = html2[0] != '\0';
                if (is_str) {
                  char dfrom[768], dsubj[2048], dtext[65536];
                  gg_parse_detail(html2, dfrom, sizeof(dfrom), dsubj,
                                  sizeof(dsubj), dtext, sizeof(dtext));
                  if (dfrom[0]) {
                    free(emails[i].from_addr);
                    emails[i].from_addr = tm_strdup(dfrom);
                  }
                  if (dsubj[0]) {
                    free(emails[i].subject);
                    emails[i].subject = tm_strdup(dsubj);
                  }
                  if (dtext[0]) {
                    free(emails[i].text);
                    emails[i].text = tm_strdup(dtext);
                  }
                }
              }
              cJSON_Delete(jresp2);
            }
          }
        }
      }

      /* 4) 兜底：text 空→subject；html 空→pre 包装 text */
      if (!emails[i].text || !emails[i].text[0]) {
        free(emails[i].text);
        emails[i].text =
            tm_strdup(emails[i].subject ? emails[i].subject : "");
      }
      if (!emails[i].html || !emails[i].html[0]) {
        char *esc = gg_html_escape(emails[i].text ? emails[i].text : "");
        if (esc) {
          size_t hcap = strlen(esc) + 64;
          emails[i].html = (char *)malloc(hcap);
          if (emails[i].html) {
            snprintf(emails[i].html, hcap,
                     "<html><body><pre>%s</pre></body></html>", esc);
          }
          free(esc);
        }
      }
    }

    free(rows);
    return emails;
  }
}