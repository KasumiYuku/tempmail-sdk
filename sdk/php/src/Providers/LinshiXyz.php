<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * LinshiXYZ 渠道实现（linshi.xyz）
 *
 * 无建箱请求：本地随机 6 位 hex 前缀（官网 client 即用短 id）+ @linshi.xyz。
 * 读信 GET https://linshi.xyz/api/mails/{前缀}，无邮件时返回空数组 []，
 *   有邮件时为邮件对象数组（元素含 headers{from,to,subject,date} 与 html 正文）。
 * 归一化时将 headers 平铺并注入收件人地址；非数组骨架（如 {ok:false}）整体失败，
 * 交由上层 fallback 渠道处理。
 */
final class LinshiXyz
{
    private const CHANNEL = 'linshi-xyz';
    private const BASE = 'https://linshi.xyz';
    private const DOMAIN = 'linshi.xyz';
    private const HEX_DIGITS = '0123456789abcdef';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
    ];

    /** 生成本地随机 6 位 hex 前缀（与官网 client 相同格式）。 */
    private static function localName(): string
    {
        $n = strlen(self::HEX_DIGITS);
        $s = '';
        for ($i = 0; $i < 6; $i++) {
            $s .= self::HEX_DIGITS[random_int(0, $n - 1)];
        }
        return $s;
    }

    /** 创建 linshi.xyz 临时邮箱：无需服务端建箱，token 复用完整地址。 */
    public static function generate(): EmailInfo
    {
        $addr = self::localName() . '@' . self::DOMAIN;
        return new EmailInfo(self::CHANNEL, $addr, $addr);
    }

    /**
     * 读取 linshi.xyz 收件箱。
     * GET /api/mails/{前缀}（URL 编码），响应为邮件对象数组。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        if ($addr === '' || !str_contains($addr, '@')) {
            throw new \InvalidArgumentException('linshi-xyz: 邮箱地址无效: ' . $addr);
        }
        $local = explode('@', $addr, 2)[0];
        $resp = HttpClient::get(self::BASE . '/api/mails/' . rawurlencode($local), self::HEADERS);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('linshi-xyz: 读取收件箱失败 http ' . $resp->getStatusCode());
        }
        $list = HttpClient::json($resp);
        if (!is_array($list) || !array_is_list($list)) {
            throw new \RuntimeException('linshi-xyz: 收件箱响应非数组');
        }

        $out = [];
        foreach ($list as $m) {
            if (!is_array($m)) {
                continue;
            }
            // headers 对象平铺为顶层字段并注入收件人
            if (isset($m['headers']) && is_array($m['headers'])) {
                $m = array_merge($m['headers'], $m);
            }
            if (!array_key_exists('to', $m)) {
                $m['to'] = $addr;
            }
            $out[] = Normalize::email($m, $addr);
        }
        return $out;
    }
}