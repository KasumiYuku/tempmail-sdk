<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;
use GuzzleHttp\Psr7\Response;

/**
 * Tempmailto 渠道实现（tempmailto.com，Laravel）
 *
 * 复刻前端同款协议（官方 REST apiKey 通道不可用）：
 * - GET / 首页：建立 Cookie 会话（权威邮箱随会话渲染于 #mainEmail value），
 *   并从 <meta name="csrf-token" content="..."> 提取 CSRF _token；
 * - POST /get_messages（表单 _token + captcha 留空）返回
 *   {status, mailbox, email_token, messages, histories}；
 * - 详情为站内 GET /view/{id}。
 *
 * 会话粘性：邮箱由 Cookie 承载（无独立密钥），Token 约定为邮箱本身；
 * 读信发现当前邮箱与请求邮箱不一致时用 POST /change 拉回目标邮箱。
 * 本端无全局 Cookie 罐：本类维护静态 Cookie 数组，逐请求以 Cookie 头
 * 回填并以响应 Set-Cookie 键值覆写（与 TempmailEe 显式 Cookie 头模式一致）。
 */
final class Tempmailto
{
    private const CHANNEL = 'tempmailto';
    private const BASE = 'https://tempmailto.com';

    /** 固定浏览器 UA（与前端同源调用一致） */
    private const UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
        . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36';

    /** @var array<string,string> 会话 Cookie 键值表（本渠道无全局 Cookie 罐，静态接管会话） */
    private static array $cookies = [];

    /** 将响应 Set-Cookie 逐行解析为键值覆写入静态 Cookie 表（按浏览器语义取分号前部分） */
    private static function absorbCookies(Response $resp): void
    {
        foreach ($resp->getHeader('Set-Cookie') as $sc) {
            $kv = $sc;
            $semi = strpos($sc, ';');
            if ($semi !== false) {
                $kv = substr($sc, 0, $semi);
            }
            $eq = strpos($kv, '=');
            if ($eq === false) {
                continue;
            }
            $key = trim(substr($kv, 0, $eq));
            if ($key !== '') {
                self::$cookies[$key] = trim(substr($kv, $eq + 1));
            }
        }
    }

    /** 由静态 Cookie 表拼接 Cookie 请求头（空表返回空串） */
    private static function cookieHeader(): string
    {
        $parts = [];
        foreach (self::$cookies as $k => $v) {
            $parts[] = $k . '=' . $v;
        }
        return implode('; ', $parts);
    }

    /** 浏览器页面请求头（GET 首页/详情），Cookie 头按静态表回填 */
    private static function pageHeaders(): array
    {
        $h = [
            'User-Agent' => self::UA,
            'Accept' => 'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8',
            'Accept-Language' => 'en-US,en;q=0.9',
            'Origin' => self::BASE,
            'Referer' => self::BASE . '/',
        ];
        $cookie = self::cookieHeader();
        if ($cookie !== '') {
            $h['Cookie'] = $cookie;
        }
        return $h;
    }

    /** AJAX 表单请求头（POST /get_messages、/change），Cookie 头按静态表回填 */
    private static function ajaxHeaders(): array
    {
        $h = [
            'Content-Type' => 'application/x-www-form-urlencoded; charset=UTF-8',
            'X-Requested-With' => 'XMLHttpRequest',
            'Origin' => self::BASE,
            'Referer' => self::BASE . '/',
            'User-Agent' => self::UA,
        ];
        $cookie = self::cookieHeader();
        if ($cookie !== '') {
            $h['Cookie'] = $cookie;
        }
        return $h;
    }

    /** GET 页面请求并吸收响应 Set-Cookie（会话 Cookie 轮换） */
    private static function getPage(string $path): Response
    {
        $resp = HttpClient::get(self::BASE . $path, self::pageHeaders());
        self::absorbCookies($resp);
        return $resp;
    }

    /** POST 表单请求（form_params 自动 urlencode）并吸收响应 Set-Cookie */
    private static function postForm(string $path, array $form): Response
    {
        $resp = HttpClient::post(self::BASE . $path, self::ajaxHeaders(), form: $form);
        self::absorbCookies($resp);
        return $resp;
    }

    /**
     * 创建 tempmailto.com 临时邮箱。
     *
     * GET 首页建立 Cookie 会话并提取服务端渲染的当前邮箱（#mainEmail value），
     * 同一会话内邮箱不变；Token 约定为邮箱本身（无独立密钥）。
     * 邮箱约 10 分钟无活动后过期。
     */
    public static function generate(): EmailInfo
    {
        $resp = self::getPage('');
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmailto: 首页 http ' . $resp->getStatusCode());
        }
        $page = (string) $resp->getBody();
        $email = '';
        if (preg_match('/(?is)id="mainEmail"[^>]*\bvalue="([^"]+)"/', $page, $m) === 1) {
            $email = trim($m[1]);
        }
        if ($email === '') {
            throw new \RuntimeException('tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱');
        }
        return new EmailInfo(self::CHANNEL, $email, $email);
    }

    /**
     * 读取 tempmailto.com 当前邮箱的收件箱。
     *
     * 流程：GET 首页取 CSRF -> POST /get_messages 得 mailbox/messages ->
     * 会话邮箱与请求邮箱不一致时 POST /change（内置重新取 CSRF）拉回 ->
     * 重新读 messages -> 逐封 GET /view/{id} 补正文 -> 归一化输出。
     * token 为 Generate 约定的邮箱（防御性非空即可，无密钥用途）。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $email = trim($email);
        if ($email === '') {
            throw new \InvalidArgumentException('tempmailto: 邮箱为空，请重新 Generate');
        }

        $data = self::fetchMessages();

        // 会话粘性：当前邮箱与请求目标不一致 -> change 拉回（change 内置重新取 CSRF）
        $mailbox = trim((string) ($data['mailbox'] ?? ''));
        if ($mailbox !== '' && strcasecmp($mailbox, $email) !== 0) {
            $changed = self::change($email);
            if (strcasecmp($changed, $email) !== 0) {
                throw new \RuntimeException('tempmailto: 会话邮箱无法拉回请求邮箱');
            }
            $data = self::fetchMessages();
        }

        $out = [];
        $messages = $data['messages'] ?? null;
        if (!is_array($messages)) {
            return [];
        }
        foreach ($messages as $m) {
            if (!is_array($m)) {
                continue;
            }
            $raw = self::normalizeItem($m, $email);
            if ($raw === null) {
                continue;
            }
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }

    /** 拉首页并提取 CSRF _token（读信/换箱共用） */
    private static function csrfToken(): string
    {
        $resp = self::getPage('');
        $page = (string) $resp->getBody();
        if (preg_match('/<meta\s+name="csrf-token"\s+content="([^"]+)"/', $page, $m) === 1) {
            $token = trim($m[1]);
            if ($token !== '') {
                return $token;
            }
        }
        throw new \RuntimeException('tempmailto: 首页未找到 csrf-token');
    }

    /**
     * POST /get_messages（_token + captcha 留空）拉取当前会话收件箱。
     *
     * @return array<mixed> status/mailbox/email_token/messages/histories
     */
    private static function fetchMessages(): array
    {
        $csrf = self::csrfToken();
        $resp = self::postForm('/get_messages', ['_token' => $csrf, 'captcha' => '']);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmailto 读信: http ' . $resp->getStatusCode());
        }
        return HttpClient::json($resp);
    }

    /**
     * 换箱：POST /change（_token + name + domain），返回变更后的当前邮箱。
     * local 部分缺失时回退 "TmSdk"，domain 缺失时回退 "tempmailto.com"。
     */
    private static function change(string $email): string
    {
        $csrf = self::csrfToken();
        $at = strpos($email, '@');
        $name = $at === false ? $email : substr($email, 0, $at);
        if ($name === '') {
            $name = 'TmSdk';
        }
        $domain = 'tempmailto.com';
        if ($at !== false && $at + 1 < strlen($email)) {
            $domain = substr($email, $at + 1);
        }
        $resp = self::postForm('/change', ['_token' => $csrf, 'name' => $name, 'domain' => $domain]);
        $data = HttpClient::json($resp);
        $mailbox = trim((string) ($data['mailbox'] ?? ''));
        if ($mailbox === '') {
            throw new \RuntimeException('tempmailto: change 响应异常');
        }
        return $mailbox;
    }

    /** 按候选键顺序提取标量值（字符串 trim；int/float 转十进制；其它类型跳过） */
    private static function pickStr(array $row, array $keys): string
    {
        foreach ($keys as $key) {
            $v = $row[$key] ?? null;
            if (is_string($v)) {
                $s = trim($v);
                if ($s !== '') {
                    return $s;
                }
            } elseif (is_int($v) || is_float($v)) {
                return (string) $v;
            }
        }
        return '';
    }

    /** 将 is_seen 归一为布尔已读标记，兼容 bool / 数字(非0) / string("true"|"1" 忽略大小写) */
    private static function readOf(mixed $v): bool
    {
        if (is_bool($v)) {
            return $v;
        }
        if (is_numeric($v)) {
            return (float) $v !== 0.0;
        }
        if (is_string($v)) {
            $s = trim($v);
            return strtolower($s) === 'true' || $s === '1';
        }
        return false;
    }

    /**
     * 将 messages 列表行组装为归一化骨架数组（id 缺失视为无效行返回 null）。
     * from 优先 from_name，其次 from_email/from；正文来源：详情页
     * GET /view/{id}（同一 Cookie 会话），提取失败回退列表字段，最终
     * 无正文回退 subject。
     *
     * @param array<mixed> $row
     * @return array<string,mixed>|null
     */
    private static function normalizeItem(array $row, string $email): ?array
    {
        $id = self::pickStr($row, ['id']);
        if ($id === '') {
            return null;
        }
        $fromEmail = self::pickStr($row, ['from_email', 'from']);
        $fromName = self::pickStr($row, ['from_name']);
        if ($fromName === '') {
            $fromName = $fromEmail;
        }
        $date = self::pickStr($row, ['receivedAt', 'received_at', 'createdAt']);
        if ($date === '') {
            $date = gmdate('c');
        }
        $raw = [
            'id' => $id,
            'from' => $fromName,
            'to' => $email,
            'subject' => self::pickStr($row, ['subject']),
            'text' => '',
            'html' => '',
            'date' => $date,
            'isRead' => self::readOf($row['is_seen'] ?? null),
        ];

        if ($id !== '') {
            $htmlBody = self::viewDetail($id);
            if ($htmlBody !== '') {
                $raw['html'] = $htmlBody;
                $raw['text'] = self::htmlToText($htmlBody);
            }
        }
        if ($raw['text'] === '') {
            $text = self::pickStr($row, ['body', 'text', 'snippet', 'preview']);
            $raw['text'] = $text !== '' ? $text : $raw['subject'];
        }
        if ($raw['html'] === '') {
            $raw['html'] = '<html><body><pre>' . htmlspecialchars($raw['text'], ENT_QUOTES) . '</pre></body></html>';
        }
        return $raw;
    }

    /**
     * GET /view/{id} 提取邮件正文 HTML（同一 Cookie 会话）。
     * 视图页结构以候选 class 依次尝试（div/section/article 块），
     * 全失败回退 <main>/<article> 区块；仍失败返回空串（列表归一不中断）。
     */
    private static function viewDetail(string $id): string
    {
        $resp = self::getPage('/view/' . $id);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return '';
        }
        $page = (string) $resp->getBody();
        foreach (['mail-body', 'mail_content', 'email-body', 'content-body', 'message-content', 'mail-content'] as $class) {
            $re = '/(?is)<[^>]+class="[^"]*\b' . preg_quote($class, '/') . '\b[^"]*"[^>]*>'
                . '([\s\S]*?)<\/(?:div|section|article)>/';
            if (preg_match($re, $page, $m) === 1 && trim($m[1]) !== '') {
                return trim($m[1]);
            }
        }
        if (preg_match('/(?is)<(main|article)[^>]*>([\s\S]*?)<\/\1>/', $page, $m) === 1 && trim($m[2]) !== '') {
            return trim($m[2]);
        }
        return '';
    }

    /** 详情 HTML 转纯文本（去 script/style/标签 + 反转义 + 空白压缩） */
    private static function htmlToText(string $src): string
    {
        $cleaned = preg_replace('/<(script|style)[^>]*>.*?<\/\1>/is', ' ', $src) ?? $src;
        $cleaned = strip_tags($cleaned);
        $cleaned = html_entity_decode($cleaned, ENT_QUOTES | ENT_HTML5);
        return trim(preg_replace('/\s+/', ' ', $cleaned) ?? $cleaned);
    }
}