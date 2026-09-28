<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Mailticking 渠道实现（www.mailticking.com，旧域名 temporary-mail.net 的更名站）
 *
 * 实测协议（三类请求均以 application/json 交流，站点在 Cloudflare 后）：
 *   1. 建箱 POST /get-mailbox body {"types":["4"]}（4=独立域名，排除 Gmail 别名）
 *      响应 {"success":true,"email":"xxx@domain","activate_token":"..."}
 *   2. 激活 POST /activate-email body {"email":..,"source":"api","activate_token":..}
 *      响应 {"success":true}
 *   3. 列信 POST /get-emails?lang=en body {"email":..,"code":..}
 *      空箱实测响应 {"emails":[],"success":true}；空闲超时/被改绑后返回
 *      {"success":false,"needNewEmail":true,...}，此时返回错误提示换箱。
 *
 * 读信正文无公开端点：GetEmails 只具备列表能力，列表字段名采用多候选提取
 * 策略，命中多少映射多少（Normalize 既有候选字段负责兜底）。
 */
final class Mailticking
{
    private const CHANNEL = 'mailticking';
    private const BASE = 'https://www.mailticking.com';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
        'Accept-Language' => 'en-US,en;q=0.9',
        'Content-Type' => 'application/json',
        'Referer' => self::BASE . '/',
        'Origin' => self::BASE,
    ];

    /**
     * 执行 JSON POST 请求并解析响应；响应非 2xx 或 success=false 时抛错。
     *
     * @param array<mixed> $payload
     * @return array<mixed>
     */
    private static function doPost(string $path, array $payload): array
    {
        $resp = HttpClient::post(self::BASE . $path, self::HEADERS, json: $payload);
        $data = HttpClient::json($resp);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            $msg = (string) ($data['error'] ?? $data['message'] ?? (string) $resp->getBody());
            throw new \RuntimeException('mailticking: http ' . $resp->getStatusCode() . ': ' . $msg);
        }
        if (($data['success'] ?? null) !== true) {
            $msg = (string) ($data['error'] ?? $data['message'] ?? 'unknown error');
            throw new \RuntimeException('mailticking: 请求失败: ' . $msg);
        }
        return $data;
    }

    /**
     * 创建 mailticking 邮箱账号。
     * 请求 type=4（独立域名）固定取独立域名邮箱，建箱后立即激活会话；
     * token 必须携带 activate_token（列信协议依赖激活会话），不能为空。
     */
    public static function generate(): EmailInfo
    {
        $box = self::doPost('/get-mailbox', ['types' => ['4']]);
        $token = trim((string) ($box['code'] ?? ''));
        if ($token === '') {
            $token = trim((string) ($box['email'] ?? ''));
        }
        $email = trim((string) ($box['email'] ?? ''));
        if ($token === '' || $email === '') {
            throw new \RuntimeException('mailticking: get-mailbox 返回空 email/activate_token');
        }

        // 激活邮箱，使后续列信请求与服务器记录的最新会话一致
        self::doPost('/activate-email', [
            'email' => $email,
            'source' => 'api',
            'activate_token' => $token,
        ]);

        return new EmailInfo(self::CHANNEL, $email, $token);
    }

    /**
     * 获取 mailticking 邮箱的邮件列表。
     * 空箱返回空列表不报错；需要换箱的响应（needNewEmail）返回语义错误。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        $tk = trim($token ?? '');
        if ($addr === '') {
            throw new \InvalidArgumentException('mailticking: 邮箱地址为空');
        }
        if ($tk === '') {
            throw new \InvalidArgumentException('mailticking: activate code 为空');
        }
        $resp = HttpClient::post(self::BASE . '/get-emails?lang=en', self::HEADERS, json: [
            'email' => $addr,
            'code' => $tk,
        ]);
        $data = HttpClient::json($resp);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            $msg = (string) ($data['error'] ?? $data['message'] ?? (string) $resp->getBody());
            throw new \RuntimeException('mailticking: http ' . $resp->getStatusCode() . ': ' . $msg);
        }
        if (($data['success'] ?? null) !== true) {
            if (($data['needNewEmail'] ?? null) === true) {
                throw new \RuntimeException('mailticking: 邮箱已过期，请重新建箱');
            }
            throw new \RuntimeException('mailticking: get-emails 失败');
        }
        $emails = $data['emails'] ?? null;
        if (!is_array($emails)) {
            return [];
        }

        $out = [];
        foreach ($emails as $raw) {
            if (!is_array($raw)) {
                continue;
            }
            $flat = self::migratedFields($raw);
            $out[] = Normalize::email($flat, $addr);
        }
        return $out;
    }

    /**
     * 尽量映射列表字段到统一字段名。
     * 官网首页表格只有 SENDER/SUBJECT/TIME 三列，字段全名缺少可观测证据，
     * 因此对常见字段做多候选提取；未命中的字段留给 Normalize 自己处理。
     *
     * @param array<mixed> $raw
     * @return array<mixed>
     */
    private static function migratedFields(array $raw): array
    {
        $m = $raw;
        foreach ([
            'mail_from', 'from_mail', 'from_email', 'sender_address', 'from_address',
            'send_addr', 'mail_addr', 'address_from', 'ho_from', 'fromname', 'fromS',
        ] as $key) {
            $v = $m[$key] ?? null;
            if (is_scalar($v) && $v !== null && trim((string) $v) !== '' && !isset($m['from'])) {
                $m['from'] = (string) $v;
                break;
            }
        }
        if (!isset($m['sender']) && isset($m['from']) && trim((string) $m['from']) !== '') {
            $m['sender'] = (string) $m['from'];
        }
        if (!isset($m['id']) && isset($m['mail_id'])) {
            $m['id'] = $m['mail_id'];
        }
        if (!isset($m['date']) && isset($m['received_at'])) {
            $m['date'] = $m['received_at'];
        }
        return $m;
    }
}