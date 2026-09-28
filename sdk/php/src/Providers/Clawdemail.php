<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * ClawdEmail 渠道实现（clawdemail.com，API 域 api.clawdemail.com）
 *
 * POST /register 建箱（无验证码，body {"name":"xxx"}，响应 success/email/token），
 * GET /inbox 读信列表（Header Authorization: Bearer <token>，
 *   响应 success/email/count/unread/emails[]），GET /email/{id} 取单封详情（Bearer）。
 * 信件保留 30 分钟，仅接收不发送，正文仅纯文本。
 */
final class Clawdemail
{
    private const CHANNEL = 'clawdemail';
    private const BASE = 'https://api.clawdemail.com';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
    ];

    /** 设置 clawdemail 请求的通用请求头（可选附带 Bearer token）。 */
    private static function authHeaders(?string $token): array
    {
        $h = self::HEADERS;
        if ($token !== null && $token !== '') {
            $h['Authorization'] = 'Bearer ' . $token;
        }
        return $h;
    }

    /** 创建 clawdemail.com 临时邮箱：POST /register（body {"name":""}）返回 email 与 token。 */
    public static function generate(): EmailInfo
    {
        $resp = HttpClient::post(
            self::BASE . '/register',
            self::authHeaders(null) + ['Content-Type' => 'application/json'],
            json: ['name' => ''],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('clawdemail: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $email = trim((string) ($data['email'] ?? ''));
        $token = trim((string) ($data['token'] ?? ''));
        if ($email === '' || $token === '' || !str_contains($email, '@')) {
            throw new \RuntimeException('clawdemail: 创建邮箱响应缺少必要字段');
        }
        return new EmailInfo(self::CHANNEL, $email, $token);
    }

    /**
     * 获取 clawdemail.com 邮件列表。
     * 流程：GET /inbox?limit=50 取列表，对每个元素按 id 逐封 GET /email/{id} 合并详情；
     * 详情失败时以列表摘要归一。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        $resp = HttpClient::get(
            self::BASE . '/inbox',
            self::authHeaders($token),
            query: ['limit' => '50'],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('clawdemail: 获取邮件列表失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        if (($data['success'] ?? null) !== true) {
            throw new \RuntimeException('clawdemail: 读取收件箱失败: ' . (string) ($data['error'] ?? ''));
        }
        $emails = $data['emails'] ?? null;
        if (!is_array($emails)) {
            return [];
        }

        $out = [];
        foreach ($emails as $m) {
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

    /**
     * 获取单封详情（GET /email/{id}，Bearer token），
     * 响应含 email 嵌套对象（from_addr/body_text/received_at）时提升嵌套对象。
     */
    private static function getDetail(?string $token, string $id): ?array
    {
        $resp = HttpClient::get(
            self::BASE . '/email/' . rawurlencode($id),
            self::authHeaders($token),
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return null;
        }
        $detail = HttpClient::json($resp);
        if ($detail === []) {
            return null;
        }
        $nested = $detail['email'] ?? null;
        return is_array($nested) ? $nested : $detail;
    }
}