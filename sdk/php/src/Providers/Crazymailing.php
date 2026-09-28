<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Crazymailing 渠道实现（crazymailing.com）
 *
 * Next.js 全栈站点，API 结构：
 *   POST /api/mailbox（空 JSON body）建箱，响应
 *     {"mailbox":{"id":"...","address":"...@crazymailing.com","expiresAt":"<RFC3339>"}}；
 *   GET /api/messages?mailbox=<完整地址 URL 编码> 读信，响应 {"messages":[...]}；
 *   GET /api/message/{id}/body 取单封正文（完整 HTML 页面）。
 * 请求需携带 Origin/Referer 浏览器形态头。
 */
final class Crazymailing
{
    private const CHANNEL = 'crazymailing';
    private const BASE = 'https://crazymailing.com';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Content-Type' => 'application/json',
        'Accept' => 'application/json',
        'Origin' => self::BASE,
        'Referer' => self::BASE . '/',
    ];

    /**
     * 创建临时邮箱：POST /api/mailbox（空 JSON body）；
     * 域名由服务端统一分配（当前仅 @crazymailing.com）。
     */
    public static function generate(): EmailInfo
    {
        $resp = HttpClient::post(self::BASE . '/api/mailbox', self::HEADERS, json: []);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('crazymailing: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $mailbox = $data['mailbox'] ?? null;
        if (!is_array($mailbox)) {
            throw new \RuntimeException('crazymailing: 创建响应缺少 mailbox');
        }
        $address = trim((string) ($mailbox['address'] ?? ''));
        if ($address === '') {
            throw new \RuntimeException('crazymailing: 创建响应缺少 mailbox.address');
        }
        $expiresStr = $mailbox['expiresAt'] ?? null;
        $expiresAt = null;
        if (is_string($expiresStr) && $expiresStr !== '') {
            $ts = strtotime($expiresStr);
            $expiresAt = $ts !== false ? $ts * 1000 : null;
        }
        return new EmailInfo(self::CHANNEL, $address, (string) ($mailbox['id'] ?? ''), expiresAt: $expiresAt);
    }

    /**
     * 读取收件箱：GET /api/messages?mailbox=<完整地址>；
     * 对每个元素逐封 GET /api/message/{id}/body 拉取正文（失败不阻断列表）。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        if ($addr === '') {
            throw new \InvalidArgumentException('crazymailing: 邮箱地址为空');
        }
        $resp = HttpClient::get(
            self::BASE . '/api/messages',
            self::HEADERS,
            query: ['mailbox' => $addr],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('crazymailing: 读取收件箱失败 http ' . $resp->getStatusCode());
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
            if (!array_key_exists('to', $m)) {
                $m['to'] = $addr;
            }
            $id = self::messageIdOf($m);
            if ($id !== '') {
                // 列表元素为摘要，正文须逐封二拉（失败不阻断）
                $html = self::getBody($id);
                if ($html !== '') {
                    $m['html'] = $html;
                }
            }
            $out[] = Normalize::email($m, $addr);
        }
        return $out;
    }

    /** 从列表元素提取邮件 ID，候选字段 id/Id/slug/messageId/message_id。 */
    private static function messageIdOf(array $m): string
    {
        foreach (['id', 'Id', 'slug', 'messageId', 'message_id'] as $key) {
            $v = $m[$key] ?? null;
            if (is_scalar($v) && $v !== null && trim((string) $v) !== '') {
                return trim((string) $v);
            }
        }
        return '';
    }

    /** 拉取单封正文（GET /api/message/{id}/body，响应为完整 HTML 页面）。 */
    private static function getBody(string $id): string
    {
        $resp = HttpClient::get(
            self::BASE . '/api/message/' . rawurlencode($id) . '/body',
            [
                'User-Agent' => self::HEADERS['User-Agent'],
                'Accept' => 'text/html,application/xhtml+xml,*/*;q=0.8',
                'Origin' => self::BASE,
                'Referer' => self::BASE . '/',
            ],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return '';
        }
        return (string) $resp->getBody();
    }
}