<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;

/**
 * NoxenDe5Net 渠道实现（UniMail-Bot 公共实例 tempmail.noxen.de5.net）
 *
 * 完整接入契约：
 *   登录 POST /api/login {"username":"guest","password":"123456"}
 *     → 200 {"success":true,"role":"guest"} 并 Set-Cookie: iding-session=<JWT>；
 *   建箱 GET /api/generate → 200 {"email":"随机@域名","expires":毫秒时间戳}；
 *   读信 GET /api/emails?mailbox=<地址>&limit=20 → 200 邮件数组（无邮件为 []）；
 *   详情 GET /api/email/{id} → {..., content, html_content, to_addrs, r2_bucket,
 *     r2_object_key, download}；content/html_content 平台恒为空，原始 EML 存
 *     Cloudflare R2，download 指向 GET /api/email/{id}/download 下载端点。
 * 鉴权边界：读信不带会话 Cookie 返回 401；访客邮箱只能查自己的 mailbox。
 * 会话隔离：先 GET /api/session 兜底校验 cookie，未通过则重新 login；
 *   凭据串只由会话 Cookie 与同源地址构成，读信时逐请求显式携带。
 */
final class NoxenDe5Net
{
    private const BASE = 'https://tempmail.noxen.de5.net';
    private const USER = 'guest';
    private const PASS = '123456';

    /** 本渠道凭据串前缀 */
    private const TOKEN_PREFIX = 'noxen-de5-net|';

    /** @var array<string,string> */
    private const HEADERS = [
        'User-Agent' => 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 '
            . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36',
        'Accept' => 'application/json',
    ];

    /** 登录取得会话 Cookie（iding-session=JWT）。 */
    private static function sessionCookie(): string
    {
        $body = (string) json_encode(['username' => self::USER, 'password' => self::PASS]);
        $resp = HttpClient::post(
            self::BASE . '/api/login',
            ['Content-Type' => 'application/json'] + self::HEADERS,
            body: $body,
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('noxen-de5-net login: http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        if (($data['success'] ?? null) !== true) {
            throw new \RuntimeException('noxen-de5-net login: 登录失败');
        }
        $session = self::cookieValue($resp->getHeader('Set-Cookie'), 'iding-session');
        if ($session === '') {
            throw new \RuntimeException('noxen-de5-net login: 未下发会话 Cookie');
        }
        return 'iding-session=' . $session;
    }

    /** 从 Set-Cookie 列表中提取指定 cookie 的值。 */
    private static function cookieValue(array $setCookies, string $name): string
    {
        foreach ($setCookies as $sc) {
            $kv = trim(explode(';', $sc, 2)[0]);
            if (str_starts_with($kv, $name . '=')) {
                return substr($kv, strlen($name) + 1);
            }
        }
        return '';
    }

    /** 校验会话 Cookie 是否仍有效（GET /api/session）。 */
    private static function cookieStillValid(string $cookie): bool
    {
        $resp = HttpClient::get(self::BASE . '/api/session', self::HEADERS + ['Cookie' => $cookie]);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return false;
        }
        $data = HttpClient::json($resp);
        return ($data['authenticated'] ?? null) === true;
    }

    /** 校验域名是否在平台域名池内。 */
    private static function domainInPool(string $domain): bool
    {
        $resp = HttpClient::get(self::BASE . '/api/domains', self::HEADERS);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return false;
        }
        $pool = HttpClient::json($resp);
        if (!is_array($pool)) {
            return false;
        }
        foreach ($pool as $d) {
            if (strtolower((string) $d) === strtolower($domain)) {
                return true;
            }
        }
        return false;
    }

    /**
     * 登录并创建临时邮箱。
     *
     * @param string|null $domain 可选首选域名（空时平台自动选域）
     * token 凭据串格式："noxen-de5-net|<iding-session=JWT>|base=<基址>"
     */
    public static function generate(?string $domain = null): EmailInfo
    {
        $want = trim((string) $domain);
        if ($want !== '' && !self::domainInPool($want)) {
            throw new \RuntimeException('noxen-de5-net generate: 域名 ' . $want . ' 不在平台域名池');
        }

        $cookie = self::sessionCookie();
        $resp = HttpClient::get(self::BASE . '/api/generate', self::HEADERS + ['Cookie' => $cookie]);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('noxen-de5-net generate: http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $email = trim((string) ($data['email'] ?? ''));
        if ($email === '') {
            throw new \RuntimeException('noxen-de5-net generate: 响应缺少 email');
        }
        $expires = (int) ($data['expires'] ?? 0);
        $expiresAt = $expires > 0 ? (int) $expires : null;
        $token = self::TOKEN_PREFIX . rawurlencode($cookie) . '|base=' . self::BASE;
        return new EmailInfo('noxen-de5-net', $email, $token, expiresAt: $expiresAt);
    }

    /** 从凭据串解析会话 Cookie。 */
    private static function cookieFromToken(string $token): string
    {
        if (!str_starts_with($token, self::TOKEN_PREFIX)) {
            throw new \InvalidArgumentException('noxen-de5-net: token 格式错误');
        }
        $enc = substr($token, strlen(self::TOKEN_PREFIX));
        $enc = preg_replace('/\|base=' . preg_quote(self::BASE, '/') . '$/', '', $enc) ?? $enc;
        return rawurldecode($enc);
    }

    /**
     * 读取收件箱。
     *
     * 正文获取优先级：
     * 1) 详情（download 定位）+ 下载端点拉取原始 EML，本地拆分 text/plain 与 text/html；
     * 2) 详情/EML 链路不可得时，用 verification_code 置顶 + preview 合成占位正文。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $addr = trim($email);
        $cookie = self::cookieFromToken(trim($token ?? ''));
        if (!self::cookieStillValid($cookie)) {
            $cookie = self::sessionCookie();
        }

        $resp = HttpClient::get(
            self::BASE . '/api/emails',
            self::HEADERS + ['Cookie' => $cookie],
            query: ['mailbox' => $addr, 'limit' => '20'],
        );
        if ($resp->getStatusCode() === 401) {
            throw new \RuntimeException('noxen-de5-net 读信: http 401（会话失效或非本会话邮箱）');
        }
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('noxen-de5-net 读信: http ' . $resp->getStatusCode());
        }
        $list = HttpClient::json($resp);
        if (!is_array($list) || !array_is_list($list)) {
            throw new \RuntimeException('noxen-de5-net 读信: 收件箱响应非数组');
        }

        $out = [];
        foreach ($list as $m) {
            if (!is_array($m)) {
                continue;
            }
            $flat = $m;
            $flat['from'] = $m['sender'] ?? '';
            $flat['to'] = $addr;
            $flat['date'] = $m['received_at'] ?? null;
            $flat['text'] = $m['preview'] ?? '';
            $flat['isRead'] = $m['is_read'] ?? null;
            $full = false;
            $id = trim((string) ($m['id'] ?? ''));
            if ($id !== '' && $id !== '0') {
                $detail = self::fetchDetail($cookie, $id);
                if ($detail !== null) {
                    $flat['content'] = $detail['content'] ?? null;
                    $flat['html_content'] = $detail['html_content'] ?? null;
                    $flat['to_addrs'] = $detail['to_addrs'] ?? null;
                    $flat['r2_bucket'] = $detail['r2_bucket'] ?? null;
                    $flat['r2_object_key'] = $detail['r2_object_key'] ?? null;
                    $dl = (string) ($detail['download'] ?? '');
                    if ($dl !== '') {
                        $eml = self::fetchEml($cookie, $dl);
                        if ($eml !== '') {
                            [$text, $html] = self::parseEml($eml);
                            if ($text !== '' || $html !== '') {
                                $flat['text'] = $text;
                                $flat['html'] = $html;
                                $full = true;
                            }
                        }
                    }
                }
                if (!$full) {
                    $flat['text'] = self::composePlaceholder($m);
                }
            }
            $out[] = Normalize::email($flat, $addr);
        }
        return $out;
    }

    /** 拉取单封邮件详情（GET /api/email/{id}），失败返回 null。 */
    private static function fetchDetail(string $cookie, string $id): ?array
    {
        $resp = HttpClient::get(
            self::BASE . '/api/email/' . rawurlencode($id),
            self::HEADERS + ['Cookie' => $cookie],
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return null;
        }
        return HttpClient::json($resp);
    }

    /** 拉取原始 EML 报文（详情 download 字段指向的下载端点）。 */
    private static function fetchEml(string $cookie, string $dlPath): string
    {
        $u = $dlPath;
        if (!str_starts_with($u, 'http://') && !str_starts_with($u, 'https://')) {
            $u = self::BASE . $u;
        }
        $resp = HttpClient::get(
            $u,
            [
                'User-Agent' => self::HEADERS['User-Agent'],
                'Accept' => 'message/rfc822, */*',
                'Cookie' => $cookie,
            ],
            timeout: 20,
        );
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return '';
        }
        return (string) $resp->getBody();
    }

    /** 全文不可得时的合成占位正文：verification_code 置顶，preview 附后。 */
    private static function composePlaceholder(array $m): string
    {
        $code = trim((string) ($m['verification_code'] ?? ''));
        $preview = trim((string) ($m['preview'] ?? ''));
        $parts = [];
        if ($code !== '') {
            $parts[] = '验证码: ' . $code;
        }
        if ($preview !== '') {
            $parts[] = $preview;
        }
        return implode("\n\n", $parts);
    }

    /**
     * 解析 EML 原始报文 → [纯文本正文, HTML 正文]。
     *
     * @return array{0:string,1:string}
     */
    private static function parseEml(string $eml): array
    {
        $payload = str_replace("\r\n", "\n", $eml);
        $payload = str_replace("\r", '', $payload);
        [$headers, $body] = self::splitEml($payload, 0);
        return self::parseEntity($headers, $body);
    }

    /**
     * 将原始报文切分为首部 map 与正文块。
     *
     * @return array{0:array<string,string>,1:string}
     */
    private static function splitEml(string $payload, int $offset): array
    {
        $lines = explode("\n", $payload);
        $headers = [];
        $i = $offset;
        if ($i < count($lines) && str_starts_with($lines[$i], 'From ')) {
            $i++;
        }
        $curKey = '';
        for (; $i < count($lines); $i++) {
            $line = $lines[$i];
            if ($line === '') {
                $i++;
                break;
            }
            // 折行续行（RFC 5322）：以空白开头且已有当前头键时拼接到上一条
            if (($line[0] === ' ' || $line[0] === "\t") && $curKey !== '') {
                $headers[$curKey] .= ' ' . trim($line);
                continue;
            }
            $k = strpos($line, ':');
            if ($k !== false && $k > 0) {
                $curKey = strtolower(trim(substr($line, 0, $k)));
                $headers[$curKey] = trim(substr($line, $k + 1));
            }
        }
        $body = implode("\n", array_slice($lines, $i));
        return [$headers, $body];
    }

    /** 从 Content-Type 头值提取 multipart boundary（引号可选）。 */
    private static function extractBoundary(string $ctRaw): string
    {
        if (preg_match('/boundary="?([^";\s]+)"?/i', $ctRaw, $mm) === 1) {
            return $mm[1];
        }
        return '';
    }

    /**
     * 按 boundary 切出各 part（含各自首部行）。
     *
     * @return string[]
     */
    private static function splitMultipart(string $body, string $boundary): array
    {
        $segments = explode('--' . $boundary, $body);
        $parts = [];
        foreach ($segments as $seg) {
            if (str_starts_with($seg, "\n")) {
                $seg = substr($seg, 1);
            }
            $seg = preg_replace('/--\n?$/', '', $seg) ?? $seg;
            if (trim($seg) !== '') {
                $parts[] = $seg;
            }
        }
        return $parts;
    }

    /**
     * 递归解析单个 MIME 实体（上游 parseEntity 同构）。
     *
     * @param array<string,string> $headers
     * @return array{0:string,1:string}
     */
    private static function parseEntity(array $headers, string $body): array
    {
        $ct = strtolower($headers['content-type'] ?? '');
        $cte = strtolower($headers['content-transfer-encoding'] ?? '');

        // 单体：text/html 或 text/plain（含无 Content-Type 时按纯文本处理）
        if (!str_starts_with($ct, 'multipart/')) {
            $decoded = self::decodePart($body, $cte);
            return str_contains($ct, 'text/html') ? ['', $decoded] : [$decoded, ''];
        }

        // 复合：递归拆分，text 槽与 html 槽各自取第一个非空命中
        $text = '';
        $html = '';
        $boundary = self::extractBoundary($headers['content-type'] ?? '');
        if ($boundary !== '') {
            foreach (self::splitMultipart($body, $boundary) as $part) {
                [$ph, $pb] = self::splitEml("#participant\n" . $part, 1);
                $pct = strtolower($ph['content-type'] ?? '');
                if (str_starts_with($pct, 'multipart/')) {
                    [$t, $h] = self::parseEntity($ph, $pb);
                    if ($text === '') {
                        $text = $t;
                    }
                    if ($html === '') {
                        $html = $h;
                    }
                } elseif (str_starts_with($pct, 'message/rfc822')) {
                    [$nh, $nb] = self::splitEml($pb, 0);
                    [$t, $h] = self::parseEntity($nh, $nb);
                    if ($text === '') {
                        $text = $t;
                    }
                    if ($html === '') {
                        $html = $h;
                    }
                } elseif (str_contains($pct, 'rfc822-headers')) {
                    // 纯头部 part 跳过，正文在后续 part 中抓取
                    continue;
                } else {
                    [$t, $h] = self::parseEntity($ph, $pb);
                    if ($text === '') {
                        $text = $t;
                    }
                    if ($html === '') {
                        $html = $h;
                    }
                }
                if ($text !== '' && $html !== '') {
                    break;
                }
            }
        }
        // 无 HTML 命中时从整体原文兜底抓取 HTML 片段（上游 guessHtmlFromRaw 同构）
        if ($html === '') {
            $html = self::guessHtml($body);
        }
        return [$text, $html];
    }

    /** 按 Content-Transfer-Encoding 解码 part 内容。 */
    private static function decodePart(string $data, string $cte): string
    {
        switch (strtolower(trim($cte))) {
            case 'base64':
                $joined = preg_replace('/[\r\n\t ]/', '', $data) ?? $data;
                $decoded = base64_decode($joined, true);
                if ($decoded === false) {
                    return $data;
                }
                return trim($decoded);
            case 'quoted-printable':
                return trim(quoted_printable_decode($data));
            default:
                // 7bit/8bit/binary：原样返回
                return trim($data);
        }
    }

    /** 从整体原文中抓取 <html>…</html> 片段（上游 guessHtmlFromRaw 同构）。 */
    private static function guessHtml(string $body): string
    {
        if ($body === '') {
            return '';
        }
        $lower = strtolower($body);
        $hs = strpos($lower, '<html');
        if ($hs === false) {
            $hs = strpos($lower, '<!doctype html');
        }
        if ($hs === false) {
            return '';
        }
        $he = strrpos($lower, '</html>');
        if ($he === false || $he < $hs) {
            return '';
        }
        return substr($body, $hs, $he - $hs + 7);
    }
}