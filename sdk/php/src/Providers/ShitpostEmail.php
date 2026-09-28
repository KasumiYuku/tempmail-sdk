<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * ShitpostEmail 渠道实现（shitpost.email 公共实例）
 *
 * 无认证 REST：POST /api/create 建箱（username/domain/ttl → email/token/type/expires），
 * GET /api/inbox?email=&token= 读信（messages[] 含 from/fromName/subject/text/html/date）。
 * 域名池：shitpost.email / letsfuckingpiss.party（克隆自 shamu4life/throwaway-email 公共实例）。
 */
final class ShitpostEmail
{
    private const CHANNEL = 'shitpost-email';
    private const BASE = 'https://shitpost.email';

    /** @var string[] 平台域名池 */
    private const DOMAINS = ['shitpost.email', 'letsfuckingpiss.party'];

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
        'Content-Type' => 'application/json',
    ];

    /** @return array<string,string> */
    private static function getHeaders(): array
    {
        $h = self::HEADERS;
        unset($h['Content-Type']);
        return $h;
    }

    /** 生成 "sdk"+10 位小写字母数字的本地用户名 */
    private static function localName(): string
    {
        $chars = 'abcdefghijklmnopqrstuvwxyz0123456789';
        $suffix = '';
        for ($i = 0; $i < 10; $i++) {
            $suffix .= $chars[random_int(0, strlen($chars) - 1)];
        }
        return 'sdk' . $suffix;
    }

    public static function generate(): EmailInfo
    {
        $domain = self::DOMAINS[random_int(0, count(self::DOMAINS) - 1)];
        $payload = [
            'username' => self::localName(),
            'domain' => $domain,
            'ttl' => 3600,
        ];
        $resp = HttpClient::post(self::BASE . '/api/create', self::HEADERS, json: $payload);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('shitpost-email: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $email = trim((string) ($data['email'] ?? ''));
        $token = trim((string) ($data['token'] ?? ''));
        if ($email === '' || $token === '') {
            throw new \RuntimeException('shitpost-email: 创建响应缺少 email 或 token');
        }

        $expires = $data['expires'] ?? null;
        return new EmailInfo(self::CHANNEL, $email, $token, expiresAt: is_numeric($expires) ? (int) $expires * 1000 : null);
    }

    /**
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $token = trim($token ?? '');
        if ($token === '') {
            throw new \InvalidArgumentException('shitpost-email: token 不能为空');
        }
        $resp = HttpClient::get(
            self::BASE . '/api/inbox',
            self::getHeaders(),
            query: ['email' => $email, 'token' => $token],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('shitpost-email: 读取收件箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $messages = $data['messages'] ?? null;
        if (!is_array($messages)) {
            return [];
        }

        $out = [];
        foreach ($messages as $m) {
            if (!is_array($m)) {
                continue;
            }
            $raw = [
                'from' => $m['from'] ?? ($m['fromName'] ?? ''),
                'to' => $email,
                'subject' => $m['subject'] ?? '',
                'text' => $m['text'] ?? '',
                'html' => $m['html'] ?? '',
                'date' => $m['date'] ?? '',
            ];
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }
}