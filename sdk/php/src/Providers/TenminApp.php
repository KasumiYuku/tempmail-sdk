<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * TenminApp 渠道实现（tenmin.app，真实 API 域 api.tenmin.app）
 *
 * 建箱+读信共用 GET /api/inbox/{localpart}（Accept: application/json）。
 * localpart 为随机 6 位小写十六进制串（首访即建箱，无显式创建接口）；
 * 响应：{"inboxId":..,"address":"..@tenmin.app","ttl":600,"count":0,"messages":[]}，
 * messages[] 元素字段：id/from/subject/text/html/receivedAt（from 为 {name,address} 对象）。
 */
final class TenminApp
{
    private const CHANNEL = 'tenmin-app';
    private const BASE = 'https://api.tenmin.app';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
    ];

    /** 生成 6 位小写十六进制随机 localpart */
    private static function localPart(): string
    {
        $chars = '0123456789abcdef';
        $suffix = '';
        for ($i = 0; $i < 6; $i++) {
            $suffix .= $chars[random_int(0, strlen($chars) - 1)];
        }
        return $suffix;
    }

    /** 请求 /api/inbox/{localpart}（建箱与读信共用），响应解码为关联数组 */
    private static function fetchInbox(string $localpart): array
    {
        $resp = HttpClient::get(self::BASE . '/api/inbox/' . rawurlencode($localpart), self::HEADERS);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tenmin-app: 读取收件箱失败 http ' . $resp->getStatusCode());
        }
        return HttpClient::json($resp);
    }

    public static function generate(): EmailInfo
    {
        // 首次 GET 随机 localpart 即自动建箱（10 分钟 TTL），token 存储 localpart
        $local = self::localPart();
        $data = self::fetchInbox($local);
        $address = trim((string) ($data['address'] ?? ''));
        if ($address === '') {
            $address = $local . '@tenmin.app';
        }
        $ttl = (int) ($data['ttl'] ?? 0);
        $expiresAt = $ttl > 0 ? (int) ((time() + $ttl) * 1000) : null;
        return new EmailInfo(self::CHANNEL, $address, $local, expiresAt: $expiresAt);
    }

    /**
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $token = trim($token ?? '');
        if ($token === '') {
            throw new \InvalidArgumentException('tenmin-app: token 为空');
        }
        $data = self::fetchInbox($token);
        $messages = $data['messages'] ?? null;
        if (!is_array($messages)) {
            return [];
        }

        $out = [];
        foreach ($messages as $m) {
            if (!is_array($m)) {
                continue;
            }
            // from 为对象（{name,address}）时拆出地址字段
            $from = $m['from'] ?? '';
            if (is_array($from)) {
                $addr = trim((string) ($from['address'] ?? ''));
                $name = trim((string) ($from['name'] ?? ''));
                if ($addr !== '' && $name !== '') {
                    $from = $name . ' <' . $addr . '>';
                } elseif ($addr !== '') {
                    $from = $addr;
                } else {
                    $from = $name;
                }
            }
            $raw = [
                'id' => $m['id'] ?? '',
                'from' => is_scalar($from) ? (string) $from : '',
                'to' => $email,
                'subject' => $m['subject'] ?? '',
                'text' => $m['text'] ?? '',
                'html' => $m['html'] ?? '',
                'date' => $m['receivedAt'] ?? '',
            ];
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }
}