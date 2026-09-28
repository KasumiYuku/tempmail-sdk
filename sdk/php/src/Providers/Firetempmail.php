<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Firetempmail 渠道实现（firetempmail.com）
 *
 * 无认证 REST：建箱无需请求，本地生成 随机词+0-999@<域>（域池与官网一致：
 * offrework.click / service-today.click / jobsdeforyou.sa.com）；
 * 读信 GET https://mail.firetempmail.com/mail/get?address=<邮箱 URL 编码>，
 * 必须携带 Header Origin: https://firetempmail.com（否则 403 'Origin not allowed'）。
 * 邮件字段以 sender/subject/date/recipient/suffix + content-html/content-text/content-plain
 * 多候选归一化。
 */
final class Firetempmail
{
    private const CHANNEL = 'firetempmail';
    private const BASE = 'https://mail.firetempmail.com';
    private const ORIGIN = 'https://firetempmail.com';

    /** @var string[] 官网 JS 中的完整平台域池，顺序与官网一致 */
    private const DOMAINS = ['offrework.click', 'service-today.click', 'jobsdeforyou.sa.com'];

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
        'Origin' => self::ORIGIN,
        'Referer' => self::ORIGIN . '/',
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

    /** 随机小写单词（3-6 位字母，模拟官网 faker 词的观感） */
    private static function localWord(): string
    {
        $chars = 'abcdefghijklmnopqrstuvwxyz';
        $word = '';
        $n = random_int(3, 6);
        for ($i = 0; $i < $n; $i++) {
            $word .= $chars[random_int(0, strlen($chars) - 1)];
        }
        return $word;
    }

    public static function generate(): EmailInfo
    {
        // 建箱无需请求，本地生成 随机词+0-999@域名 的形式，token 复用完整地址
        $domain = self::DOMAINS[random_int(0, count(self::DOMAINS) - 1)];
        $email = self::localWord() . (string) random_int(0, 999) . '@' . $domain;
        return new EmailInfo(self::CHANNEL, $email, $email);
    }

    /**
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $email = trim($email);
        if ($email === '') {
            throw new \InvalidArgumentException('firetempmail: 邮箱地址为空');
        }
        $resp = HttpClient::get(
            self::BASE . '/mail/get',
            self::HEADERS,
            query: ['address' => $email],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('firetempmail: 读取收件箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $status = (string) ($data['status'] ?? '');
        if ($status !== '' && $status !== 'ok') {
            throw new \RuntimeException('firetempmail: 读取收件箱失败 ' . (string) ($data['msg'] ?? $status));
        }
        $mails = $data['mails'] ?? null;
        if (!is_array($mails)) {
            return [];
        }

        $out = [];
        foreach ($mails as $m) {
            if (!is_array($m)) {
                continue;
            }
            $raw = [
                'id' => self::pickStr($m, ['id']),
                // 官网 JSON 无统一 to 字段，收件人固定为当前邮箱
                'to' => $email,
                'from' => self::pickStr($m, ['sender', 'from', 'from_address']),
                'subject' => self::pickStr($m, ['subject', 'title']),
                'html' => self::pickStr($m, ['content-html', 'html']),
                'text' => self::pickStr($m, ['content-text', 'content-plain', 'text']),
                'date' => self::pickStr($m, ['date', 'received_at', 'created_at']),
            ];
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }
}