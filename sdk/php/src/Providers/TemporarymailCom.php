<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Temporarymail 渠道实现（temporarymail.com）
 *
 * 无认证 REST（key 为空即随机建箱）：
 *   GET /api/?action=requestEmailAccess&key=&value=random 建箱，
 *   响应 {"address":"...","secretKey":"..."}，secretKey 用于后续 checkInbox。
 * 读信 GET /api/?action=checkInbox&value=<secretKey>，响应有两种形态：
 *   空收件箱为 []，有信时为 map[id]→邮件元数据对象。
 * 详情 POST /api/?action=getEmail&value=<id> 覆盖真实主题（列表常为 "[No Subject]"）；
 * 全文 GET /view/?i=<id> 返回 HTML 化网页，本地剥标签还原纯文本。
 * 地址最长周期固定为 4 小时。
 */
final class TemporarymailCom
{
    private const CHANNEL = 'temporarymail-com';
    private const BASE = 'https://temporarymail.com';

    /** 备用浏览器 UA（403 重试用，规避共享池随机 UA 耗尽） */
    private const ALT_UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
        . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Accept' => 'application/json, text/plain, */*',
        'Accept-Language' => 'en-US,en;q=0.9',
        'Sec-Fetch-Site' => 'same-origin',
        'Sec-Fetch-Mode' => 'cors',
        'Sec-Fetch-Dest' => 'empty',
        'Referer' => self::BASE . '/',
        'Origin' => self::BASE,
    ];

    /** 构造 /api/ 请求头（403 时换备用 UA 重试）。 */
    private static function apiHeaders(string $ua): array
    {
        $h = self::HEADERS;
        $h['User-Agent'] = $ua;
        return $h;
    }

    /**
     * 创建 temporarymail.com 临时邮箱。
     * GET /api/?action=requestEmailAccess&key=&value=random；token 复用 secretKey。
     */
    public static function generate(): EmailInfo
    {
        $resp = HttpClient::get(
            self::BASE . '/api/?action=requestEmailAccess&key=&value=random',
            self::apiHeaders(self::HEADERS['User-Agent']),
        );
        if ($resp->getStatusCode() === 429) {
            throw new \RuntimeException('temporarymail: 创建邮箱平台限流(429)，请稍后重试');
        }
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('temporarymail: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $addr = trim((string) ($data['address'] ?? ''));
        $key = trim((string) ($data['secretKey'] ?? ''));
        if ($addr === '' || $key === '') {
            throw new \RuntimeException('temporarymail: 创建响应缺少 address 或 secretKey');
        }
        return new EmailInfo(self::CHANNEL, $addr, $key);
    }

    /**
     * 读取 temporarymail 收件箱。
     *
     * 列表主题常为 "[No Subject]"：逐封拉详情覆盖真实主题，并逐封抓 /view/ 全文。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $tk = trim($token ?? '');
        if ($tk === '') {
            throw new \InvalidArgumentException('temporarymail: token 不能为空');
        }
        $addr = trim($email);
        $body = self::checkInbox($tk);
        $rawList = is_array($body) ? $body : [];
        if ($rawList !== [] && !array_is_list($rawList)) {
            // 平台响应为 map[id]→对象 时归一为邮件数组
            $rawList = array_values($rawList);
        }

        $out = [];
        foreach ($rawList as $m) {
            if (!is_array($m)) {
                continue;
            }
            $row = $m;
            if (!array_key_exists('to', $row)) {
                $row['to'] = $addr;
            }
            $id = trim((string) ($row['id'] ?? ''));
            if ($id !== '') {
                // 详情覆盖真实主题/发件人（失败不致命：列表元数据兜底）
                $det = self::fetchDetail($id);
                if ($det !== null) {
                    if (trim((string) ($det['subject'] ?? '')) !== '') {
                        $row['subject'] = (string) $det['subject'];
                    }
                    if (trim((string) ($det['from'] ?? '')) !== '') {
                        $row['from'] = (string) $det['from'];
                    }
                }
                // /view/ 渲染端点全文（失败不致命：列表元数据兜底）
                $text = self::fetchView($id);
                if ($text !== '') {
                    $row['text'] = $text;
                }
            }
            $out[] = Normalize::email($row, $addr);
        }
        return $out;
    }

    /**
     * 拉取 checkInbox 响应体；403 换备用 UA 重试一次，429 报平台限流。
     *
     * @return array<mixed>
     */
    private static function checkInbox(string $token): array
    {
        $uas = [self::HEADERS['User-Agent'], self::ALT_UA];
        $lastStatus = 0;
        foreach ($uas as $ua) {
            $resp = HttpClient::get(
                self::BASE . '/api/?action=checkInbox&value=' . rawurlencode($token),
                self::apiHeaders($ua),
            );
            if ($resp->getStatusCode() === 429) {
                throw new \RuntimeException('temporarymail: 读取收件箱平台限流(429)，请拉大轮询间隔');
            }
            if ($resp->getStatusCode() >= 200 && $resp->getStatusCode() < 300) {
                return HttpClient::json($resp);
            }
            $lastStatus = $resp->getStatusCode();
            // 403/404 疑似 UA 键控风控，换备用 UA 重试一次
            if ($resp->getStatusCode() !== 403 && $resp->getStatusCode() !== 404) {
                throw new \RuntimeException('temporarymail: 读取收件箱失败 http ' . $resp->getStatusCode());
            }
        }
        throw new \RuntimeException('temporarymail: 读取收件箱失败 http ' . $lastStatus . '（两次尝试均被拒）');
    }

    /** 拉取单封详情（POST /api/?action=getEmail&value=<id>），风控失败返回 null 降级。 */
    private static function fetchDetail(string $id): ?array
    {
        $resp = HttpClient::post(
            self::BASE . '/api/?action=getEmail&value=' . rawurlencode($id),
            self::apiHeaders(self::HEADERS['User-Agent']),
        );
        if ($resp->getStatusCode() === 429 || $resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return null;
        }
        $data = HttpClient::json($resp);
        if ($data === []) {
            return null;
        }
        $first = reset($data);
        return is_array($first) ? $first : null;
    }

    /** 抓取 /view/ 渲染端点全文并还原纯文本。 */
    private static function fetchView(string $id): string
    {
        $resp = HttpClient::get(
            self::BASE . '/view/?i=' . rawurlencode($id) . '&width=800',
            [
                'Accept' => 'text/html, */*',
                'User-Agent' => self::HEADERS['User-Agent'],
                'Referer' => self::BASE . '/',
            ],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return '';
        }
        return self::viewToText((string) $resp->getBody());
    }

    /** 将 /view/ 响应剥标签还原为纯文本（<br>/<p> 换行保留）。 */
    private static function viewToText(string $src): string
    {
        $src = preg_replace('#<br\s*/?>#i', "\n", $src) ?? $src;
        $src = preg_replace('#</?p>#i', "\n", $src) ?? $src;
        $src = preg_replace('/<(script|style)[^>]*>.*?<\/\1>/is', ' ', $src) ?? $src;
        $src = preg_replace('/<[^>]+>/', ' ', $src) ?? $src;
        $src = html_entity_decode($src, ENT_QUOTES | ENT_HTML5, 'UTF-8');
        $lines = preg_split('/\n/', $src) ?: [];
        $lines = array_map(static fn (string $l): string => trim($l), $lines);
        return trim(implode("\n", $lines));
    }
}