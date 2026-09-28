<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;
use GuzzleHttp\Cookie\CookieJar;

/**
 * Internxt 渠道实现（internxt.com/temporary-email，Next.js + OpenNext）
 *
 * 调研实证结论（与 Go 端 internxt.go 一致）：
 *   - 建箱 GET /api/temp-mail/create-email，读信 GET
 *     /api/temp-mail/get-inbox?email=<e>&token=<t>，详情 GET
 *     /api/temp-mail/get-message?email=<e>&token=<t>&messageId=<id>。
 *     create-email 仅接受 GET（POST 返回 405 Method not allowed）。
 *   - CSRF：首次 GET /temporary-email 响应 Set-Cookie csrfSecret=...
 *     与 XSRF-TOKEN=...。数据接口校验请求头 csrf-token，其值必须与
 *     Cookie jar 中 XSRF-TOKEN 一致（每个 API 响应都会刷新 XSRF-TOKEN
 *     的 Set-Cookie，故每次读信前都应从罐中重取最新值）。
 *   - 建箱响应：{"address":"<前缀>@uberip.com","token":"<十六进制>"}。
 *   - get-inbox 正常返回顶层数组（空箱 []）；错误 token 返回 401。
 *
 * Cookie 策略：模块内自管专属 CookieJar（本渠道独占，防串池）；
 *   csrf-token 头每次请求前重取罐中 XSRF-TOKEN 最新值。
 * token 语义：{"address","token"} JSON。
 */
final class Internxt
{
    private const CHANNEL = 'internxt';
    private const SITE = 'https://internxt.com';
    private const REF = self::SITE . '/temporary-email';
    private const API_BASE = self::SITE . '/api/temp-mail';

    private const USER_AGENT = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 '
        . '(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36';

    /** 模块级专属 Cookie 罐 */
    private static ?CookieJar $jar = null;

    /** 取得专属 Cookie 罐（首次调用时初始化） */
    private static function jar(): CookieJar
    {
        if (self::$jar === null) {
            self::$jar = new CookieJar();
        }
        return self::$jar;
    }

    /** 从罐中取当前 XSRF-TOKEN 值（无则空串） */
    private static function xsrfFromJar(): string
    {
        foreach (self::jar()->toArray() as $c) {
            if (($c['Name'] ?? '') === 'XSRF-TOKEN' && ($c['Value'] ?? '') !== '') {
                return (string) $c['Value'];
            }
        }
        return '';
    }

    /**
     * 确保罐中持有本域 csrfSecret 与 XSRF-TOKEN 并返回其值
     * 无则 GET /temporary-email 页面夺取。
     */
    private static function prepareXsrf(): string
    {
        $xsrf = self::xsrfFromJar();
        if ($xsrf !== '') {
            return $xsrf;
        }
        $resp = HttpClient::get(self::REF, [
            'User-Agent' => self::USER_AGENT,
            'Accept' => 'text/html,application/xhtml+xml,application/xml;q=0.9,'
                . 'image/avif,image/webp,*/*;q=0.8',
            'Accept-Language' => 'en-US,en;q=0.9',
        ], cookies: self::jar());
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('internxt: 夺取页面失败 http ' . $resp->getStatusCode());
        }
        $xsrf = self::xsrfFromJar();
        if ($xsrf === '') {
            throw new \RuntimeException('internxt: 未取得 XSRF-TOKEN Cookie');
        }
        return $xsrf;
    }

    /**
     * 带 CSRF 头请求 internxt 数据接口（GET），返回响应
     * csrf-token 头取罐中 XSRF-TOKEN 最新值（每次请求前重取）。
     *
     * @param array<string,mixed>|null $query 查询参数
     * @return mixed
     */
    private static function apiGet(string $path, ?array $query = null)
    {
        $csrf = self::prepareXsrf();
        $resp = HttpClient::get(self::API_BASE . $path, [
            'User-Agent' => self::USER_AGENT,
            'Accept' => 'application/json, text/plain, */*',
            'Origin' => self::SITE,
            'Referer' => self::REF,
            'csrf-token' => $csrf,
        ], query: $query, cookies: self::jar());
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('internxt: ' . $path . ' 失败 http ' . $resp->getStatusCode());
        }
        $text = (string) $resp->getBody();
        return $text === '' ? null : json_decode($text, true);
    }

    /**
     * 创建 internxt.com 临时邮箱
     * 先确保罐中 XSRF-TOKEN，再 GET /api/temp-mail/create-email
     * （带 csrf-token 头），响应 {"address","token"}。
     */
    public static function generate(): EmailInfo
    {
        self::prepareXsrf();
        $data = self::apiGet('/create-email');
        if (!is_array($data)) {
            throw new \RuntimeException('internxt: 解析建箱响应失败');
        }
        $address = trim((string) ($data['address'] ?? ''));
        $apiToken = trim((string) ($data['token'] ?? ''));
        if ($address === '' || $apiToken === '' || strpos($address, '@') === false) {
            throw new \RuntimeException('internxt: 创建邮箱响应缺少必要字段（address/token）');
        }
        $tokenJson = (string) json_encode(['address' => $address, 'token' => $apiToken], JSON_UNESCAPED_SLASHES);
        return new EmailInfo(self::CHANNEL, $address, $tokenJson);
    }

    /** 从列表元素中提取邮件 ID（字符串形态，与 Go 端 messageIDOf 一致） */
    private static function messageIdOf(array $m): string
    {
        foreach (['id', 'messageId', 'message_id'] as $key) {
            $v = $m[$key] ?? null;
            if (is_scalar($v) && $v !== null && trim((string) $v) !== '') {
                return trim((string) $v);
            }
        }
        return '';
    }

    /**
     * 获取 internxt.com 收件箱
     * get-inbox 返回顶层数组（列表元素含 id/from/subject/date/seen 等）；
     * 逐条 get-message 拉单封全文（响应为单封对象，含 html 渲染全文），
     * 详情失败回退列表摘要。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $sess = json_decode((string) $token, true);
        if (!is_array($sess)) {
            throw new \InvalidArgumentException('internxt: 会话凭据解析失败（非对象）');
        }
        $address = trim((string) ($sess['address'] ?? ''));
        $apiToken = trim((string) ($sess['token'] ?? ''));
        if ($address === '' || $apiToken === '') {
            throw new \InvalidArgumentException('internxt: 会话凭据缺少必要字段');
        }
        if ($address !== $email) {
            throw new \InvalidArgumentException('internxt: 会话邮箱与查询邮箱不匹配');
        }

        $inbox = self::apiGet('/get-inbox', ['email' => $address, 'token' => $apiToken]);
        if (!is_array($inbox)) {
            throw new \RuntimeException('internxt: 解析收件箱响应失败');
        }

        $out = [];
        foreach ($inbox as $item) {
            if (!is_array($item)) {
                continue;
            }
            $mid = self::messageIdOf($item);
            if ($mid !== '') {
                try {
                    $detail = self::apiGet('/get-message', [
                        'email' => $address,
                        'token' => $apiToken,
                        'messageId' => $mid,
                    ]);
                    if (is_array($detail)) {
                        // 列表字段优先，详情仅补齐缺失字段
                        foreach ($detail as $k => $v) {
                            if (!array_key_exists($k, $item)) {
                                $item[$k] = $v;
                            }
                        }
                    }
                } catch (\Throwable) {
                    // 详情失败回退列表摘要
                }
            }
            $out[] = Normalize::email($item, $email);
        }
        return $out;
    }
}