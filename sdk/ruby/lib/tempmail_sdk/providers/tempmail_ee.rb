# frozen_string_literal: true

require "base64"
require "cgi"
require "time"
require "uri"

module TempmailSdk
  module Providers
    # Tempmail.ee 渠道实现（tempmail.ee）
    #
    # 平台读信端 /api/mails 的 403 Access denied 是「会话 Cookie 绑定校验」：
    # temp_email 与 temp_mail_session 共同构成读信凭据，缺一即 403。
    # 只要在同一事务内完成 change → 提取 Set-Cookie → 读信，即可真实读到邮件。
    #
    # 已验证通行配方：
    #   - POST /api/mailbox/change 若不带 sec-ch-ua 头会被平台拒绝
    #     （403 Browser request required），带齐即 200，并下发会话 Cookie；
    #   - POST /api/mails 带完整 sec-ch-ua 三件套 + 会话 Cookie 即 200。
    # 本端无全局 Cookie 罐，会话凭据由本渠道以显式 Cookie 请求头逐请求携带。
    module TempmailEe
      CHANNEL = "tempmail-ee"
      BASE_URL = "https://tempmail.ee"

      # 与 sec-ch-ua 品牌版本一致的固定 UA（Chrome 154 / Linux）
      USER_AGENT = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " \
                   "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

      # Token 前缀，用于识别本渠道会话凭据串
      TOKEN_PREFIX = "tempmail-ee|"

      SEC_CH_HEADERS = {
        "Sec-Ch-Ua" => '"Chromium";v="154", "Google Chrome";v="154", "Not.A/Brand";v="99"',
        "Sec-Ch-Ua-Mobile" => "?0",
        "Sec-Ch-Ua-Platform" => '"Linux"'
      }.freeze

      # 建箱提交的浏览器指纹（与官方前端一致）
      BROWSER_INTEGRITY = {
        "webdriver" => false, "languagesMissing" => false, "languageMissing" => false,
        "pluginsMissing" => false, "pluginsUndefined" => false, "outerSizeMissing" => false,
        "innerSizeMissing" => false, "screenMissing" => false, "screenDepthMissing" => false,
        "timezoneMissing" => false, "timezoneOffsetMissing" => false,
        "userAgentDataPresent" => true, "userAgentMissing" => false,
        "platformClass" => "Linux", "mobile" => false, "collectionFailed" => false
      }.freeze

      module_function

      # 组装浏览器特征安全头（同站 fetch 全套）
      # @param with_sec_ch [Boolean] 是否携带 sec-ch-ua 三件套（change 必须）
      # @return [Hash]
      def browser_headers(with_sec_ch)
        hdrs = {
          "Accept" => "application/json",
          "Content-Type" => "application/json",
          "X-Requested-With" => "XMLHttpRequest",
          "Origin" => BASE_URL,
          "Referer" => "#{BASE_URL}/",
          "Sec-Fetch-Site" => "same-origin",
          "Sec-Fetch-Mode" => "cors",
          "Sec-Fetch-Dest" => "empty",
          "Accept-Language" => "en-US,en;q=0.9",
          "User-Agent" => USER_AGENT
        }
        hdrs.merge!(SEC_CH_HEADERS) if with_sec_ch
        hdrs
      end

      # 创建 tempmail.ee 临时邮箱
      # 事务会话：GET / 面熟首访 → POST /api/mailbox/change（sec-ch-ua 全套头）
      # → 提取 Set-Cookie 中的 temp_mail_session，随 token 透传给读信。
      # @return [EmailInfo]
      def generate_email
        # 步骤 1：GET / 建立会话面熟（响应 Cookie 不参与后续，凭据以 change 为准）
        begin
          Http.get(BASE_URL,
                   headers: {
                     "User-Agent" => USER_AGENT,
                     "Accept" => "text/html,application/xhtml+xml,application/xml;q=0.9," \
                                 "image/avif,image/webp,*/*;q=0.8"
                   },
                   timeout: 15)
        rescue StandardError
          nil
        end

        # 步骤 2：POST /api/mailbox/change 提交指纹换新邮箱（必须带 sec-ch-ua）
        body = { "turnstileToken" => nil, "browserIntegrity" => BROWSER_INTEGRITY }
        ch = Http.post("#{BASE_URL}/api/mailbox/change",
                       headers: browser_headers(true), json: body, timeout: 15)
        data = ch.json
        email = data.is_a?(Hash) && data["success"] ? data["newEmail"].to_s.strip : ""
        raise "tempmail-ee: 建箱响应缺少邮箱（status #{ch.status_code}）" if email.empty?

        # 步骤 3：从 Set-Cookie 接管会话凭据，供读信校验使用
        session = ch.cookie_value("temp_mail_session").to_s.strip
        raise "tempmail-ee: 建箱成功但未下发会话 Cookie，无法读信" if session.empty?

        expires_at = data["expiresAt"].to_s
        if expires_at.empty?
          expires_at = ((Time.now.to_f + 60 * 60) * 1000).to_i
        end
        EmailInfo.new(channel: CHANNEL, email: email,
                      token: "#{TOKEN_PREFIX}temp_email=#{email}; temp_mail_session=#{session}",
                      expires_at: expires_at)
      end

      # 解析读信凭据（防御 token 与 email 不一致，以请求邮箱为准重拼 Cookie）
      # @param token [String] Generate 时下发的渠道凭据串
      # @param email [String] 请求读取的邮箱
      # @return [String] 显式 Cookie 请求头（temp_email + temp_mail_session）
      def resolve_cookie(token, email)
        raise "tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱" unless token.to_s.start_with?(TOKEN_PREFIX)

        cred = token.to_s[TOKEN_PREFIX.length..]
        session = ""
        cred.split(";").each do |part|
          kv = part.strip
          session = kv[("temp_mail_session=".length)..] if kv.start_with?("temp_mail_session=")
        end
        raise "tempmail-ee: 会话凭据缺失或已失效，请重新 Generate 获取新邮箱" if session.empty?

        "temp_email=#{email}; temp_mail_session=#{session}"
      end

      # 读取 tempmail.ee 收件箱
      # POST /api/mails（显式会话 Cookie + sec-ch-ua）；
      # 列表只含元数据（id/fromAddress/toAddress/subject/createdAt/isRead），
      # 正文逐封 GET /api/mails/{id}，content 为 MIME multipart 原文（HTML 实体转义版）。
      # @param email [String] 邮箱地址
      # @param token [String] 渠道会话凭据串
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        raise "tempmail-ee: 邮箱为空" if addr.empty?
        raise "tempmail-ee: token 为空" if token.to_s.strip.empty?

        cookie = resolve_cookie(token, addr)
        hdrs = browser_headers(true).merge("Cookie" => cookie)

        resp = Http.post("#{BASE_URL}/api/mails",
                         headers: hdrs, json: { "email" => addr }, timeout: 15)
        if resp.status_code == 403
          raise "tempmail-ee inbox: http 403（会话 Cookie 校验失败，邮箱可能已过期，请重新 Generate）"
        end
        resp.raise_for_status
        data = resp.json
        mails = data.is_a?(Hash) ? data["mails"] : nil
        return [] unless mails.is_a?(Array)

        mails.filter_map do |row|
          next unless row.is_a?(Hash)

          mail = row_skeleton(row, addr)
          next if mail.nil?

          fetch_detail(mail, cookie)
          mail
        end
      end

      # 将列表行（仅元数据）转换为 Email 骨架；id 缺失视为无效行
      # @param row [Hash] 列表元素（id/fromAddress/toAddress/subject/createdAt/isRead）
      # @param email [String] 当前邮箱
      # @return [Email, nil]
      def row_skeleton(row, email)
        id = row["id"].to_s.strip
        return nil if id.empty?

        to_addr = row["toAddress"].to_s.strip
        to_addr = email if to_addr.empty?
        created = row["createdAt"].to_s.strip
        created = Time.now.utc.iso8601 if created.empty?
        Email.new(
          id: id,
          from_addr: row["fromAddress"].to_s.strip,
          to: to_addr,
          subject: row["subject"].to_s.strip,
          date: created,
          is_read: row["isRead"] == true || row["isRead"] == 1 || row["isRead"].to_s == "1"
        )
      end

      # 拉取单封详情并填充正文；失败不阻塞列表其余邮件
      # GET /api/mails/{id}（带显式会话 Cookie）
      # @param mail [Email] 列表骨架
      # @param cookie [String] 会话 Cookie 头
      def fetch_detail(mail, cookie)
        hdrs = browser_headers(false).merge("Cookie" => cookie)
        resp = Http.get("#{BASE_URL}/api/mails/#{URI.encode_www_form_component(mail.id)}",
                        headers: hdrs, timeout: 15)
        return unless resp.ok?

        detail = begin
          resp.json
        rescue StandardError
          return
        end
        return unless detail.is_a?(Hash)

        content = detail["content"].to_s
        if content.empty?
          # content 缺失时回退兜底候选键，避免平台字段演进后正文丢失
          content = detail["text"].to_s
          content = detail["body"].to_s if content.empty?
          content = detail["html"].to_s if content.empty?
          return if content.empty?
        end

        text, html = parse_content(content)
        mail.text = text if mail.text.empty?
        mail.html = html if mail.html.empty?
        mail.from_addr = detail["fromAddress"].to_s if mail.from_addr.empty?
        mail.subject = detail["subject"].to_s if mail.subject.empty?
      end

      # 解析详情 content（MIME multipart 原文，HTML 实体已转义）
      # 1) 反转义；2) 按首个 boundary 行切块解码；3) text/data-html 互为兜底合成。
      # @param raw [String] 原始 content 字符串
      # @return [Array(String, String)] [纯文本正文, HTML 正文]
      def parse_content(raw)
        payload = CGI.unescapeHTML(raw.to_s)
        payload = payload.gsub("&#61;", "=").gsub("&#43;", "+").gsub("&#47;", "/")
        payload = payload.gsub("\r\n", "\n")

        lines = payload.split("\n", -1)
        boundary = boundary_of(lines)
        text = ""
        html = ""
        if boundary.empty?
          text = singleton_part(payload)
        else
          text, html = parse_parts(lines, boundary)
        end
        if text.empty? && html.empty?
          text = singleton_part(payload)
        end
        if text.empty? && !html.empty?
          text = HtmlUtils.html_to_text(html)
        end
        if html.empty? && !text.empty?
          html = "<html><body><pre>#{HtmlUtils.escape(text)}</pre></body></html>"
        end
        [text, html]
      end

      # 扫描首块寻找 multipart 边界行（"--" 前缀）
      # @param lines [Array<String>]
      # @return [String] 边界串（无则空串）
      def boundary_of(lines)
        lines.first(120).each do |line|
          return line[2..].to_s.strip if line.start_with?("--") && line.length > 2
        end
        ""
      end

      # 按 boundary 拆分 multipart 并解码归并 text/html 两个 part
      # @param lines [Array<String>]
      # @param boundary [String] 边界串
      # @return [Array(String, String)] [纯文本正文, HTML 正文]
      def parse_parts(lines, boundary)
        text = ""
        html = ""
        i = 0
        while i < lines.length
          line = lines[i]
          unless line.start_with?("--#{boundary}")
            i += 1
            next
          end
          break if line.start_with?("--#{boundary}--")

          headers = {}
          j = i + 1
          # part 头部：直到首个空行
          while j < lines.length && lines[j] != "" && !lines[j].start_with?("--#{boundary}")
            ln = lines[j]
            if (k = ln.index(":")) && k.positive?
              headers[ln[0...k].strip.downcase] = ln[(k + 1)..].to_s.strip
            end
            j += 1
          end
          j += 1 if j < lines.length && lines[j] == ""
          # part 正文：到下一个边界行为止
          body = []
          while j < lines.length && !lines[j].start_with?("--#{boundary}")
            body << lines[j]
            j += 1
          end
          text, html = merge_part(body, headers, text, html)
          i = j
        end
        [text, html]
      end

      # 单个 part 的解码归并：按 content-type 归类，缺省回退 text 槽
      # @param body [Array<String>] part 正文行
      # @param headers [Hash] part 头部
      # @param text [String] 已归并纯文本
      # @param html [String] 已归并 HTML
      # @return [Array(String, String)]
      def merge_part(body, headers, text, html)
        ct = headers["content-type"].to_s.downcase.split(";").first
        cte = headers["content-transfer-encoding"].to_s.downcase
        content = decode_part(body.join("\n"), cte)
        if ct.include?("text/plain")
          text = content if text.empty?
        elsif ct.include?("text/html")
          html = content if html.empty?
        elsif text.empty?
          text = content
        end
        [text, html]
      end

      # 单 part（无边界或拆不出内容）降级解析：
      # 头部区隔（首个空行之后）取 body，并识别 Content-Transfer-Encoding 解码
      # @param payload [String]
      # @return [String]
      def singleton_part(payload)
        body = payload.strip
        return body unless body.include?("\n")

        lines = payload.split("\n", -1)
        # 头部以 RFC822 形式出现时，正文从首个空行后开始
        lines.each_with_index do |ln, idx|
          if ln.strip.empty?
            body = lines[(idx + 1)..].join("\n")
            break
          end
          break if idx > 40 || (idx >= 3 && !ln.include?(":"))
        end
        lower = payload.downcase
        cte = "base64" if lower.include?("base64")
        cte = "quoted-printable" if lower.include?("quoted-printable")
        decode_part(body, cte.to_s)
      end

      # 按 Content-Transfer-Encoding 解码 part 内容
      # @param data [String]
      # @param cte [String]
      # @return [String]
      def decode_part(data, cte)
        data = data.to_s.strip
        case cte
        when "base64"
          joined = data.gsub(/[\r\n\t ]/, "")
          begin
            Base64.strict_decode64(joined).strip.force_encoding("UTF-8")
          rescue ArgumentError
            data
          end
        when "quoted-printable"
          decode_quoted_printable(data)
        else
          data
        end
      end

      # 最小 quoted-printable 解码（=XX 十六进制转字节，行尾 = 软换行）
      # @param data [String]
      # @return [String]
      def decode_quoted_printable(data)
        out = data.gsub(/=\r?\n/, "")
        out = out.gsub(/=([0-9A-Fa-f]{2})/) { [$1].pack("H2") }
        out.force_encoding("UTF-8")
      rescue StandardError
        data
      end
    end
  end
end