<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * TempMail100 渠道实现（tempmail100.com）
 *
 * POST /init 建箱（空 body，响应 code/data.token（JWT），无 Cookie），
 * POST /web/generate 建随机地址（Header Authorization: <token>，响应 data.address），
 * GET /web/emails 读信列表（Header Authorization: <token>，响应 data.list[]/data.total）。
 *
 * 平台限制（实测确认，非 SDK 缺陷）：
 * 列表元素 content 恒为空字符串，详情端点对真实 token 返回 HTTP 200 + 空 body，
 * 因此正文永久不可得，本渠道客观为「列表-only」：subject/fromAddress/fromName/
 * timestamp/read 可正确输出，正文恒为空属平台限制。
 */
final class Tempmail100
{
    private const CHANNEL = 'tempmail100';
    private const BASE = 'https://tempmail100.com';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
    ];

    /** 设置 tempmail100 请求的通用请求头（前端使用 Authorization: <token> 不带 Bearer）。 */
    private static function authHeaders(?string $token): array
    {
        $h = self::HEADERS;
        if ($token !== null && $token !== '') {
            $h['Authorization'] = $token;
        }
        return $h;
    }

    /** 创建 tempmail100.com 临时邮箱：POST /init 取 JWT，再 POST /web/generate 创建地址。 */
    public static function generate(): EmailInfo
    {
        // 第一步：初始化取得 token
        $resp = HttpClient::post(self::BASE . '/init', self::authHeaders(null));
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmail100: 初始化失败 http ' . $resp->getStatusCode());
        }
        $init = HttpClient::json($resp);
        $token = trim((string) ($init['data']['token'] ?? ''));
        if (($init['code'] ?? null) !== 0 || $token === '') {
            throw new \RuntimeException('tempmail100: 初始化响应异常');
        }

        // 第二步：创建随机地址（Authorization: <token> 不带 Bearer）
        $resp2 = HttpClient::post(self::BASE . '/web/generate', self::authHeaders($token));
        if ($resp2->getStatusCode() < 200 || $resp2->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmail100: 创建地址失败 http ' . $resp2->getStatusCode());
        }
        $gen = HttpClient::json($resp2);
        $address = trim((string) ($gen['data']['address'] ?? ''));
        if (($gen['code'] ?? null) !== 0 || $address === '' || !str_contains($address, '@')) {
            throw new \RuntimeException('tempmail100: 创建地址响应异常');
        }
        return new EmailInfo(self::CHANNEL, $address, $token);
    }

    /**
     * 获取 tempmail100.com 邮件列表。
     * GET /web/emails（Authorization: <token> 不带 Bearer）返回 data.list/data.total。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        $resp = HttpClient::get(self::BASE . '/web/emails', self::authHeaders($token));
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmail100: 获取邮件列表失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        if (($data['code'] ?? null) !== 0) {
            throw new \RuntimeException('tempmail100: 获取邮件列表响应异常: ' . (string) ($data['message'] ?? ''));
        }
        $inner = $data['data'] ?? null;
        $list = is_array($inner) ? ($inner['list'] ?? null) : null;
        if (!is_array($list)) {
            return [];
        }

        $out = [];
        foreach ($list as $m) {
            if (!is_array($m)) {
                continue;
            }
            $out[] = Normalize::email(self::normalizeItem($m, $addr), $addr);
        }
        return $out;
    }

    /**
     * 将 /web/emails 列表元素归一为统一邮件结构。
     * fromName+fromAddress 组合为 "Name <address>" 填入 from；
     * timestamp 为毫秒值（>1e12 时按 UnixMilli 解析）；read 为布尔已读标记。
     *
     * @param array<mixed> $item
     * @return array<string,mixed>
     */
    private static function normalizeItem(array $item, string $email): array
    {
        $fromName = trim((string) ($item['fromName'] ?? ''));
        $fromAddress = trim((string) ($item['fromAddress'] ?? ''));
        if ($fromName !== '' && strtolower($fromName) !== strtolower($fromAddress) && str_contains($fromAddress, '@')) {
            $fromAddress = $fromName . ' <' . $fromAddress . '>';
        }

        return [
            'id' => trim((string) ($item['uuid'] ?? '')),
            'from' => $fromAddress,
            'to' => trim((string) ($item['toAddress'] ?? '')),
            'subject' => trim((string) ($item['subject'] ?? '')),
            'content' => trim((string) ($item['content'] ?? '')),
            'timestamp' => $item['timestamp'] ?? null,
            'isRead' => self::readOf($item['read'] ?? null),
        ];
    }

    /** 将 read 字段归一为布尔已读标记，兼容 bool / 数字(0|1) / string("true"|"1")。 */
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
}