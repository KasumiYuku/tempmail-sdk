<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * 30minemail 渠道实现（30minemail.com）
 *
 * 官网邮箱由服务端 16 位 hex 本地名识别：
 *   GET /?generate 返回完整 HTML 页面，从中解析 <地址>@30minemail.com；
 *   本地名无效邮箱访问 messages.php 时返回 ok:false/expired:true，故必须经服务端建箱。
 * 读信 GET /messages.php?email=<完整地址>&_=<unix毫秒>，响应
 *   {"ok":true,"expired":false,"count":0,"emails":[],"expires_in":...}。
 * emails 元素字段实测：{id,from,to,subject,date,html}（html 为完整正文）。
 * 无认证、无 Cookie、无 CSRF。
 */
final class ThirtyMinEmail
{
    private const CHANNEL = '30minemail';
    private const BASE = 'https://30minemail.com';
    private const DOMAIN = '30minemail.com';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Accept' => 'text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8',
    ];

    /**
     * 创建 30minemail.com 临时邮箱。
     * GET /?generate 返回 HTML 页面，从其中解析 16 位 hex 本地名地址；
     * token 复用完整地址（服务端以地址定位收件箱）。
     */
    public static function generate(): EmailInfo
    {
        $resp = HttpClient::get(self::BASE . '/?generate', self::HEADERS);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('30minemail: 创建邮箱失败 http ' . $resp->getStatusCode());
        }
        $page = (string) $resp->getBody();
        $idx = strpos($page, '@' . self::DOMAIN);
        if ($idx === false) {
            throw new \RuntimeException('30minemail: 创建页面未找到邮箱地址');
        }
        // 向前查找本地名起点：空白或 > 之后
        $start = $idx;
        while ($start > 0 && strpos(" \n\t>\"", $page[$start - 1]) === false) {
            $start--;
        }
        $local = trim(substr($page, $start, $idx - $start));
        if (strlen($local) < 8) {
            throw new \RuntimeException('30minemail: 创建页面解析地址异常');
        }
        $addr = $local . '@' . self::DOMAIN;
        return new EmailInfo(self::CHANNEL, $addr, $addr);
    }

    /**
     * 读取 30minemail.com 收件箱。
     * GET /messages.php?email=<完整地址>&_=<unix毫秒>，模拟官方轮询参数。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        if ($addr === '') {
            throw new \InvalidArgumentException('30minemail: 邮箱地址为空');
        }
        $resp = HttpClient::get(
            self::BASE . '/messages.php',
            [
                'User-Agent' => self::HEADERS['User-Agent'],
                'Accept' => 'application/json',
            ],
            query: ['email' => $addr, '_' => (string) ((int) floor(microtime(true) * 1000))],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('30minemail: 读取收件箱失败 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        if (($data['ok'] ?? null) !== true || ($data['expired'] ?? null) === true) {
            throw new \RuntimeException('30minemail: 收件箱不可用或已过期');
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
            // 列表元素无 to 字段时注入收件人地址
            if (!array_key_exists('to', $m)) {
                $m['to'] = $addr;
            }
            $out[] = Normalize::email($m, $addr);
        }
        return $out;
    }
}