<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * TempmailsIo 渠道实现（tempmails.io）
 *
 * 无认证 REST：POST /api/temp-mail/generate 建箱（响应 email/token/expires_at），
 * GET /api/temp-mail/inbox/{token} 读信（messages[] 含 from_email/text_body/html_body/attachments）。
 * 邮箱借用 uberip.com 等公共域（10 分钟自动过期）。
 */
final class TempmailsIo
{
    private const CHANNEL = 'tempmails-io';
    private const BASE = 'https://tempmails.io';

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

    public static function generate(): EmailInfo
    {
        $resp = HttpClient::post(self::BASE . '/api/temp-mail/generate', self::HEADERS, json: []);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmails-io: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $inner = $data['data'] ?? null;
        if (empty($data['success']) || !is_array($inner)) {
            throw new \RuntimeException('tempmails-io: 创建响应缺少必要字段');
        }
        $email = trim((string) ($inner['email'] ?? ''));
        $token = trim((string) ($inner['token'] ?? ''));
        if ($email === '' || $token === '') {
            throw new \RuntimeException('tempmails-io: 创建响应缺少 email 或 token');
        }
        return new EmailInfo(self::CHANNEL, $email, $token);
    }

    /**
     * 读取收件箱：先 POST /api/temp-mail/fetch-emails/{token} 触发平台对上游信箱的
     * 主动同步（失败不致命），再 GET /api/temp-mail/inbox/{token} 读静态收件箱。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $token = trim($token ?? '');
        if ($token === '') {
            throw new \InvalidArgumentException('tempmails-io: token 不能为空');
        }

        // 1) 触发同步（失败不致命，仍尝试静态读）
        try {
            $fetch = HttpClient::post(
                self::BASE . '/api/temp-mail/fetch-emails/' . rawurlencode($token),
                self::getHeaders(),
            );
            if ($fetch->getStatusCode() >= 400) {
                // 同步端点偶发失败，忽略继续
            }
        } catch (\Throwable $ignored) {
            // 忽略同步错误
        }

        // 2) 读静态收件箱
        $resp = HttpClient::get(
            self::BASE . '/api/temp-mail/inbox/' . rawurlencode($token),
            self::getHeaders(),
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmails-io: 读取收件箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $inner = $data['data'] ?? null;
        if (!is_array($inner)) {
            return [];
        }
        $messages = $inner['messages'] ?? null;
        if (!is_array($messages)) {
            return [];
        }

        $out = [];
        foreach ($messages as $m) {
            if (!is_array($m)) {
                continue;
            }
            $raw = [
                'from' => $m['from_email'] ?? '',
                'to' => $email,
                'subject' => $m['subject'] ?? '',
                'text' => $m['text_body'] ?? '',
                'html' => $m['html_body'] ?? '',
                'date' => $m['received_at'] ?? '',
                'attachments' => $m['attachments'] ?? null,
            ];
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }
}