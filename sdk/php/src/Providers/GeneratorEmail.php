<?php

declare(strict_types=1);

namespace ChanhanzhanX\TempMail\Providers;

use ChanhanzhanX\TempMail\Email;
use ChanhanzhanX\TempMail\EmailInfo;
use ChanhanzhanX\TempMail\HttpClient;
use ChanhanzhanX\TempMail\Normalize;
use GuzzleHttp\Cookie\CookieJar;

/**
 * GeneratorEmail 渠道实现（generator.email，PHP SSR 网页型）
 *
 * 调研实证结论（与 Go 端 generator_email.go 一致）：
 *   - 无独立建箱 API：服务器端渲染直接生成随机邮箱并写进页面内联
 *     window.SITE_DATA：cur_user:"hripynok"、cur_domain:"redproxies.com"
 *     （邮箱=user@domain），同页 Set-Cookie:
 *     inbox_ctx=redproxies.com%2Fhripynok（URL 编码）选中该邮箱会话。
 *   - 读信同为 SSR：GET /inbox4/ 带 inbox_ctx Cookie 返回该邮箱渲染页。
 *     信件列表渲染在 #email-table，每条为 class 含 list-group-item2 的
 *     行（平台实测真实类为 list-group-item2，非 list-group-item）内
 *     三个子 div：from_div_45g45gg（From）、subj_div_45g45gg（Subject）、
 *     time_div_45g45gg（Time (UTC)）。空箱时容器留空、num_mess=0。
 *   - 限制：本站邮件正文不提供纯文本/HTML 原文（默认渲染摘要），
 *     SDK 按列表三要素归一。
 *
 * Cookie 策略：模块内自管专属 CookieJar（本渠道独占），Generate 首页
 *   夺取 inbox_ctx，读信回带。
 * token 语义：{"email","domain","user"} JSON 快照。
 */
final class GeneratorEmail
{
    private const CHANNEL = 'generator-email';
    private const BASE_URL = 'https://generator.email';
    private const INBOX_URL = self::BASE_URL . '/inbox4/';

    private const USER_AGENT = 'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 '
        . '(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36';

    /* SITE_DATA 快照提取（cur_user / cur_domain） */
    private const USER_RE = '/cur_user:"([^"]*)"/';
    private const DOMAIN_RE = '/cur_domain:"([^"]*)"/';
    /* 列表条目与三要素块（平台实测真实类为 list-group-item2，按类名后缀锚定） */
    private const ITEM_RE = '/<div[^>]*class="[^"]*list-group-item2[^"]*"[^>]*>([\s\S]*?)(?:<\/div>\s*){3}/';
    private const FROM_RE = '/class="[^"]*from_div_45g45gg[^"]*"[^>]*>([\s\S]*?)<\/div>/';
    private const SUBJ_RE = '/class="[^"]*subj_div_45g45gg[^"]*"[^>]*>([\s\S]*?)<\/div>/';
    private const TIME_RE = '/class="[^"]*time_div_45g45gg[^"]*"[^>]*>([\s\S]*?)<\/div>/';
    private const SCRIPT_RE = '/<(script|style)[\s\S]*?<\/\1>/i';
    private const TAG_RE = '/<[^>]+>/';

    /** 模块级专属 Cookie 罐 */
    private static ?CookieJar $jar = null;

    /** 取得专属 Cookie 罐（首次调用时初始化） */
    private static function jar(): CookieJar
    {
        if (self::$jar === null) {
            self::$jar = new CookieJar();
        }
        return self::$jar;
    }

    /** 请求收件箱渲染页并返回 HTML（邮箱由服务端依据 inbox_ctx Cookie 选择） */
    private static function fetchPage(): string
    {
        $resp = HttpClient::get(self::INBOX_URL, [
            'User-Agent' => self::USER_AGENT,
            'Accept' => 'text/html,application/xhtml+xml,application/xml;q=0.9,'
                . 'image/avif,image/webp,*/*;q=0.8',
            'Accept-Language' => 'en-US,en;q=0.9',
            'Referer' => self::BASE_URL . '/',
        ], cookies: self::jar());
        if ($resp->getStatusCode() < 200 || $resp->getStatusCode() >= 300) {
            throw new \RuntimeException('generator-email: 请求失败 http ' . $resp->getStatusCode());
        }
        return (string) $resp->getBody();
    }

    /** 取正则第一个捕获组，无匹配返回空串 */
    private static function matchFirst(string $pattern, string $src): string
    {
        $m = preg_match($pattern, $src, $caps) === 1 ? $caps[1] : '';
        return is_string($m) ? $m : '';
    }

    /** 去标签与脚本/样式块，压缩空白 */
    private static function stripTags(string $s): string
    {
        $out = (string) preg_replace(self::SCRIPT_RE, ' ', $s);
        $out = (string) preg_replace(self::TAG_RE, ' ', $out);
        $out = (string) preg_replace('/\s+/', ' ', $out);
        return trim($out);
    }

    /**
     * 创建 generator.email 临时邮箱
     * 解析首页 SITE_DATA 快照（cur_user/cur_domain）得到邮箱地址。
     */
    public static function generate(): EmailInfo
    {
        $src = self::fetchPage();
        $user = self::matchFirst(self::USER_RE, $src);
        $domain = self::matchFirst(self::DOMAIN_RE, $src);
        if ($user === '' || $domain === '') {
            throw new \RuntimeException('generator-email: 首页未携带邮箱快照（cur_user/cur_domain）');
        }
        $email = $user . '@' . $domain;
        $tokenJson = (string) json_encode([
            'email' => $email,
            'domain' => $domain,
            'user' => $user,
        ], JSON_UNESCAPED_SLASHES);
        return new EmailInfo(self::CHANNEL, $email, $tokenJson);
    }

    /**
     * 获取 generator.email 收件箱
     * 解析收件箱渲染页的列表条目（from/subj/time 三要素）；本站不提供
     * 原文正文，SDK 按摘要归一。
     *
     * @return Email[]
     */
    public static function getEmails(string $email, ?string $token): array
    {
        $sess = json_decode((string) $token, true);
        if (!is_array($sess)) {
            throw new \InvalidArgumentException('generator-email: 会话凭据解析失败（非对象）');
        }
        if (($sess['email'] ?? '') !== $email) {
            throw new \InvalidArgumentException('generator-email: 会话邮箱与查询邮箱不匹配');
        }
        $sessDomain = (string) ($sess['domain'] ?? '');

        $src = self::fetchPage();
        // 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换
        $gotDomain = self::matchFirst(self::DOMAIN_RE, $src);
        if ($gotDomain !== $sessDomain) {
            throw new \RuntimeException(
                "generator-email: 会话域名已切换（token {$sessDomain}，服务端 {$gotDomain}）"
            );
        }

        // 列表区域锚定（#email-table ... #markodile 之间）
        $listStart = strpos($src, 'id="email-table"');
        $listEnd = strpos($src, 'id="markodile"');
        $region = ($listStart !== false && $listEnd !== false && $listEnd > $listStart)
            ? substr($src, $listStart, $listEnd - $listStart)
            : '';

        $out = [];
        if (preg_match_all(self::ITEM_RE, $src, $matches, PREG_SET_ORDER) === false) {
            return $out;
        }
        foreach ($matches as $m) {
            $raw = (string) ($m[1] ?? '');
            // 跳过列表容器外的候选：要求条目文本来自列表区域
            if ($region !== '' && strpos($region, $raw) === false) {
                continue;
            }
            $from = self::stripTags(self::matchFirst(self::FROM_RE, $raw));
            $subject = self::stripTags(self::matchFirst(self::SUBJ_RE, $raw));
            $when = self::stripTags(self::matchFirst(self::TIME_RE, $raw));
            if ($from === '' && $subject === '' && $when === '') {
                continue;
            }
            $out[] = Normalize::email([
                'from' => $from,
                'to' => $email,
                'subject' => $subject,
                'date' => $when,
            ], $email);
        }
        return $out;
    }
}