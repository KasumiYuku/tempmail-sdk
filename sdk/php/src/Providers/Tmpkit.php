<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * Tmpkit 渠道实现（tmpkit.com，Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）
 *
 * 调研实证结论（与 Go 端 tmpkit.go 一致）：
 *   - POST /api/rpc/tempmail/initSession，body 为 tRPC 包裹 {"json":{}}，
 *     免凭据。响应 {"json":{"session":{"sessionId","email",...}}}，同时
 *     Set-Cookie: tempmail_session=<sessionId>; Max-Age=3600（与 sessionId
 *     同值）。
 *   - POST /api/rpc/tempmail/getEmails，body {"json":{"offset":0,
 *     "limit":20}}，需带 tempmail_session Cookie。列表元素键为
 *     mailId/from/subject/excerpt/date/timestamp/hasAttach/isRead（无 id）。
 *   - POST /api/rpc/tempmail/getEmailDetail，body {"json":{"mailId":<数字>}}
 *     （mailId 为数字，字符串会 zod 400）。响应为单封详情对象。
 *
 * 会话粘性：token 保存 sessionId（tempMailSession=<sid>），每次读信以
 *   显式 Cookie 请求头携带，防全局会话被并行会话覆盖后串箱。
 */
final class Tmpkit
{
    private const CHANNEL = 'tmpkit';
    private const BASE_URL = 'https://tmpkit.com';
    private const RPC_PREFIX = self::BASE_URL . '/api/rpc/tempmail';

    private const USER_AGENT = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 '
        . '(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36';

    /** tRPC 调用请求头（Content-Type JSON、Accept 通配、同站 Origin/Referer） */
    private static function rpcHeaders(string $cookie = ''): array
    {
        $h = [
            'User-Agent' => self::USER_AGENT,
            'Accept' => '*/*',
            'Content-Type' => 'application/json',
            'Origin' => self::BASE_URL,
            'Referer' => self::BASE_URL . '/en',
        ];
        if ($cookie !== '') {
            $h['Cookie'] = 'tempmail_session=' . $cookie;
        }
        return $h;
    }

    /**
     * 对 tmpkit 发起 rpc 调用（tRPC 包裹 {"json":<reqBody>}），
     * 响应外层 {"json":{...}} 解析为数组返回。
     *
     * @param array<mixed> $reqBody 过程参数
     * @return array<mixed>
     */
    private static function rpc(string $procedure, array $reqBody, string $cookie = ''): array
    {
        $resp = HttpClient::post(
            self::RPC_PREFIX . '/' . $procedure,
            self::rpcHeaders($cookie),
            json: ['json' => $reqBody],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tmpkit: ' . $procedure . ' 失败 http ' . $resp->getStatusCode());
        }
        $outer = HttpClient::json($resp);
        $payload = $outer['json'] ?? null;
        if (!is_array($payload)) {
            throw new \RuntimeException('tmpkit: ' . $procedure . ' 响应缺 json 载荷');
        }
        return $payload;
    }

    /** 从 map 容错取字符串（与 Go 端 tmpkitMapGet 语义一致） */
    private static function mapGet(array $m, string $key): string
    {
        $v = $m[$key] ?? null;
        if ($v === null) {
            return '';
        }
        return trim((string) $v);
    }

    /**
     * 创建 tmpkit.com 临时邮箱
     * 调 initSession（tRPC 包裹 {"json":{}}），sessionId 与邮箱地址同返。
     */
    public static function generate(): EmailInfo
    {
        $data = self::rpc('initSession', []);
        $sess = $data['session'] ?? null;
        if (!is_array($sess)) {
            throw new \RuntimeException('tmpkit: 创建会话响应缺 session 字段');
        }
        $email = self::mapGet($sess, 'email');
        $sessionId = self::mapGet($sess, 'sessionId');
        if ($email === '' || $sessionId === '' || strpos($email, '@') === false) {
            throw new \RuntimeException('tmpkit: 创建会话响应缺少必要字段（email/sessionId）');
        }
        return new EmailInfo(self::CHANNEL, $email, 'tempMailSession=' . $sessionId);
    }

    /**
     * 获取 tmpkit.com 邮件列表
     * getEmails（offset 0 / limit 20）取摘要，逐封 getEmailDetail 拉详情并
     * 将详情键并入摘要；详情失败回退列表摘要。getEmails 返回的 session.email
     * 与收信邮箱不符时报错（session 为 null 视为会话失效）。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $raw = trim((string) $token);
        $prefix = 'tempMailSession=';
        $mailbox = str_starts_with($raw, $prefix) ? substr($raw, strlen($prefix)) : $raw;
        if ($mailbox === '') {
            throw new \InvalidArgumentException('tmpkit: 会话 token 为空');
        }

        $data = self::rpc('getEmails', ['offset' => 0, 'limit' => 20], $mailbox);

        // 会话指向校验：getEmails 响应自带 session.email（无 Cookie 时
        // session 为 null，同样视为会话失效）
        $sess = $data['session'] ?? null;
        if (is_array($sess)) {
            $got = self::mapGet($sess, 'email');
            if ($got !== '' && $got !== $email) {
                throw new \RuntimeException("tmpkit: 会话邮箱不匹配（响应 {$got}，请求 {$email}）");
            }
        } else {
            throw new \RuntimeException('tmpkit: 会话已失效（getEmails 返回空会话）');
        }

        $list = $data['emails'] ?? null;
        if (!is_array($list)) {
            throw new \RuntimeException('tmpkit: 邮件列表响应缺 emails 字段');
        }

        $out = [];
        foreach ($list as $item) {
            if (!is_array($item)) {
                continue;
            }
            // mailId 为数字：合法时逐封拉详情（详情键并入摘要，跳过会话类键）
            $mailId = $item['mailId'] ?? null;
            if (is_numeric($mailId) && (float) $mailId > 0) {
                try {
                    $detail = self::rpc('getEmailDetail', ['mailId' => (int) $mailId], $mailbox);
                    foreach ($detail as $k => $v) {
                        if (in_array($k, ['session', 'emails', 'total', 'error'], true)) {
                            continue;
                        }
                        $item[$k] = $v;
                    }
                } catch (\Throwable) {
                    // 详情失败回退列表摘要
                }
            }
            $out[] = Normalize::email($item, $email);
        }
        return $out;
    }
}