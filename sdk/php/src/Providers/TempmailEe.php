<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;
use GuzzleHttp\Psr7\Response;

/**
 * TempmailEE 渠道实现（tempmail.ee）
 *
 * 平台读信端 /api/mails 的 403 是「会话 Cookie 绑定校验」：
 * change（换箱）返回的 Set-Cookie 中 temp_mail_session 与 temp_email 共同构成读信凭据；
 * 同会话查别的邮箱、只带 session 不带 temp_email 均 403，带齐即 200。
 * 因此只要在同一个「事务」内完成 change → 提取 Set-Cookie → 读信即可读到邮件。
 *
 * 会话凭据由本渠道逐请求以显式 Cookie 请求头携带：
 * - POST /api/mailbox/change 若不带 sec-ch-ua 头会被平台拒绝（403 Browser request required）；
 * - POST /api/mails 带完整 sec-ch-ua 三件套，UA 使用固定 Chrome 154（与其 sec-ch-ua 品牌版本一致）。
 */
final class TempmailEe
{
    private const CHANNEL = 'tempmail-ee';
    private const BASE = 'https://tempmail.ee';

    /** 与 sec-ch-ua 品牌版本一致的固定 UA（Chrome 154 / Linux） */
    private const UA = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36';

    /** Token 前缀，用于识别本渠道会话凭据串 */
    private const TOKEN_PREFIX = 'tempmail-ee|';

    /** 建箱提交的浏览器指纹（与官方前端一致） */
    private static function browserIntegrity(): array
    {
        return [
            'webdriver' => false, 'languagesMissing' => false, 'languageMissing' => false,
            'pluginsMissing' => false, 'pluginsUndefined' => false, 'outerSizeMissing' => false,
            'innerSizeMissing' => false, 'screenMissing' => false, 'screenDepthMissing' => false,
            'timezoneMissing' => false, 'timezoneOffsetMissing' => false,
            'userAgentDataPresent' => true, 'userAgentMissing' => false, 'platformClass' => 'Linux',
            'mobile' => false, 'collectionFailed' => false,
        ];
    }

    /**
     * 设置浏览器特征安全头（同站 fetch 全套）。
     *
     * @param bool $withSecCh 是否携带 sec-ch-ua 三件套（change 必须，平台据此放行）
     * @return array<string,string>
     */
    private static function browserHeaders(bool $withSecCh): array
    {
        $h = [
            'Accept' => 'application/json',
            'Content-Type' => 'application/json',
            'X-Requested-With' => 'XMLHttpRequest',
            'Origin' => self::BASE,
            'Referer' => self::BASE . '/',
            'Sec-Fetch-Site' => 'same-origin',
            'Sec-Fetch-Mode' => 'cors',
            'Sec-Fetch-Dest' => 'empty',
            'Accept-Language' => 'en-US,en;q=0.9',
            'User-Agent' => self::UA,
        ];
        if ($withSecCh) {
            $h['Sec-Ch-Ua'] = '"Chromium";v="154", "Google Chrome";v="154", "Not.A/Brand";v="99"';
            $h['Sec-Ch-Ua-Mobile'] = '?0';
            $h['Sec-Ch-Ua-Platform'] = '"Linux"';
        }
        return $h;
    }

    /**
     * 发起带浏览器特征头的 POST（显式 Cookie 头模式）。
     *
     * @param array<mixed> $json      请求体
     * @param string       $cookie    显式 Cookie 请求头（会话凭据串，空则不携带）
     */
    private static function post(string $path, array $json, bool $withSecCh, string $cookie): Response
    {
        $headers = self::browserHeaders($withSecCh);
        if ($cookie !== '') {
            $headers['Cookie'] = $cookie;
        }
        return HttpClient::post(self::BASE . $path, $headers, json: $json);
    }

    /**
     * 从 change 响应提取会话 Cookie 键值对（显式 Cookie 头模式下手动接管会话）。
     *
     * @return array{0:string,1:string} [temp_email, temp_mail_session]
     */
    private static function cookieFromResponse(Response $resp): array
    {
        $email = '';
        $session = '';
        foreach ($resp->getHeader('Set-Cookie') as $sc) {
            $kv = $sc;
            $semi = strpos($sc, ';');
            if ($semi !== false) {
                $kv = substr($sc, 0, $semi);
            }
            if (str_starts_with($kv, 'temp_email=')) {
                $email = substr($kv, strlen('temp_email='));
            } elseif (str_starts_with($kv, 'temp_mail_session=')) {
                $session = substr($kv, strlen('temp_mail_session='));
            }
        }
        return [$email, $session];
    }

    /** 由邮箱与 temp_mail_session 组装渠道内部凭据串 */
    private static function tokenBuild(string $email, string $session): string
    {
        return self::TOKEN_PREFIX . 'temp_email=' . $email . '; temp_mail_session=' . $session;
    }

    /**
     * 解析读信凭据，返回 [Cookie 头值, 是否有效]。
     * 会话绑定邮箱：以请求邮箱为准重拼 cookie，防止凭据与邮箱错配。
     *
     * @return array{0:string,1:bool}
     */
    private static function parseToken(string $token, string $email): array
    {
        if (!str_starts_with($token, self::TOKEN_PREFIX)) {
            return ['', false];
        }
        $cred = substr($token, strlen(self::TOKEN_PREFIX));
        $session = '';
        foreach (explode(';', $cred) as $part) {
            $kv = trim($part);
            if (str_starts_with($kv, 'temp_mail_session=')) {
                $session = substr($kv, strlen('temp_mail_session='));
            }
        }
        if ($session === '') {
            return ['', false];
        }
        return ['temp_email=' . $email . '; temp_mail_session=' . $session, true];
    }

    public static function generate(): EmailInfo
    {
        // 步骤 1：GET / 建立 Cookie 会话（面熟首访），响应直接丢弃
        $bootHeaders = [
            'User-Agent' => self::UA,
            'Accept' => 'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8',
        ];
        $boot = HttpClient::get(self::BASE, $bootHeaders);
        $boot->getBody()->close();

        // 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua）
        $payload = [
            'turnstileToken' => null,
            'browserIntegrity' => self::browserIntegrity(),
        ];
        $resp = self::post('/api/mailbox/change', $payload, true, '');
        $chg = HttpClient::json($resp);
        $newEmail = trim((string) ($chg['newEmail'] ?? ''));
        if (empty($chg['success']) || $newEmail === '') {
            throw new \RuntimeException(
                'tempmail-ee: 建箱失败（status ' . $resp->getStatusCode() . '）'
            );
        }

        // 步骤 3：从 Set-Cookie 接管会话凭据，供读信校验使用
        [$cookieEmail, $session] = self::cookieFromResponse($resp);
        $email = $newEmail;
        if ($cookieEmail !== '' && $cookieEmail !== $email) {
            // 以防万一以 Cookie 为准，Cookie 仅取 session
            $email = $cookieEmail;
        }
        if ($session === '') {
            throw new \RuntimeException('tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信');
        }

        $expiresStr = (string) ($chg['expiresAt'] ?? '');
        $ts = strtotime($expiresStr);
        $expiresAt = $ts !== false ? $ts * 1000 : (int) ((time() + 3600) * 1000);

        return new EmailInfo(
            self::CHANNEL,
            $email,
            self::tokenBuild($email, $session),
            expiresAt: $expiresAt,
        );
    }

    /** 按候选键顺序从数组提取字符串值 */
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

    /**
     * 将 /api/mails 列表行（仅元数据）转换为归一化骨架数组；
     * id 缺失视为无效行返回 null。
     *
     * @return array<string,mixed>|null
     */
    private static function rowToRaw(array $row, string $email): ?array
    {
        $id = self::pickStr($row, ['id']);
        if ($id === '') {
            return null;
        }
        $to = self::pickStr($row, ['toAddress', 'to']);
        if ($to === '') {
            $to = $email;
        }
        $created = self::pickStr($row, ['createdAt', 'date', 'receivedAt']);
        if ($created === '') {
            $created = gmdate('Y-m-d\TH:i:s\Z');
        }
        return [
            'id' => $id,
            'from' => self::pickStr($row, ['fromAddress', 'from', 'sender']),
            'to' => $to,
            'subject' => self::pickStr($row, ['subject']),
            'text' => '',
            'html' => '',
            'date' => $created,
            'isRead' => ($row['isRead'] ?? false) === true
                || ($row['isRead'] ?? null) === 1
                || ($row['isRead'] ?? null) === '1',
        ];
    }

    /**
     * 拉取单封详情并填充正文（按引用修改骨架）。
     *
     * GET /api/mails/{id}（带显式会话 Cookie），content 为平台包装后的
     * MIME multipart 原文（HTML 实体转义版），解析后写入骨架 text/html。
     */
    private static function fillDetail(array &$raw, string $cookie): bool
    {
        $headers = self::browserHeaders(false);
        $headers['Cookie'] = $cookie;
        $resp = HttpClient::get(self::BASE . '/api/mails/' . rawurlencode($raw['id']), $headers);
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            return false;
        }
        $detail = HttpClient::json($resp);
        $content = self::pickStr($detail, ['content']);
        if ($content === '') {
            // content 缺失时回退兜底候选键，避免平台字段演进后正文丢失
            $content = self::pickStr($detail, ['text', 'body', 'html']);
            if ($content === '') {
                return false;
            }
        }
        [$text, $htmlStr] = self::parseContent($content);
        if ($raw['text'] === '') {
            $raw['text'] = $text;
        }
        if ($raw['html'] === '') {
            $raw['html'] = $htmlStr;
        }
        // 详情缺失 from/subject 时补全（列表行已带则不动）
        if ($raw['from'] === '') {
            $raw['from'] = self::pickStr($detail, ['fromAddress', 'from']);
        }
        if ($raw['subject'] === '') {
            $raw['subject'] = self::pickStr($detail, ['subject']);
        }
        return true;
    }

    /**
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $email = trim($email);
        if ($email === '') {
            throw new \InvalidArgumentException('tempmail-ee: 邮箱为空');
        }
        $token = trim($token ?? '');
        if ($token === '') {
            throw new \InvalidArgumentException('tempmail-ee: token 为空');
        }
        [$cookie, $ok] = self::parseToken($token, $email);
        if (!$ok) {
            throw new \RuntimeException('tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱');
        }

        $resp = self::post('/api/mails', ['email' => $email], true, $cookie);
        if ($resp->getStatusCode() === 403) {
            throw new \RuntimeException('tempmail-ee: 读信 http 403（会话 Cookie 校验失败，邮箱可能已过期，请重新 Generate）');
        }
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('tempmail-ee: 读信 http ' . $resp->getStatusCode());
        }
        $data = HttpClient::json($resp);
        $rows = $data['mails'] ?? null;
        if (!is_array($rows)) {
            return [];
        }

        $out = [];
        foreach ($rows as $m) {
            if (!is_array($m)) {
                continue;
            }
            $raw = self::rowToRaw($m, $email);
            if ($raw === null) {
                continue;
            }
            // 单封详情拉取失败不阻塞列表其余邮件
            if (!self::fillDetail($raw, $cookie)) {
                continue;
            }
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }

    private static function htmlToText(string $src): string
    {
        $cleaned = preg_replace('/<(script|style)[^>]*>.*?<\/\1>/is', ' ', $src) ?? $src;
        $cleaned = strip_tags($cleaned);
        $cleaned = html_entity_decode($cleaned, ENT_QUOTES | ENT_HTML5);
        return trim(preg_replace('/\s+/', ' ', $cleaned) ?? $cleaned);
    }

    /**
     * 解析详情 content：
     * 1) content 已被平台做 HTML 实体转义，先反转义；
     * 2) content 是 MIME multipart 原文（含顶部边界装饰头），按首个边界行切块，
     *    各 part 依据 Content-Transfer-Encoding 做 base64 / quoted-printable 解码，
     *    text part 入文本、html part 入 HTML。
     *
     * @return array{0:string,1:string} [纯文本正文, HTML 正文]（两者之一为空时互为兜底合成）
     */
    private static function parseContent(string $rawContent): array
    {
        $payload = html_entity_decode($rawContent, ENT_QUOTES | ENT_HTML5);
        $payload = str_replace("\r\n", "\n", $payload);
        $lines = explode("\n", $payload);
        $boundary = self::boundaryOf($lines);
        $text = '';
        $htmlStr = '';
        if ($boundary !== '') {
            [$text, $htmlStr] = self::splitParts($lines, $boundary);
        }
        if ($boundary === '' || ($text === '' && $htmlStr === '')) {
            // 无有效 multipart 结构：整个 content 去壳后作为正文，避免丢信
            [$text, $htmlStr] = self::singleton($payload);
        }
        if ($text === '' && $htmlStr !== '') {
            $text = self::htmlToText($htmlStr);
        }
        if ($htmlStr === '' && $text !== '') {
            $htmlStr = '<html><body><pre>' . htmlspecialchars($text, ENT_QUOTES) . '</pre></body></html>';
        }
        return [$text, $htmlStr];
    }

    /** 扫描前 120 行寻找边界行（默认 multipart 边界行处于块首） */
    private static function boundaryOf(array $lines): string
    {
        $limit = min(count($lines), 120);
        for ($i = 0; $i < $limit; $i++) {
            $line = rtrim($lines[$i], "\r");
            if (str_starts_with($line, '--') && strlen($line) > 2) {
                return substr($line, 2);
            }
        }
        return '';
    }

    /**
     * 单 part（无边界或拆不出内容）降级解析：
     * 依次按「头部区隔（首个空行之后）→ 整体」取内容，
     * 并通过关键字识别 Content-Transfer-Encoding 做解码。
     *
     * @return array{0:string,1:string}
     */
    private static function singleton(string $payload): array
    {
        $body = trim($payload);
        if (!str_contains($body, "\n")) {
            return [$body, ''];
        }
        $lines = explode("\n", $payload);
        $total = count($lines);
        for ($i = 0; $i < $total; $i++) {
            if (trim($lines[$i]) === '') {
                $body = implode("\n", array_slice($lines, $i + 1));
                break;
            }
            if ($i > 40 || ($i >= 3 && !str_contains($lines[$i], ':'))) {
                break;
            }
        }
        $lower = strtolower($payload);
        $cte = str_contains($lower, 'base64') ? 'base64'
            : (str_contains($lower, 'quoted-printable') ? 'quoted-printable' : '');
        return [self::decodePart($body, $cte), ''];
    }

    /**
     * 按 boundary 拆分 multipart 并解码归并 text/html 两个 part。
     *
     * @return array{0:string,1:string}
     */
    private static function splitParts(array $lines, string $boundary): array
    {
        $text = '';
        $htmlStr = '';
        $total = count($lines);
        for ($i = 0; $i < $total; $i++) {
            if (!str_starts_with($lines[$i], '--' . $boundary)) {
                continue;
            }
            if (str_starts_with($lines[$i], '--' . $boundary . '--')) {
                break;
            }
            $headers = [];
            $j = $i + 1;
            // part 头部：直到首个空行
            while ($j < $total && $lines[$j] !== '' && !str_starts_with($lines[$j], '--' . $boundary)) {
                $ln = $lines[$j];
                $colon = strpos($ln, ':');
                if ($colon !== false && $colon > 0) {
                    $headers[strtolower(trim(substr($ln, 0, $colon)))] = trim(substr($ln, $colon + 1));
                }
                $j++;
            }
            if ($j < $total && $lines[$j] === '') {
                $j++;
            }
            // part 正文：到下一个边界行为止，内部空行属于正文内容
            $bodyLines = [];
            while ($j < $total && !str_starts_with($lines[$j], '--' . $boundary)) {
                $bodyLines[] = $lines[$j];
                $j++;
            }
            [$text, $htmlStr] = self::mergePart($bodyLines, $headers, $text, $htmlStr);
            $i = $j - 1;
        }
        return [$text, $htmlStr];
    }

    /**
     * 单个 part 的解码归并：按 content-type 归类，缺省回退 text 槽。
     *
     * @param array<string,string> $headers
     * @return array{0:string,1:string}
     */
    private static function mergePart(array $body, array $headers, string $text, string $htmlStr): array
    {
        $ct = strtolower($headers['content-type'] ?? '');
        $semi = strpos($ct, ';');
        if ($semi !== false) {
            $ct = substr($ct, 0, $semi);
        }
        $cte = strtolower($headers['content-transfer-encoding'] ?? '');
        $content = implode("\n", $body);
        if (str_contains($ct, 'text/plain')) {
            if ($text === '') {
                $text = self::decodePart($content, $cte);
            }
            return [$text, $htmlStr];
        }
        if (str_contains($ct, 'text/html')) {
            if ($htmlStr === '') {
                $htmlStr = self::decodePart($content, $cte);
            }
            return [$text, $htmlStr];
        }
        if ($text === '') {
            $text = self::decodePart($content, $cte);
        }
        return [$text, $htmlStr];
    }

    /** 按 Content-Transfer-Encoding 解码 part 内容 */
    private static function decodePart(string $data, string $cte): string
    {
        $data = trim($data);
        switch (strtolower($cte)) {
            case 'base64':
                $joined = preg_replace('/[\s\r\n\t]/', '', $data) ?? $data;
                $dec = base64_decode($joined, true);
                return $dec === false ? $data : trim($dec);
            case 'quoted-printable':
                return trim(quoted_printable_decode($data));
            default:
                return $data;
        }
    }
}