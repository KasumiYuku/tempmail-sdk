<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * NowtempMail 渠道实现（nowtempmail.com）
 *
 * POST /mailbox 建箱（无 body 或 {}，响应 token（JWT）/mailbox），
 * GET /messages 读信列表（Header Authorization: Bearer <token>，
 *   响应 {"messages":[...]}），GET /message/{id} 取单封详情（Bearer）。
 * 列表元素按多候选字段归一；详情拉取失败时以列表摘要兜底。
 */
final class Nowtempmail
{
    private const CHANNEL = 'nowtempmail';
    private const BASE = 'https://nowtempmail.com';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
    ];

    /** 设置 nowtempmail 请求的通用请求头（可选附带 Bearer token）。 */
    private static function authHeaders(?string $token): array
    {
        $h = self::HEADERS;
        if ($token !== null && $token !== '') {
            $h['Authorization'] = 'Bearer ' . $token;
        }
        return $h;
    }

    /** 创建 nowtempmail.com 临时邮箱：POST /mailbox 返回 token（JWT）与 mailbox 地址。 */
    public static function generate(): EmailInfo
    {
        $resp = HttpClient::post(
            self::BASE . '/mailbox',
            self::authHeaders(null) + ['Content-Type' => 'application/json'],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('nowtempmail: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $token = trim((string) ($data['token'] ?? ''));
        $mailbox = trim((string) ($data['mailbox'] ?? ''));
        if ($token === '' || $mailbox === '' || !str_contains($mailbox, '@')) {
            throw new \RuntimeException('nowtempmail: 创建邮箱响应缺少必要字段');
        }
        return new EmailInfo(self::CHANNEL, $mailbox, $token);
    }

    /**
     * 获取 nowtempmail.com 邮件列表。
     * 流程：GET /messages 取列表，对每个元素按 id 逐封 GET /message/{id} 合并详情；
     * 详情失败时以列表摘要归一。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        $resp = HttpClient::get(self::BASE . '/messages', self::authHeaders($token));
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('nowtempmail: 获取邮件列表失败 http ' . $resp->getStatusCode());
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
            $id = self::messageIdOf($m);
            $detail = $id !== '' ? self::getDetail($token, $id) : null;
            $row = $detail !== null && is_array($detail) ? array_merge($detail, $m) : $m;
            $out[] = Normalize::email($row, $addr);
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

    /** 获取单封详情（GET /message/{id}，Bearer token），失败返回 null。 */
    private static function getDetail(?string $token, string $id): ?array
    {
        $resp = HttpClient::get(
            self::BASE . '/message/' . rawurlencode($id),
            self::authHeaders($token),
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return null;
        }
        $detail = HttpClient::json($resp);
        return $detail === [] ? null : $detail;
    }
}