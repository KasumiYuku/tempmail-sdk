<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Shadowmail 渠道实现（shadowmail.win）
 *
 * 完整接入契约：
 *   注册 POST /api/register {"email":"<随机前缀>@gmail.com","password":"Abcd1234!"}
 *     → 200 {"message":"Successfully Registered"}；
 *   登录 POST /api/login → 200 {"message":"Successfull Login"}
 *     并 Set-Cookie: sessionId=<uuid>（HttpOnly; Secure; Max-Age 3600）；
 *   建箱 POST /api/new-address → 200 {...,"address":"<id>@shadowmail.win","id":<id>}；
 *   读信 POST /api/get-emails {"address":"<地址>"} → 200 {"message":"Emails read",
 *     "mails":[...]}；mails 元素字段 id/address_id/sender/subject/body/created_at。
 * 会话隔离：全域使用显式 Cookie 头（sessionId=<uuid>），凭据串 token 持久化
 *   注册邮箱/密码/会话 id，便于会话过期后自动重生。
 */
final class Shadowmail
{
    private const CHANNEL = 'shadowmail';
    private const BASE = 'https://shadowmail.win';

    /** 固定注册密码（平台无自选密码入口，注册即固定） */
    private const PW = 'Abcd1234!';

    /** 平台唯一收信域 */
    private const DOMAIN = 'shadowmail.win';

    /** 本渠道凭据串前缀 */
    private const TOKEN_PREFIX = 'shadowmail|';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Content-Type' => 'application/json',
        'Accept' => 'application/json',
    ];

    /** 生成随机注册邮箱前缀（sdk+8 位小写字母）。 */
    private static function randomAccount(): string
    {
        $chars = 'abcdefghijklmnopqrstuvwxyz';
        $n = strlen($chars);
        $s = '';
        for ($i = 0; $i < 8; $i++) {
            $s .= $chars[random_int(0, $n - 1)];
        }
        return 'sdk' . $s;
    }

    /**
     * 携带显式 Cookie 的 JSON POST 请求，返回 [body数组, status, setCookies]。
     *
     * @param array<mixed> $body
     * @return array{0:array<mixed>,1:int,2:array<string>}
     */
    private static function doPost(string $path, array $body, string $cookie): array
    {
        $headers = self::HEADERS;
        if ($cookie !== '') {
            $headers['Cookie'] = $cookie;
        }
        $resp = HttpClient::post(self::BASE . $path, $headers, json: $body);
        return [HttpClient::json($resp), $resp->getStatusCode(), $resp->getHeader('Set-Cookie')];
    }

    /** 从 Set-Cookie 中提取 sessionId 值（纯 uuid，不含键名）。 */
    private static function sessionFrom(array $cookies): string
    {
        foreach ($cookies as $line) {
            $kv = trim(explode(';', $line, 2)[0]);
            if (str_starts_with($kv, 'sessionId=')) {
                return substr($kv, strlen('sessionId='));
            }
        }
        return '';
    }

    /** 注册或登录（POST /api/register、/api/login），返回会话 sessionId。 */
    private static function registerLogin(string $account, string $password, bool $isLogin): string
    {
        $path = $isLogin ? '/api/login' : '/api/register';
        $body = ['email' => $account, 'password' => $password];
        [$data, $status, $cookies] = self::doPost($path, $body, '');
        if ($status < 200 || $status >= 300) {
            throw new \RuntimeException('shadowmail ' . $path . ': http ' . $status);
        }
        $msg = (string) ($data['message'] ?? '');
        if ($isLogin) {
            if ($msg !== 'Successfull Login') {
                throw new \RuntimeException('shadowmail login: ' . $msg);
            }
        } else {
            // 重复注册（幂等）：消息为 Email already in use 时视为账号已存在，继续走登录
            if ($msg !== 'Successfully Registered' && $msg !== 'Email already in use') {
                throw new \RuntimeException('shadowmail register: ' . $msg);
            }
        }
        $session = self::sessionFrom($cookies);
        if ($isLogin && $session === '') {
            throw new \RuntimeException('shadowmail login: 未下发 sessionId Cookie');
        }
        return $session;
    }

    /**
     * 注册账号并创建临时邮箱地址。
     * token 凭据串格式："shadowmail|<account>|<password>|<sessionId>"。
     */
    public static function generate(): EmailInfo
    {
        $account = self::randomAccount() . '@gmail.com';

        // 1) 注册（幂等：已存在同名账号则跳过）
        self::registerLogin($account, self::PW, false);
        // 2) 登录取得 sessionId
        $session = self::registerLogin($account, self::PW, true);
        // 3) 创建地址（每账号 12 槽）：SDK 层显式 Cookie 头传 sessionId
        [$data, $status] = self::doPost('/api/new-address', [], 'sessionId=' . $session);
        if ($status < 200 || $status >= 300) {
            throw new \RuntimeException('shadowmail new-address: http ' . $status);
        }
        $address = trim((string) ($data['address'] ?? ''));
        if ($address === '' || !str_ends_with($address, '@' . self::DOMAIN)) {
            throw new \RuntimeException('shadowmail new-address: 响应缺少有效地址');
        }

        // Token 持久化：account|password|sessionId（sessionId 为 uuid，无分隔符冲突）
        $token = self::TOKEN_PREFIX . implode('|', [$account, self::PW, $session]);
        return new EmailInfo(self::CHANNEL, strtolower($address), $token);
    }

    /**
     * 解析凭据串为 account/password/sessionId 三元组。
     *
     * @return array{0:string,1:string,2:string}
     */
    private static function parseToken(string $token): array
    {
        if (!str_starts_with($token, self::TOKEN_PREFIX)) {
            throw new \InvalidArgumentException('shadowmail: token 格式错误');
        }
        $parts = explode('|', substr($token, strlen(self::TOKEN_PREFIX)));
        if (count($parts) !== 3) {
            throw new \InvalidArgumentException('shadowmail: token 字段缺失');
        }
        [$account, $password, $session] = $parts;
        if ($account === '' || $password === '' || $session === '') {
            throw new \InvalidArgumentException('shadowmail: token 凭据字段为空');
        }
        return [$account, $password, $session];
    }

    /**
     * 读取收件箱；会话失效时自动以凭据内 account/password 重新登录换新 sessionId。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        [$account, $password, $session] = self::parseToken(trim($token ?? ''));
        $addr = trim($email);

        $doFetch = static function () use ($addr, &$session): array {
            return self::doPost('/api/get-emails', ['address' => $addr], 'sessionId=' . $session);
        };

        [$data, $status] = $doFetch();
        // sessionId 最长 1 小时（Max-Age 3600），过期后重登录重试一次
        if ($status === 401 || $status === 404) {
            $newSession = self::registerLogin($account, $password, true);
            if ($newSession !== '') {
                $session = $newSession;
                [$data, $status] = $doFetch();
            }
        }
        if ($status < 200 || $status >= 300) {
            throw new \RuntimeException('shadowmail get-emails: http ' . $status);
        }
        $msg = (string) ($data['message'] ?? '');
        if ($msg !== 'Emails read') {
            throw new \RuntimeException('shadowmail get-emails: ' . $msg);
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
            $flat = $m;
            $flat['from'] = $m['sender'] ?? '';
            $flat['to'] = $addr;
            $flat['date'] = $m['created_at'] ?? null;
            // 平台无 text/html 区分，body 为正文（默认按纯文本处理，普通化可按需互转）
            $flat['text'] = $m['body'] ?? '';
            $out[] = Normalize::email($flat, $addr);
        }
        return $out;
    }
}