<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Nukemail 渠道实现（nukemail.app）
 *
 * 完整接入契约：
 *   挑战 GET /api/pow/challenge?difficulty=4 → 200 {"id","challenge","difficulty"}；
 *     求 nonce 使 SHA-256(challenge+nonce) 十六进制前 4 位为 0
 *     （前端 solvePow 逐 nonce 自 0 递增）。
 *   建箱 POST /api/inbox/create body {"address","domain","pow_id","pow_nonce"}
 *     → 200 {"token":"NUKE-xxxxxxxx","email":"名@域名"}，并
 *     Set-Cookie: nukemail_token=<token>（Secure; HttpOnly; SameSite=lax, 72h）。
 *   读信 GET /api/inbox（Cookie: nukemail_token=<token>）→ 200
 *     {"token","state","addresses":[...],"messages":[...],"is_premium"...}；
 *     messages 元素字段为 sender、sender_name、subject、body_html、body_text、
 *     received_at、read。
 *   会话恢复 POST /api/inbox/resume {"accessCode":<token>} 重设 Cookie，仅作兜底。
 * 会话隔离：nukemail_token 由生成结果持久化为 token，读信时以显式 Cookie 头携带。
 */
final class Nukemail
{
    private const CHANNEL = 'nukemail';
    private const BASE = 'https://nukemail.app';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
    ];

    /** 求解 PoW：返回使 SHA-256(challenge+nonce) 十六进制前 difficulty 位为 0 的最小 nonce。 */
    private static function solvePow(string $challenge, int $difficulty): int
    {
        $prefix = str_repeat('0', $difficulty);
        $nonce = 0;
        while (true) {
            if (str_starts_with(hash('sha256', $challenge . $nonce), $prefix)) {
                return $nonce;
            }
            $nonce++;
        }
    }

    /** 生成本地随机名（与前端 generateRandomName 等价形态）。 */
    private static function randomAddress(): string
    {
        $chars = 'abcdefghijklmnopqrstuvwxyz0123456789';
        $n = strlen($chars);
        $s = '';
        for ($i = 0; $i < 10; $i++) {
            $s .= $chars[random_int(0, $n - 1)];
        }
        return 'nuke' . $s;
    }

    /** 取第一个非 premium 的活跃域名。 */
    private static function defaultDomain(): string
    {
        $resp = HttpClient::get(self::BASE . '/api/domains', self::HEADERS);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('nukemail generate: domains http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $domains = $data['domains'] ?? null;
        if (!is_array($domains)) {
            throw new \RuntimeException('nukemail generate: domains 响应非数组');
        }
        foreach ($domains as $d) {
            if (!is_array($d)) {
                continue;
            }
            $domain = trim((string) ($d['domain'] ?? ''));
            if ($domain !== '' && ($d['is_premium_only'] ?? null) !== true) {
                return $domain;
            }
        }
        throw new \RuntimeException('nukemail generate: 无可用非 premium 域名');
    }

    /** 创建临时邮箱（PoW 建箱）；token 为平台返回的 NUKE-<随机> 访问码。 */
    public static function generate(): EmailInfo
    {
        // 1) 取 PoW 挑战
        $resp = HttpClient::get(self::BASE . '/api/pow/challenge', self::HEADERS, query: ['difficulty' => '4']);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('nukemail generate: challenge http ' . $resp->getStatusCode());
        }
        $ch = HttpClient::json($resp);
        $chId = trim((string) ($ch['id'] ?? ''));
        $challenge = trim((string) ($ch['challenge'] ?? ''));
        if ($chId === '' || $challenge === '') {
            throw new \RuntimeException('nukemail generate: challenge 响应缺少 id/challenge');
        }
        $difficulty = (int) ($ch['difficulty'] ?? 0);
        if ($difficulty <= 0) {
            $difficulty = 4;
        }

        // 2) 本地求 PoW 解（SHA-256 前缀 4 零）
        $nonce = self::solvePow($challenge, $difficulty);

        // 3) 取域名并建箱
        $dom = self::defaultDomain();
        $body = (string) json_encode([
            'address' => self::randomAddress(),
            'domain' => $dom,
            'pow_id' => $chId,
            'pow_nonce' => (string) $nonce,
        ]);
        $resp2 = HttpClient::post(
            self::BASE . '/api/inbox/create',
            ['Content-Type' => 'application/json'] + self::HEADERS,
            body: $body,
        );
        if ($resp2->getStatusCode() < 200 || $resp2->getStatusCode() >= 300) {
            throw new \RuntimeException('nukemail generate: create http ' . $resp2->getStatusCode());
        }
        $data = HttpClient::json($resp2);
        $token = trim((string) ($data['token'] ?? ''));
        $email = trim((string) ($data['email'] ?? ''));
        if ($token === '' || $email === '') {
            throw new \RuntimeException('nukemail generate: create 响应缺少 token/email');
        }
        return new EmailInfo(self::CHANNEL, $email, $token);
    }

    /**
     * 读取收件箱。
     * 主通道 GET /api/inbox 带 Cookie: nukemail_token=<token>；
     * 会话过期时经 POST /api/inbox/resume 显式重设会话后重试一次。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        $tk = trim($token ?? '');
        if ($tk === '') {
            throw new \InvalidArgumentException('nukemail: token 不能为空');
        }
        $cookie = 'nukemail_token=' . $tk;

        $fetch = static function () use ($cookie): array {
            $resp = HttpClient::get(self::BASE . '/api/inbox', self::HEADERS + ['Cookie' => $cookie]);
            if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
                throw new \RuntimeException('nukemail 读信: http ' . $resp->getStatusCode());
            }
            return HttpClient::json($resp);
        };

        $data = $fetch();
        if (($data['state'] ?? '') === '' || ($data['state'] ?? '') === 'expired') {
            // 会话可能已过期：经 resume 重设会话后重试
            $resumeBody = (string) json_encode(['accessCode' => $tk]);
            HttpClient::post(
                self::BASE . '/api/inbox/resume',
                ['Content-Type' => 'application/json', 'User-Agent' => self::HEADERS['User-Agent']],
                body: $resumeBody,
            );
            $data = $fetch();
        }

        $messages = $data['messages'] ?? null;
        if (!is_array($messages)) {
            return [];
        }
        $out = [];
        foreach ($messages as $m) {
            if (!is_array($m)) {
                continue;
            }
            $flat = $m;
            $flat['to'] = $addr;
            // 平台消息字段为 body_html/body_text，补齐 text/html 候选（若原字段缺失）
            if (!array_key_exists('text', $flat)) {
                $flat['text'] = $m['body_text'] ?? null;
            }
            if (!array_key_exists('html', $flat)) {
                $flat['html'] = $m['body_html'] ?? null;
            }
            $flat['date'] = $m['received_at'] ?? null;
            $flat['read'] = $m['read'] ?? null;
            // sender 是小写发件人地址，sender_name 是展示名
            $flat['sender_email'] = $m['sender'] ?? null;
            $out[] = Normalize::email($flat, $addr);
        }
        return $out;
    }
}