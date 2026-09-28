# frozen_string_literal: true

require "base64"
require "time"
require "uri"

module TempmailSdk
  module Providers
    # NoxenDe5Net 渠道实现（UniMail-Bot 公共实例 tempmail.noxen.de5.net）
    #
    # 完整接入契约：
    #   登录 POST /api/login {"username":"guest","password":"123456"}
    #     → 200 {"success":true,"role":"guest"} 并 Set-Cookie: iding-session=<JWT>；
    #   建箱 GET /api/generate → 200 {"email":"随机@域名","expires":毫秒时间戳}；
    #   读信 GET /api/emails?mailbox=<地址>&limit=20 → 200 邮件数组（无邮件为 []）；
    #   详情 GET /api/email/{id} → {..., content, html_content, to_addrs, r2_bucket,
    #     r2_object_key, download}；content/html_content 平台恒为空，原始 EML 存
    #     Cloudflare R2，download 指向 GET /api/email/{id}/download 下载端点。
    # 鉴权边界：读信不带会话 Cookie 返回 401；访客邮箱只能查自己的 mailbox。
    # 会话隔离：先 GET /api/session 兜底校验 cookie，未通过则重新 login；
    #   凭据串只由会话 Cookie 与同源地址构成，读信时逐请求显式携带。
    module NoxenDe5Net
      BASE_URL = "https://tempmail.noxen.de5.net"
      USER = "guest"
      PASS = "123456"
      # 本渠道凭据串前缀
      TOKEN_PREFIX = "noxen-de5-net|"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " \
                        "(KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      module_function

      # 登录取得会话 Cookie（iding-session=JWT）
      # @return [String] "iding-session=<JWT>"
      def session_cookie
        headers = {
          "Content-Type" => "application/json",
          "Accept" => "application/json",
          "User-Agent" => HEADERS["User-Agent"]
        }
        resp = Http.post("#{BASE_URL}/api/login",
                         headers: headers,
                         json: { "username" => USER, "password" => PASS },
                         timeout: 15)
        raise "noxen-de5-net login: http #{resp.status_code}" unless resp.ok?

        data = resp.json
        raise "noxen-de5-net login: 登录失败" unless data.is_a?(Hash) && data["success"] == true

        session = resp.cookie_value("iding-session").to_s.strip
        raise "noxen-de5-net login: 未下发会话 Cookie" if session.empty?

        "iding-session=#{session}"
      end

      # 校验会话 Cookie 是否仍有效（GET /api/session）
      # @param cookie [String]
      # @return [Boolean]
      def cookie_still_valid(cookie)
        resp = Http.get("#{BASE_URL}/api/session",
                        headers: HEADERS.merge("Cookie" => cookie), timeout: 15)
        return false unless resp.ok?

        data = resp.json
        data.is_a?(Hash) && data["authenticated"] == true
      rescue StandardError
        false
      end

      # 校验域名是否在平台域名池内
      # @param domain [String]
      # @return [Boolean]
      def domain_in_pool(domain)
        resp = Http.get("#{BASE_URL}/api/domains", headers: HEADERS, timeout: 15)
        return false unless resp.ok?

        pool = resp.json
        return false unless pool.is_a?(Array)

        pool.any? { |d| d.to_s.casecmp(domain).zero? }
      rescue StandardError
        false
      end

      # 登录并创建临时邮箱
      # @param domain [String, nil] 可选首选域名（空时平台自动选域）
      # token 凭据串格式："noxen-de5-net|<iding-session=JWT>|base=<基址>"
      # @return [EmailInfo]
      def generate_email(domain = nil)
        want = domain.to_s.strip
        if !want.empty? && !domain_in_pool(want)
          raise "noxen-de5-net generate: 域名 #{want} 不在平台域名池"
        end

        cookie = session_cookie

        resp = Http.get("#{BASE_URL}/api/generate",
                        headers: HEADERS.merge("Cookie" => cookie), timeout: 15)
        raise "noxen-de5-net generate: http #{resp.status_code}" unless resp.ok?

        data = resp.json
        email = data.is_a?(Hash) ? data["email"].to_s.strip : ""
        raise "noxen-de5-net generate: 响应缺少 email" if email.empty?

        expires = data.is_a?(Hash) ? data["expires"].to_i : 0
        expires_at = expires.positive? ? Time.at(expires / 1000).utc.iso8601 : nil
        token = TOKEN_PREFIX + URI.encode_www_form_component(cookie) + "|base=" + BASE_URL
        EmailInfo.new(channel: "noxen-de5-net", email: email, token: token, expires_at: expires_at)
      end

      # 从凭据串解析会话 Cookie
      # @param token [String] 建箱下发的凭据串
      # @return [String] "iding-session=<JWT>"
      def cookie_from_token(token)
        raise "noxen-de5-net: token 格式错误" unless token.to_s.start_with?(TOKEN_PREFIX)

        enc = token.to_s[TOKEN_PREFIX.length..].to_s
        enc = enc.sub(/\|base=#{Regexp.escape(BASE_URL)}\z/, "")
        URI.decode_www_form_component(enc)
      end

      # 读取收件箱
      # 正文获取优先级：
      # 1) 详情（download 定位）+ 下载端点拉取原始 EML，本地拆分 text/plain 与 text/html；
      # 2) 详情/EML 链路不可得时，用 verification_code 置顶 + preview 合成占位正文。
      # @param email [String] 信箱地址（与凭据同源）
      # @param token [String] 建箱下发的凭据串
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        cookie = cookie_from_token(token)
        cookie = session_cookie unless cookie_still_valid(cookie)

        uri = "#{BASE_URL}/api/emails?mailbox=#{URI.encode_www_form_component(addr)}&limit=20"
        resp = Http.get(uri, headers: HEADERS.merge("Cookie" => cookie), timeout: 15)
        if resp.status_code == 401
          raise "noxen-de5-net 读信: http 401（会话失效或非本会话邮箱）"
        end
        raise "noxen-de5-net 读信: http #{resp.status_code}" unless resp.ok?

        list = resp.json
        raise "noxen-de5-net 读信: 收件箱响应非数组" unless list.is_a?(Array)

        list.filter_map do |m|
          next unless m.is_a?(Hash)

          flat = m.dup
          flat["from"] = m["sender"]
          flat["to"] = addr
          flat["date"] = m["received_at"]
          flat["text"] = m["preview"]
          flat["isRead"] = m["is_read"]
          full = false
          id = m["id"].to_s.strip
          if !id.empty? && id != "0"
            detail = fetch_detail(cookie, id)
            if detail.is_a?(Hash)
              flat["content"] = detail["content"]
              flat["html_content"] = detail["html_content"]
              flat["to_addrs"] = detail["to_addrs"]
              flat["r2_bucket"] = detail["r2_bucket"]
              flat["r2_object_key"] = detail["r2_object_key"]
              dl = detail["download"].to_s
              unless dl.empty?
                eml = fetch_eml(cookie, dl)
                unless eml.empty?
                  text, html = parse_eml(eml)
                  if !text.empty? || !html.empty?
                    flat["text"] = text
                    flat["html"] = html
                    full = true
                  end
                end
              end
            end
            flat["text"] = compose_placeholder(m) unless full
          end
          Normalize.normalize_email(flat, addr)
        end
      end

      # 拉取单封邮件详情（GET /api/email/{id}）
      # @param cookie [String]
      # @param id [String]
      # @return [Hash, nil]
      def fetch_detail(cookie, id)
        uri = "#{BASE_URL}/api/email/#{URI.encode_www_form_component(id)}"
        resp = Http.get(uri, headers: HEADERS.merge("Cookie" => cookie), timeout: 15)
        return nil unless resp.ok?

        resp.json
      rescue StandardError
        nil
      end

      # 拉取原始 EML 报文（详情 download 字段指向的下载端点）
      # @param cookie [String]
      # @param dl_path [String] 下载相对路径（如 /api/email/3880/download）
      # @return [String] 原始报文文本
      def fetch_eml(cookie, dl_path)
        u = dl_path.to_s
        unless u.start_with?("http://", "https://")
          u = BASE_URL + u
        end
        resp = Http.get(u,
                        headers: {
                          "Accept" => "message/rfc822, */*",
                          "User-Agent" => HEADERS["User-Agent"],
                          "Cookie" => cookie
                        },
                        timeout: 20)
        return "" unless resp.ok?

        resp.body.to_s
      rescue StandardError
        ""
      end

      # 全文不可得时的合成占位正文：verification_code 置顶，preview 附后。
      # @param m [Hash] 列表元素 map（verification_code/preview）
      # @return [String]
      def compose_placeholder(m)
        code = m["verification_code"].to_s.strip
        preview = m["preview"].to_s.strip
        parts = []
        parts << "验证码: #{code}" unless code.empty?
        parts << preview unless preview.empty?
        parts.join("\n\n")
      end

      # 解析 EML 原始报文 → [纯文本正文, HTML 正文]
      # @param eml [String] 原始报文
      # @return [Array(String, String)]
      def parse_eml(eml)
        payload = eml.to_s.gsub("\r\n", "\n").gsub("\r", "")
        headers, body = split_eml(payload, 0)
        parse_entity(headers, body)
      end

      # 将原始报文切分为首部 map 与正文块
      # @param payload [String] 已做 CRLF→LF 归一化的原始报文
      # @param offset [Integer] 起始行号（外包 "#participant"+part 时置 1）
      # @return [Array(Hash, String)]
      def split_eml(payload, offset)
        lines = payload.split("\n", -1)
        headers = {}
        i = offset
        i += 1 if i < lines.length && lines[i].start_with?("From ")
        cur_key = ""
        while i < lines.length
          line = lines[i]
          break if line == ""

          # 折行续行（RFC 5322）：以空白开头且已有当前头键时拼接到上一条
          if (line[0] == " " || line[0] == "\t") && !cur_key.empty?
            headers[cur_key] = "#{headers[cur_key]} #{line.strip}"
            i += 1
            next
          end
          if (k = line.index(":")) && k.positive?
            cur_key = line[0...k].strip.downcase
            headers[cur_key] = line[(k + 1)..].to_s.strip
          end
          i += 1
        end
        i += 1 if i < lines.length
        [headers, lines[i..].to_a.join("\n")]
      end

      # 从 Content-Type 头值提取 multipart boundary（引号可选）
      # @param ct_raw [String]
      # @return [String]
      def extract_boundary(ct_raw)
        m = /boundary="?([^";\s]+)"?/i.match(ct_raw.to_s)
        m ? m[1] : ""
      end

      # 按 boundary 切出各 part（含各自首部行）
      # @param body [String]
      # @param boundary [String]
      # @return [Array<String>]
      def split_multipart(body, boundary)
        segments = body.split("--#{boundary}")
        segments.filter_map do |seg|
          seg = seg.sub(/\A\n/, "")
          seg = seg.sub(/--\n\z/, "").sub(/--\z/, "")
          seg.empty? ? nil : seg
        end
      end

      # 递归解析单个 MIME 实体（上游 parseEntity 同构）
      # @param headers [Hash]
      # @param body [String]
      # @return [Array(String, String)]
      def parse_entity(headers, body)
        ct = headers["content-type"].to_s.downcase
        cte = headers["content-transfer-encoding"].to_s.downcase

        # 单体：text/html 或 text/plain（含无 Content-Type 时按纯文本处理）
        unless ct.start_with?("multipart/")
          decoded = decode_part(body, cte)
          return ct.include?("text/html") ? ["", decoded] : [decoded, ""]
        end

        # 复合：递归拆分，text 槽与 html 槽各自取第一个非空命中
        text = ""
        html = ""
        boundary = extract_boundary(headers["content-type"].to_s)
        unless boundary.empty?
          split_multipart(body, boundary).each do |part|
            ph, pb = split_eml("#participant\n#{part}", 1)
            pct = ph["content-type"].to_s.downcase
            if pct.start_with?("multipart/")
              t, h = parse_entity(ph, pb)
              text = t if text.empty?
              html = h if html.empty?
            elsif pct.start_with?("message/rfc822")
              nh, nb = split_eml(pb, 0)
              t, h = parse_entity(nh, nb)
              text = t if text.empty?
              html = h if html.empty?
            elsif pct.include?("rfc822-headers")
              # 纯头部 part 跳过，正文在后续 part 中抓取
              next
            else
              t, h = parse_entity(ph, pb)
              text = t if text.empty?
              html = h if html.empty?
            end
            break if !text.empty? && !html.empty?
          end
        end
        # 无 HTML 命中时从整体原文兜底抓取 HTML 片段（上游 guessHtmlFromRaw 同构）
        html = guess_html(body) if html.empty?
        [text, html]
      end

      # 按 Content-Transfer-Encoding 解码 part 内容
      # @param data [String]
      # @param cte [String]
      # @return [String]
      def decode_part(data, cte)
        case cte.to_s.strip.downcase
        when "base64"
          joined = data.gsub(/[\n\r\t ]/, "")
          begin
            Base64.strict_decode64(joined).strip.force_encoding("UTF-8")
          rescue ArgumentError
            data
          end
        when "quoted-printable"
          decode_quoted_printable(data)
        else
          # 7bit/8bit/binary：原样返回
          data.strip
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

      # 从整体原文中抓取 <html>…</html> 片段（上游 guessHtmlFromRaw 同构）
      # @param body [String]
      # @return [String]
      def guess_html(body)
        return "" if body.to_s.empty?

        lower = body.downcase
        hs = lower.index("<html")
        hs = lower.index("<!doctype html") if hs.nil?
        return "" if hs.nil?

        he = lower.rindex("</html>")
        return "" if he.nil? || he < hs

        body[hs...(he + 7)]
      end
    end
  end
end