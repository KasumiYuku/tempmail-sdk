<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Nullmail 渠道实现（nullmail.cc / maildock.store）
 *
 * 无认证 REST：POST /api/emails（空 JSON body）建箱，响应
 * {"address":"...@maildock.store","expiry":"..."}；
 * 读信 GET /api/emails/{address}（URL 编码），响应 {"expiry":"...","emails":[...]}，
 * 列表项只有 id/sender/subject/delivered，正文须逐封二拉 GET /api/emails/{addr}/body/{id}
 * （响应 {"body":...}）。
 */
final class Nullmail
{
    private const CHANNEL = 'nullmail';
    private const BASE = 'https://www.nullmail.cc';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
        'Content-Type' => 'application/json',
        'Origin' => self::BASE,
        'Referer' => self::BASE . '/',
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
        $resp = HttpClient::post(self::BASE . '/api/emails', self::HEADERS, json: []);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('nullmail: 建箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $addr = trim((string) ($data['address'] ?? ''));
        if ($addr === '') {
            throw new \RuntimeException('nullmail: 建箱响应缺少 address 字段');
        }
        return new EmailInfo(self::CHANNEL, $addr, $addr);
    }

    /** 单封正文二拉：GET /api/emails/{addr}/body/{id}，响应 {"body":"<完整纯文本正文>"} */
    private static function fetchBody(string $addr, mixed $id): string
    {
        $resp = HttpClient::get(
            self::BASE . '/api/emails/' . rawurlencode($addr) . '/body/' . rawurlencode((string) $id),
            self::getHeaders(),
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return '';
        }
        $data = HttpClient::json($resp);
        return (string) ($data['body'] ?? '');
    }

    /**
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $email = trim($email);
        if ($email === '') {
            throw new \InvalidArgumentException('nullmail: 邮箱地址为空');
        }
        $resp = HttpClient::get(self::BASE . '/api/emails/' . rawurlencode($email), self::getHeaders());
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('nullmail: 读取收件箱失败 http ' . $resp->getStatusCode());
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
            $id = $m['id'] ?? null;
            $raw = [
                'id' => is_scalar($id) && $id !== null ? (string) $id : '',
                'from' => $m['sender'] ?? '',
                'to' => $email,
                'subject' => $m['subject'] ?? '',
                // normalizeDate 候选键不含 delivered，显式映射为 date 后归一化
                'date' => $m['delivered'] ?? '',
            ];
            // 列表只有 id/sender/subject/delivered，正文逐封二拉 body 端点，失败降级留空不阻断列表
            if ($id !== null && is_scalar($id)) {
                $text = self::fetchBody($email, $id);
                if ($text !== '') {
                    $raw['text'] = $text;
                }
            }
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }
}