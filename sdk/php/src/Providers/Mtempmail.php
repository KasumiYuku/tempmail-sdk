<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Mtempmail 渠道实现（mtempmail.com，公共 key 认证）
 *
 * 建箱: POST /api/emails/{apiKey}（body {}）→
 *   {"status":true,"data":{"email":"xxx@domain","domain":"..","fingerprint":"..",
 *    "expire_at":"..","created_at":"..","id":213000,"email_token":"..."}}
 * 读信: GET /api/messages/{apiKey}/{email} →
 *   {"status":true,"mailbox":"..","email_token":"..","messages":[]}
 *   消息列表元素为 mailgun 入站 webhook 风格：
 *   {"to":[{..}],"body":[{content_type:"text/html",value:".."}],
 *    "created_at":"..","id":123,"from":[{"full":"Sender <a@b.com>"}],"subject":".."}。
 */
final class Mtempmail
{
    private const CHANNEL = 'mtempmail';
    private const BASE = 'https://mtempmail.com';

    /** 公共固定 API key（mtempmail.com 官方提供） */
    private const PUBLIC_KEY = 'pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw';

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

    /** 清洗主题前导分隔符（后台可能以 "• " 开头拼接微件） */
    private static function subject(mixed $s): string
    {
        if (!is_scalar($s) || $s === null) {
            return '';
        }
        return preg_replace('/^[•·]+\s*/u', '', trim((string) $s)) ?? trim((string) $s);
    }

    /** 拼接正文纯文本（body[].value 按序） */
    private static function bodyText(array $parts): string
    {
        $lines = [];
        foreach ($parts as $p) {
            if (is_array($p) && isset($p['value']) && is_scalar($p['value'])) {
                $lines[] = (string) $p['value'];
            }
        }
        return implode("\n", $lines);
    }

    /** 提取首个 text/html 段 */
    private static function bodyHtml(array $parts): string
    {
        foreach ($parts as $p) {
            if (is_array($p) && ($p['content_type'] ?? '') === 'text/html' && is_scalar($p['value'] ?? null)) {
                return (string) $p['value'];
            }
        }
        return '';
    }

    public static function generate(): EmailInfo
    {
        $resp = HttpClient::post(
            self::BASE . '/api/emails/' . self::PUBLIC_KEY,
            self::HEADERS,
            json: [],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('mtempmail: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $inner = $data['data'] ?? null;
        if (empty($data['status']) || !is_array($inner)) {
            throw new \RuntimeException('mtempmail: 创建响应缺少 data');
        }
        $email = trim((string) ($inner['email'] ?? ''));
        if ($email === '') {
            throw new \RuntimeException('mtempmail: 创建响应缺少邮箱');
        }
        return new EmailInfo(
            self::CHANNEL,
            $email,
            (string) ($inner['email_token'] ?? ''),
            createdAt: $inner['created_at'] ?? null,
        );
    }

    /**
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $email = trim($email);
        if ($email === '') {
            throw new \InvalidArgumentException('mtempmail: 邮箱为空');
        }
        if (trim($token ?? '') === '') {
            throw new \InvalidArgumentException('mtempmail: token 为空');
        }
        $resp = HttpClient::get(
            self::BASE . '/api/messages/' . self::PUBLIC_KEY . '/' . rawurlencode($email),
            self::getHeaders(),
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('mtempmail: 读取收件箱失败 http ' . $resp->getStatusCode());
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
            $bodyParts = is_array($m['body'] ?? null) ? $m['body'] : [];
            // from 为 [{"full":"Sender <a@b.com>"}] 数组时拆出 full 字段
            $from = '';
            if (is_array($m['from'] ?? null)) {
                foreach ($m['from'] as $f) {
                    if (is_array($f) && !empty($f['full']) && is_scalar($f['full'])) {
                        $from = (string) $f['full'];
                        break;
                    }
                }
            }
            $raw = [
                'id' => $m['id'] ?? '',
                'from' => $from,
                'to' => $email,
                'subject' => self::subject($m['subject'] ?? ''),
                'text' => self::bodyText($bodyParts),
                'html' => self::bodyHtml($bodyParts),
                'date' => $m['created_at'] ?? '',
            ];
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }
}