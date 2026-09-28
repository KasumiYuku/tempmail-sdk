<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;
use GuzzleHttp\Psr7\Response;

/**
 * TempMailGG 渠道实现（temp-mail.gg，Laravel + Livewire v3）
 *
 * 纯 HTTP 复刻前端 Livewire 协议：
 * - GET / 首页返回 data-csrf 令牌与初始 wire:snapshot（JSON，data.email
 *   初始为空，须由 generateEmail 组件方法显式生成邮箱）；
 * - POST /livewire/update 为唯一边界：顶层 _token（=data-csrf）+
 *   components[0]{snapshot, updates:{}, calls:[{path,method,params}]}；
 *   200 且 calls 为空的 update 即平台轮询形态（响应 effects.html 含
 *   收件箱整块 UI，条目 div wire:click="selectEmail(<数字id>)"）；
 * - 详情：update calls=[{method:"selectEmail", params:[<数字id>]}]，
 *   响应 effects.html 模态框含 From / 主题 / Plain Text 全文
 *   （x-show 含 activeTab === 'text' 区块）。
 *
 * 会话粘性：本端无全局 Cookie 罐，本类维护静态 Cookie 数组（Laravel
 * 每次 livewire/update 轮换会话 Cookie，必须以响应 Set-Cookie 覆写并以
 * Cookie 头回填）；token 保存 {email, csrf, snapshot} 凭据串，读信时
 * 以响应快照 data.email 断言会话仍指向本邮箱（防串箱）。
 */
final class TempMailGg
{
    private const CHANNEL = 'temp-mail-gg';
    private const BASE = 'https://temp-mail.gg';

    /** Token 前缀，用于识别本渠道会话凭据串 */
    private const TOKEN_PREFIX = 'temp-mail-gg|';

    /** 固定浏览器 UA（与前端同源调用一致） */
    private const UA = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
        . '(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36';

    /** @var array<string,string> 会话 Cookie 键值表（本渠道无全局 Cookie 罐，静态接管会话） */
    private static array $cookies = [];

    /** 将响应 Set-Cookie 逐行解析为键值覆写入静态 Cookie 表（按浏览器语义取分号前部分） */
    private static function absorbCookies(Response $resp): void
    {
        foreach ($resp->getHeader('Set-Cookie') as $sc) {
            $kv = $sc;
            $semi = strpos($sc, ';');
            if ($semi !== false) {
                $kv = substr($sc, 0, $semi);
            }
            $eq = strpos($kv, '=');
            if ($eq === false) {
                continue;
            }
            $key = trim(substr($kv, 0, $eq));
            if ($key !== '') {
                self::$cookies[$key] = trim(substr($kv, $eq + 1));
            }
        }
    }

    /** 由静态 Cookie 表拼接 Cookie 请求头（空表返回空串） */
    private static function cookieHeader(): string
    {
        $parts = [];
        foreach (self::$cookies as $k => $v) {
            $parts[] = $k . '=' . $v;
        }
        return implode('; ', $parts);
    }

    /** 浏览器页面请求头（GET 首页），Cookie 头按静态表回填 */
    private static function pageHeaders(): array
    {
        $h = [
            'User-Agent' => self::UA,
            'Accept' => 'text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8',
            'Accept-Language' => 'en-US,en;q=0.9',
        ];
        $cookie = self::cookieHeader();
        if ($cookie !== '') {
            $h['Cookie'] = $cookie;
        }
        return $h;
    }

    /** livewire/update 同步请求头（同站 fetch 全套），Cookie 头按静态表回填 */
    private static function postHeaders(): array
    {
        $h = [
            'Content-Type' => 'application/json',
            'X-Livewire' => '',
            'X-Requested-With' => 'XMLHttpRequest',
            'Origin' => self::BASE,
            'Referer' => self::BASE . '/',
            'User-Agent' => self::UA,
        ];
        $cookie = self::cookieHeader();
        if ($cookie !== '') {
            $h['Cookie'] = $cookie;
        }
        return $h;
    }

    /** GET 页面请求并吸收响应 Set-Cookie（会话 Cookie 轮换） */
    private static function getPage(string $path): Response
    {
        $resp = HttpClient::get(self::BASE . $path, self::pageHeaders());
        self::absorbCookies($resp);
        return $resp;
    }

    /** POST JSON 请求（原始 body，json_encode 由调用方负责）并吸收响应 Set-Cookie */
    private static function postJson(string $body): Response
    {
        $resp = HttpClient::post(self::BASE . '/livewire/update', self::postHeaders(), body: $body);
        self::absorbCookies($resp);
        return $resp;
    }

    /**
     * 调用 /livewire/update（与前端同构；Cookie 表随响应 Set-Cookie 轮换）。
     *
     * @param string       $snapshot 当前组件快照 JSON 文本（快照文本的 JSON 转义由 json_encode 负责）
     * @param string       $csrf     data-csrf 令牌（顶层 _token）
     * @param string       $method   组件方法（空串 = 纯轮询刷新）
     * @param array<mixed> $params   方法参数
     * @return array<mixed>          update 响应（components 列表）
     */
    private static function livewireUpdate(string $snapshot, string $csrf, string $method, array $params): array
    {
        $calls = [];
        if ($method !== '') {
            $calls = [['path' => '', 'method' => $method, 'params' => $params]];
        }
        $payload = [
            '_token' => $csrf,
            'components' => [[
                'snapshot' => $snapshot,
                'updates' => (object) [],
                'calls' => $calls,
            ]],
        ];
        $resp = self::postJson((string) json_encode($payload, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));
        if ($resp->getStatusCode() === 419) {
            throw new \RuntimeException('temp-mail-gg: livewire 会话过期（419），请重新 Generate');
        }
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('temp-mail-gg: livewire/update http ' . $resp->getStatusCode());
        }
        return HttpClient::json($resp);
    }

    /**
     * 创建 temp-mail.gg 临时邮箱。
     *
     * GET 首页取 data-csrf + 初始快照 -> update calls=generateEmail ->
     * 响应快照 data.email 即新邮箱；凭据串 = 前缀 +
     * {email, csrf, snapshot（新快照数组）}。邮箱约 30 分钟无活动后过期。
     */
    public static function generate(): EmailInfo
    {
        $resp = self::getPage('');
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('temp-mail-gg: 首页 http ' . $resp->getStatusCode());
        }
        $page = (string) $resp->getBody();
        $csrf = '';
        $snapRaw = '';
        if (preg_match('/data-csrf="([^"]*)"/', $page, $m) === 1) {
            $csrf = trim($m[1]);
        }
        if (preg_match('/wire:snapshot="([^"]*)"/', $page, $m) === 1) {
            // HTML 属性内的快照 JSON 是双转义态（&quot; 等），反转义一层还原原始 JSON
            $snapRaw = html_entity_decode($m[1], ENT_QUOTES | ENT_HTML5);
        }
        if ($csrf === '' || $snapRaw === '') {
            throw new \RuntimeException('temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱');
        }

        // generateEmail：平台免费额度为免登录每时段 5 个，耗尽时响应快照无 email
        $updateResp = self::livewireUpdate($snapRaw, $csrf, 'generateEmail', []);
        $components = $updateResp['components'] ?? [];
        if (!is_array($components) || !isset($components[0])
            || trim((string) ($components[0]['snapshot'] ?? '')) === '') {
            throw new \RuntimeException('temp-mail-gg: generateEmail 响应异常（components 缺失）');
        }
        $snap2 = json_decode((string) $components[0]['snapshot'], true);
        if (!is_array($snap2)) {
            throw new \RuntimeException('temp-mail-gg: generateEmail 响应异常（快照解析失败）');
        }
        $email = trim((string) ($snap2['data']['email'] ?? ''));
        if ($email === '') {
            throw new \RuntimeException('temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录配额（每时段 5 个）');
        }

        $token = self::TOKEN_PREFIX . (string) json_encode(
            ['email' => $email, 'csrf' => $csrf, 'snapshot' => $snap2],
            JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE,
        );
        return new EmailInfo(self::CHANNEL, $email, $token);
    }

    /**
     * 读取 temp-mail.gg 收件箱。
     *
     * 流程：校验凭据串 -> 轮询 update（calls 为空）取 Inbox 列表
     * （effects.html 解析条目）-> 每封 selectEmail 拉详情正文（失败回退
     * 列表字段）。轮询响应快照 data.email 与请求邮箱不一致即会话被切换。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $email = trim($email);
        if ($email === '') {
            throw new \InvalidArgumentException('temp-mail-gg: 邮箱为空，请重新 Generate');
        }
        $token = trim($token ?? '');
        if (!str_starts_with($token, self::TOKEN_PREFIX)) {
            throw new \RuntimeException('temp-mail-gg: 凭据串前缀不符，请重新 Generate');
        }
        $sess = json_decode(substr($token, strlen(self::TOKEN_PREFIX)), true);
        if (!is_array($sess)) {
            throw new \RuntimeException('temp-mail-gg: 解析凭据串失败');
        }
        $sessEmail = trim((string) ($sess['email'] ?? ''));
        if ($sessEmail === '' || !is_array($sess['snapshot'] ?? null)) {
            throw new \RuntimeException('temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate');
        }
        if (strcasecmp($sessEmail, $email) !== 0) {
            throw new \RuntimeException('temp-mail-gg: 邮箱与凭据不匹配');
        }
        $csrf = trim((string) ($sess['csrf'] ?? ''));
        if ($csrf === '') {
            throw new \RuntimeException('temp-mail-gg: 凭据串缺失 csrf，请重新 Generate');
        }

        // 1) 轮询刷新（calls 为空 = 平台 20s 自动刷形态）
        $snapStr = (string) json_encode($sess['snapshot'], JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
        $respPoll = self::livewireUpdate($snapStr, $csrf, '', []);
        $components = $respPoll['components'] ?? [];
        if (!is_array($components) || !isset($components[0])) {
            throw new \RuntimeException('temp-mail-gg: 轮询响应异常（components 缺失）');
        }
        $c0 = $components[0];
        $latestSnap = $sess['snapshot'];
        if (trim((string) ($c0['snapshot'] ?? '')) !== '') {
            $snapArr = json_decode((string) $c0['snapshot'], true);
            if (is_array($snapArr)) {
                $current = trim((string) ($snapArr['data']['email'] ?? ''));
                if ($current !== '' && strcasecmp($current, $email) !== 0) {
                    throw new \RuntimeException('temp-mail-gg: 会话已被切换');
                }
                $latestSnap = $snapArr;
            }
        }
        $htmlBlock = trim((string) ($c0['effects']['html'] ?? ''));
        if ($htmlBlock === '') {
            throw new \RuntimeException('temp-mail-gg: 轮询响应无 effects.html，会话可能已失效');
        }

        // 2) 列表解析（条目容器 div wire:click="selectEmail(<数字id>)"）
        $rows = self::parseList($htmlBlock);
        if ($rows === []) {
            return [];
        }

        // 3) 逐封拉详情正文（selectEmail）；详情失败回退列表字段（不中断整批）
        $out = [];
        foreach ($rows as $row) {
            $raw = self::listToRaw($row, $email);
            if ($raw === null) {
                continue;
            }
            $idNum = (int) $row['id'];
            if ($idNum > 0) {
                $snapStr2 = (string) json_encode($latestSnap, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
                $respDetail = self::livewireUpdate($snapStr2, $csrf, 'selectEmail', [$idNum]);
                $detailComponents = $respDetail['components'] ?? [];
                if (is_array($detailComponents) && isset($detailComponents[0])) {
                    if (trim((string) ($detailComponents[0]['snapshot'] ?? '')) !== '') {
                        $detailSnap = json_decode((string) $detailComponents[0]['snapshot'], true);
                        if (is_array($detailSnap)) {
                            $latestSnap = $detailSnap;
                        }
                    }
                    $detailHtml = trim((string) ($detailComponents[0]['effects']['html'] ?? ''));
                    if ($detailHtml !== '') {
                        $detail = self::parseDetail($detailHtml);
                        if ($detail['from'] !== '') {
                            $raw['from'] = $detail['from'];
                        }
                        if ($detail['subject'] !== '') {
                            $raw['subject'] = $detail['subject'];
                        }
                        if ($detail['text'] !== '') {
                            $raw['text'] = $detail['text'];
                        }
                    }
                }
            }
            if ($raw['text'] === '') {
                $raw['text'] = $raw['subject'];
            }
            if ($raw['html'] === '') {
                $raw['html'] = '<html><body><pre>' . htmlspecialchars($raw['text'], ENT_QUOTES) . '</pre></body></html>';
            }
            $out[] = Normalize::email($raw, $email);
        }
        return $out;
    }

    /**
     * 从轮询响应 effects.html 解析收件箱条目。
     * 条目容器为 <div wire:click="selectEmail(<数字id>)">，容器内
     * h3(class 含 font-semibold)=发件人、p(class 含 text-zinc-300)=主题、
     * p(class 含 line-clamp-2)=预览、span(class 含 text-xs)=相对时间。
     *
     * @return array<int,array{id:string,from:string,subject:string,preview:string,when:string}>
     */
    private static function parseList(string $htmlBlock): array
    {
        $decoded = html_entity_decode($htmlBlock, ENT_QUOTES | ENT_HTML5);
        if (preg_match_all('/<div[^>]*wire:click="selectEmail\((\d+)\)"[^>]*>([\s\S]*?)<\/div>/i', $decoded, $items, PREG_SET_ORDER) === false) {
            return [];
        }
        $rows = [];
        foreach ($items as $item) {
            $rows[] = [
                'id' => $item[1],
                'from' => self::grabTag($item[2], 'h3', 'font-semibold'),
                'subject' => self::grabTag($item[2], 'p', 'text-zinc-300'),
                'preview' => self::grabTag($item[2], 'p', 'line-clamp-2'),
                'when' => self::grabTag($item[2], 'span', 'text-xs'),
            ];
        }
        return $rows;
    }

    /** 在片段内提取指定标签（class 属性含目标类名词）的首个文本内容 */
    private static function grabTag(string $fragment, string $tag, string $class): string
    {
        $re = '/<' . $tag . '[^>]*class="[^"]*\b' . preg_quote($class, '/') . '\b[^"]*"[^>]*>([\s\S]*?)<\/' . $tag . '>/i';
        if (preg_match($re, $fragment, $m) === 1) {
            return trim(html_entity_decode(strip_tags($m[1]), ENT_QUOTES | ENT_HTML5));
        }
        return '';
    }

    /**
     * 解析 selectEmail 详情视图（模态框）：h3(class 含 text-xl)=主题、
     * span 文本 "From:" 前缀=发件人、div x-show 含 activeTab === 'text'=正文。
     *
     * @return array{from:string,subject:string,text:string}
     */
    private static function parseDetail(string $htmlBlock): array
    {
        $decoded = html_entity_decode($htmlBlock, ENT_QUOTES | ENT_HTML5);
        $subject = self::grabTag($decoded, 'h3', 'text-xl');
        $from = '';
        if (preg_match_all('/<span[^>]*>([\s\S]*?)<\/span>/i', $decoded, $spans, PREG_SET_ORDER) !== false) {
            foreach ($spans as $span) {
                $t = trim(html_entity_decode(strip_tags($span[1]), ENT_QUOTES | ENT_HTML5));
                if (str_starts_with($t, 'From:')) {
                    $from = trim(substr($t, strlen('From:')));
                    break;
                }
            }
        }
        $text = '';
        if (preg_match('/<div[^>]*x-show="[^"]*activeTab\s*===\s*\'text\'[^"]*"[^>]*>([\s\S]*?)<\/div>/i', $decoded, $m) === 1) {
            $text = trim(html_entity_decode(strip_tags($m[1]), ENT_QUOTES | ENT_HTML5));
        }
        return ['from' => $from, 'subject' => $subject, 'text' => $text];
    }

    /**
     * 解析相对时间（"N seconds/minutes/hours/days ago"）为 ISO 8601；
     * 解析失败回退当前时间。
     */
    private static function relativeToDate(string $when): string
    {
        if (preg_match('/^\s*(\d+)\s+(second|minute|hour|day)s?\s+ago\s*$/i', trim($when), $m) === 1) {
            $units = ['second' => 1, 'minute' => 60, 'hour' => 3600, 'day' => 86400];
            return gmdate('c', time() - ((int) $m[1]) * $units[strtolower($m[2])]);
        }
        return gmdate('c', time());
    }

    /**
     * 列表行转归一化骨架（详情前的基础归一；text 暂用 preview）。
     *
     * @param array{id:string,from:string,subject:string,preview:string,when:string} $row
     * @return array<string,mixed>|null
     */
    private static function listToRaw(array $row, string $email): ?array
    {
        if ($row['id'] === '') {
            return null;
        }
        return [
            'id' => $row['id'],
            'from' => $row['from'],
            'to' => $email,
            'subject' => $row['subject'],
            'text' => $row['preview'],
            'html' => '',
            'date' => self::relativeToDate($row['when']),
        ];
    }
}