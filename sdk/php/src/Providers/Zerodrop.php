<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Zerodrop 渠道实现（zerodrop.dev）
 *
 * 无认证 REST：建箱无需请求，本地生成 "sdk"+8 位随机名，地址为 <名>@zerodrop-sandbox.online；
 * 读信 GET /api/inbox/{name}?source=sdk，响应形如 {"emails":[...],"count":N}。
 * 平台邮件对象只有 id/from/to/subject/receivedAt/raw/otp/magicLink：正文仅存在于
 * raw（完整 MIME 原文，头部与 body 以空行分隔），无 text/html 字段，
 * 故须从 raw 中剥离头部提取纯文本 body 填入 text。
 */
final class Zerodrop
{
    private const CHANNEL = 'zerodrop';
    private const BASE = 'https://zerodrop.dev';
    private const DOMAIN = 'zerodrop-sandbox.online';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
    ];

    /** 多候选字段取值：返回第一个存在且非空的标量字符串值 */
    private static function pickStr(array $m, array $keys): string
    {
        foreach ($keys as $key) {
            $v = $m[$key] ?? null;
            if (is_scalar($v) && $v !== null && trim((string) $v) !== '') {
                return trim((string) $v);
            }
        }
        return '';
    }

    /** 生成 "sdk"+8 位随机本地名 */
    private static function localName(): string
    {
        $chars = 'abcdefghijklmnopqrstuvwxyz0123456789';
        $suffix = '';
        for ($i = 0; $i < 8; $i++) {
            $suffix .= $chars[random_int(0, strlen($chars) - 1)];
        }
        return 'sdk' . $suffix;
    }

    /**
     * 从 raw（完整 MIME 原文）提取纯文本正文：
     * 定位首个空行（RFC 5322 头部/正文分隔，\r\n\r\n 或 \n\n），其后部分即 body。
     */
    private static function rawBody(string $raw): string
    {
        $idx = strpos($raw, "\r\n\r\n");
        if ($idx !== false) {
            return substr($raw, $idx + 4);
        }
        $idx = strpos($raw, "\n\n");
        if ($idx !== false) {
            return substr($raw, $idx + 2);
        }
        return '';
    }

    public static function generate(): EmailInfo
    {
        // 建箱无需请求，本地生成随机名，token 复用完整地址以便收件箱回查
        $email = self::localName() . '@' . self::DOMAIN;
        return new EmailInfo(self::CHANNEL, $email, $email);
    }

    /**
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $email = trim($email);
        $parts = explode('@', $email, 2);
        if (count($parts) !== 2 || $parts[1] !== self::DOMAIN) {
            throw new \InvalidArgumentException('zerodrop: 非 zerodrop 域邮箱地址');
        }

        $resp = HttpClient::get(
            self::BASE . '/api/inbox/' . rawurlencode($parts[0]),
            self::HEADERS,
            query: ['source' => 'sdk'],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('zerodrop: 读取收件箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $emails = $data['emails'] ?? null;
        if (!is_array($emails)) {
            return [];
        }

        $out = [];
        foreach ($emails as $m) {
            if (!is_array($m)) {
                continue;
            }
            $raw = [
                'id' => self::pickStr($m, ['id']),
                'from' => self::pickStr($m, ['from']),
                'to' => self::pickStr($m, ['to']) !== '' ? self::pickStr($m, ['to']) : $email,
                'subject' => self::pickStr($m, ['subject']),
                'date' => self::pickStr($m, ['receivedAt']),
            ];
            // 正文仅存在于 raw（完整 MIME 原文），无 text/html 字段：提取纯文本 body 作 text
            $rawBody = self::rawBody(self::pickStr($m, ['raw']));
            if ($rawBody !== '') {
                $raw['text'] = $rawBody;
            }
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }
}