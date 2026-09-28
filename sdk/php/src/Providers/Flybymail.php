<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Flybymail 渠道实现（flybymail.com）
 *
 * POST /api/recipients 建箱（空 JSON body，响应 id/email/createdAt/expiresAt，
 *   expiresAt 为毫秒时间戳），GET /api/recipients/{email}/emails 读信
 *   （按邮箱地址、非 id 查询，响应 {"emails":[...]}）。
 * 信件字段：id/from/to/subject/body（纯文本）/htmlBody（HTML 正文）/preview/time/read。
 */
final class Flybymail
{
    private const CHANNEL = 'flybymail';
    private const BASE = 'https://flybymail.com';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Content-Type' => 'application/json',
        'Accept' => 'application/json',
    ];

    /**
     * 创建 flybymail.com 临时邮箱。
     * POST /api/recipients（空 JSON body）返回 id/email/createdAt/expiresAt，
     * expiresAt 为毫秒时间戳（约 4 小时）。
     */
    public static function generate(): EmailInfo
    {
        $resp = HttpClient::post(self::BASE . '/api/recipients', self::HEADERS, json: []);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('flybymail: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $id = trim((string) ($data['id'] ?? ''));
        $email = trim((string) ($data['email'] ?? ''));
        if ($id === '' || $email === '' || !str_contains($email, '@')) {
            throw new \RuntimeException('flybymail: 创建邮箱响应缺少必要字段');
        }
        $expiresAt = ((int) ($data['expiresAt'] ?? 0)) > 0 ? (int) $data['expiresAt'] : null;
        return new EmailInfo(self::CHANNEL, $email, $id, expiresAt: $expiresAt);
    }

    /**
     * 获取 flybymail.com 邮件列表。
     * GET /api/recipients/{email}/emails（按地址查询）返回 {"emails":[...]}。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        if ($addr === '' || !str_contains($addr, '@')) {
            throw new \InvalidArgumentException('flybymail: 邮箱地址为空或格式错误');
        }
        $resp = HttpClient::get(self::BASE . '/api/recipients/' . rawurlencode($addr) . '/emails', self::HEADERS);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('flybymail: 获取邮件列表失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $emails = $data['emails'] ?? null;
        if (!is_array($emails)) {
            return [];
        }

        $out = [];
        foreach ($emails as $raw) {
            if (!is_array($raw)) {
                continue;
            }
            // id 为数字时按字符串归一，time 为毫秒时间戳时按 timestamp 归一
            $row = [
                'id' => trim((string) ($raw['id'] ?? '')),
                'from' => $raw['from'] ?? null,
                'to' => $raw['to'] ?? null,
                'subject' => $raw['subject'] ?? null,
                'text' => $raw['body'] ?? null,
                'html' => $raw['htmlBody'] ?? null,
                'time' => $raw['time'] ?? null,
                'read' => $raw['read'] ?? null,
                'attachments' => $raw['attachments'] ?? null,
                'timestamp' => array_key_exists('time', $raw) && $raw['time'] !== null
                    ? $raw['time']
                    : ($raw['date'] ?? null),
            ];
            $out[] = Normalize::email($row, $addr);
        }
        return $out;
    }
}