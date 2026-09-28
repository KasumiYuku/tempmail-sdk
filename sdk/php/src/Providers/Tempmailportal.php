<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Tempmailportal 渠道实现（api.tempmailportal.com）
 *
 * POST /api/v2/inbox 建箱（body {}，响应 address/token/private/expiresAt/retentionMs，token 为 p2 前缀），
 * GET /api/messages 读信（Header Authorization: Bearer <token>），
 * GET /api/messages/{id} 取单封详情（Bearer）。
 * 列表元素按多候选字段归一，详情合并失败时回退列表摘要。
 */
final class Tempmailportal
{
    private const CHANNEL = 'tempmailportal';
    private const BASE = 'https://api.tempmailportal.com';

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
            self::BASE . '/api/messages/' . rawurlencode($messageId),
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
        $resp = HttpClient::post(self::BASE . '/api/v2/inbox', self::HEADERS, json: []);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmailportal: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $address = trim((string) ($data['address'] ?? ''));
        $token = trim((string) ($data['token'] ?? ''));
        if ($address === '' || $token === '') {
            throw new \RuntimeException('tempmailportal: 创建邮箱响应缺少必要字段');
        }
        $expiresStr = $data['expiresAt'] ?? null;
        $expiresAt = null;
        if (is_string($expiresStr) && $expiresStr !== '') {
            $ts = strtotime($expiresStr);
            $expiresAt = $ts !== false ? $ts * 1000 : null;
        } elseif (is_numeric($expiresStr)) {
            $expiresAt = ((float) $expiresStr) > 1e12 ? (int) $expiresStr : (int) ((float) $expiresStr * 1000);
        }
        return new EmailInfo(self::CHANNEL, $address, $token, expiresAt: $expiresAt);
    }

    /**
     * 流程：GET /api/messages 取列表，对每个元素按 id 逐封 GET 详情合并；
     * 详情失败时以列表摘要归一。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $token = trim($token ?? '');
        if ($token === '') {
            throw new \InvalidArgumentException('tempmailportal: token 不能为空');
        }
        $resp = HttpClient::get(self::BASE . '/api/messages', self::authHeaders($token));
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmailportal: 获取邮件列表失败 http ' . $resp->getStatusCode());
        }
        $list = HttpClient::json($resp);
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