<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Huskmail 渠道实现（huskmail.xyz，API 域 api.huskmail.space）
 *
 * POST /api/v1/accounts 建箱（body {}，响应 id/address/password/token/expiresAt/tier，token 为 JWT），
 * GET /v1/messages 读信（Header Authorization: Bearer <token>，响应 {"messages":[...]}），
 * GET /v1/messages/{id} 取单封详情（Bearer）。
 * 收信域固定为 @huskmail.xyz（huskmail.space 无 MX）。
 */
final class Huskmail
{
    private const CHANNEL = 'huskmail';
    private const BASE = 'https://api.huskmail.space';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
        'Content-Type' => 'application/json',
    ];

    /**
     * 组装读信请求头（可选附带 Bearer token）。
     *
     * @return array<string,string>
     */
    private static function authHeaders(?string $token): array
    {
        $h = self::HEADERS;
        unset($h['Content-Type']);
        if ($token !== null && $token !== '') {
            $h['Authorization'] = 'Bearer ' . $token;
        }
        return $h;
    }

    /** 从列表元素提取邮件 ID，候选字段 id/Id/slug/messageId/message_id */
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

    /** 拉取单封邮件详情，失败返回 null。 */
    private static function fetchDetail(?string $token, string $messageId): ?array
    {
        $resp = HttpClient::get(
            self::BASE . '/v1/messages/' . rawurlencode($messageId),
            self::authHeaders($token),
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return null;
        }
        $data = HttpClient::json($resp);
        return is_array($data) ? $data : null;
    }

    public static function generate(): EmailInfo
    {
        $resp = HttpClient::post(self::BASE . '/v1/accounts', self::HEADERS, json: []);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('huskmail: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $address = trim((string) ($data['address'] ?? ''));
        $token = trim((string) ($data['token'] ?? ''));
        if ($address === '' || $token === '') {
            throw new \RuntimeException('huskmail: 创建邮箱响应缺少必要字段');
        }
        $expiresAt = null;
        if (is_numeric($data['expiresAt'] ?? null)) {
            // 与 Go 端一致：秒级过期时间戳转为毫秒透传
            $expiresAt = (int) ((float) $data['expiresAt'] * 1000);
        }
        return new EmailInfo(self::CHANNEL, $address, $token, expiresAt: $expiresAt);
    }

    /**
     * 流程：GET /v1/messages 取列表（{"messages":[...]}），对每个元素按 id 逐封
     * GET /v1/messages/{id} 合并详情；详情失败时以列表摘要归一。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $token = trim($token ?? '');
        if ($token === '') {
            throw new \InvalidArgumentException('huskmail: token 不能为空');
        }
        $resp = HttpClient::get(self::BASE . '/v1/messages', self::authHeaders($token));
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('huskmail: 获取邮件列表失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $list = $data['messages'] ?? null;
        if (!is_array($list)) {
            return [];
        }

        $out = [];
        foreach ($list as $m) {
            if (!is_array($m)) {
                continue;
            }
            $id = self::messageIdOf($m);
            if ($id === '') {
                $out[] = Normalize::email($m, $email);
                continue;
            }
            // 详情仅填补列表缺失字段（列表已有键优先，与 Go 端一致）
            $detail = self::fetchDetail($token, $id);
            $row = is_array($detail) ? array_merge($detail, $m) : $m;
            $row['to'] = $row['to'] ?? $email;
            $out[] = Normalize::email($row, $email);
        }
        return $out;
    }
}