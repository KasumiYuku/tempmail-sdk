# frozen_string_literal: true

require "cgi"
require "time"
require "uri"

module TempmailSdk
  module Providers
    # Tempmailto 渠道实现（tempmailto.com，Laravel）
    #
    # 前端同款协议：GET / 首页建立 Cookie 会话并从 #mainEmail 的 value
    # 提取当前邮箱；POST /get_messages（_token=<CSRF>&captcha=）返回
    # {status, mailbox, email_token, messages, histories}；详情为站内
    # GET /view/{id}。邮箱约 10 分钟无活动过期，无独立密钥。
    #
    # 会话粘性：邮箱由会话 Cookie 承载，Token 约定为邮箱本身（注册表
    # 以 token 非空作防御）。读到的当前邮箱与请求邮箱不一致时，用
    # POST /change（_token + name + domain）把会话拉回请求邮箱。
    # 本端无全局 Cookie 罐，会话 Cookie 由本渠道模块级维护并逐请求
    # 以显式 Cookie 头回填。
    module Tempmailto
      CHANNEL = "tempmailto"
      BASE_URL = "https://tempmailto.com"

      USER_AGENT = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " \
                   "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

      HOME_ACCEPT = "text/html,application/xhtml+xml,application/xml;q=0.9," \
                    "image/avif,image/webp,*/*;q=0.8"
      XHR_ACCEPT = "application/json, text/plain, */*"

      # 模块级会话 Cookie 状态：site => 值，逐请求回填 Cookie 头
      @cookies = {}

      CSRF_RE = /<meta\s+name="csrf-token"\s+content="([^"]+)"/
      MAIN_EMAIL_RE = /id="mainEmail"[^>]*\bvalue="([^"]+)"/im

      # 详情正文候选 class（按平台视图页结构依次尝试，后回退 main/article）
      DETAIL_CLASSES = %w[mail-body mail_content email-body content-body
                          message-content mail-content].freeze

      module_function

      # 解析 Set-Cookie 头，覆写合并进模块级会话 Cookie 状态
      # @param resp [Http::Response]
      def store_cookies(resp)
        resp.set_cookies.each do |line|
          kv = line.split(";").first.to_s.strip
          next if kv.empty?

          k, v = kv.split("=", 2)
          @cookies[k.strip] = v.to_s if k && !k.strip.empty?
        end
      end

      # 由模块级 Cookie 状态拼出显式 Cookie 请求头
      # @return [String, nil] 无 Cookie 时为 nil
      def cookie_header
        return nil if @cookies.empty?

        @cookies.map { |k, v| "#{k}=#{v}" }.join("; ")
      end

      # 组装浏览器特征请求头（同站调用，照前端实际携带的头）
      # @param accept [String] Accept 头取值
      # @param with_cookie [Boolean] 是否回填会话 Cookie
      # @return [Hash]
      def browser_headers(accept, with_cookie: true)
        hdrs = {
          "User-Agent" => USER_AGENT,
          "Accept" => accept,
          "Accept-Language" => "en-US,en;q=0.9",
          "Origin" => BASE_URL,
          "Referer" => "#{BASE_URL}/"
        }
        hdrs["Cookie"] = cookie_header if with_cookie
        hdrs.compact
      end

      # 组装 /get_messages、/change 的 POST 请求头
      # @return [Hash]
      def post_headers(accept)
        hdrs = browser_headers(accept)
        hdrs["Content-Type"] = "application/x-www-form-urlencoded; charset=UTF-8"
        hdrs["X-Requested-With"] = "XMLHttpRequest"
        hdrs
      end

      # 创建 tempmailto.com 临时邮箱
      # GET 首页建立会话（响应 Cookie 落入模块级状态）并提取服务端渲染
      # 的当前邮箱；Token 约定为邮箱本身。邮箱约 10 分钟无活动过期。
      # @return [EmailInfo]
      def generate_email
        resp = Http.get(BASE_URL, headers: browser_headers(HOME_ACCEPT, with_cookie: true), timeout: 15)
        unless resp.ok?
          raise "tempmailto: 首页 http #{resp.status_code}"
        end

        store_cookies(resp)
        email = ""
        if (m = resp.body.match(MAIN_EMAIL_RE))
          email = m[1].strip
        end
        if email.empty?
          raise "tempmailto: 首页未渲染出邮箱（mainEmail 缺失），无法建箱"
        end

        EmailInfo.new(channel: CHANNEL, email: email, token: email)
      end

      # 读取 tempmailto.com 当前邮箱的收件箱
      # 邮箱与请求邮箱不一致时用 change 拉回，再重新读 messages。
      # @param email [String] 邮箱地址
      # @param token [String] 渠道会话凭据串（Generate 时约定的邮箱本身）
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        if addr.empty?
          raise "tempmailto: 邮箱为空，请重新 Generate"
        end

        csrf = fetch_csrf
        data = fetch_messages(csrf)
        mailbox = data["mailbox"].to_s.strip

        # 会话粘性：当前邮箱与请求目标不一致 -> change 拉回
        if !mailbox.empty? && mailbox.casecmp(addr) != 0
          changed = change_mailbox(addr)
          if changed.empty?
            raise "tempmailto: change 响应异常"
          end
          if changed.casecmp(addr) != 0
            raise "tempmailto: 会话邮箱无法拉回请求邮箱"
          end
          data = fetch_messages(fetch_csrf)
        end

        messages = data["messages"]
        return [] unless messages.is_a?(Array)

        messages.filter_map do |row|
          next unless row.is_a?(Hash)

          build_email(row, addr)
        end
      end

      # GET 首页并提取 CSRF _token（读信/换箱共用，调用同时刷新会话 Cookie）
      # @return [String]
      def fetch_csrf
        resp = Http.get(BASE_URL, headers: browser_headers(HOME_ACCEPT), timeout: 15)
        store_cookies(resp)
        m = resp.body.match(CSRF_RE)
        raise "tempmailto: 首页未找到 csrf-token" unless m

        m[1]
      end

      # POST /get_messages 拉取当前邮箱的消息 JSON
      # @param csrf [String] _token 取值
      # @return [Hash]
      def fetch_messages(csrf)
        resp = Http.post("#{BASE_URL}/get_messages",
                         headers: post_headers(XHR_ACCEPT),
                         body: "_token=#{csrf}&captcha=", timeout: 15)
        unless resp.ok?
          raise "tempmailto 读信: http #{resp.status_code}"
        end
        store_cookies(resp)

        data = resp.json
        raise "tempmailto: 读信响应非对象" unless data.is_a?(Hash)

        data
      end

      # POST /change 换箱：以请求邮箱的 @ 前部/后部作为 name/domain 拉回会话
      # @param email [String] 目标邮箱
      # @return [String] 变更后的当前邮箱（响应 mailbox）
      def change_mailbox(email)
        name = email.split("@", 2).first.to_s
        name = "TmSdk" if name.empty?
        domain = email.split("@", 2)[1].to_s
        domain = "tempmailto.com" if domain.empty?

        csrf = fetch_csrf
        resp = Http.post("#{BASE_URL}/change",
                         headers: post_headers(XHR_ACCEPT),
                         body: "_token=#{csrf}&name=#{CGI.escape(name)}&domain=#{CGI.escape(domain)}",
                         timeout: 15)
        store_cookies(resp)
        data = resp.json
        return "" unless data.is_a?(Hash)

        data["mailbox"].to_s.strip
      rescue StandardError
        ""
      end

      # 将 messages 列表元素组装为统一邮件：
      # 详情页 /view/{id} 提取正文，失败回退列表字段；id 空跳过。
      # @param row [Hash] 列表元素
      # @param email [String] 收件人地址
      # @return [Email, nil]
      def build_email(row, email)
        id = str_of(row["id"])
        return nil if id.empty?

        html = view_detail(id)
        text = html.empty? ? "" : HtmlUtils.html_to_text(html)
        if text.empty?
          text = str_of(row["body"])
          text = str_of(row["text"]) if text.empty?
          text = str_of(row["snippet"]) if text.empty?
          text = str_of(row["preview"]) if text.empty?
        end
        subject = str_of(row["subject"])
        text = subject if text.empty?
        html = "<html><body><pre>#{HtmlUtils.escape(text)}</pre></body></html>" if html.empty?
        date = str_of(row["receivedAt"])
        date = str_of(row["received_at"]) if date.empty?
        date = str_of(row["createdAt"]) if date.empty?
        date = Time.now.utc.iso8601 if date.empty?

        Normalize.normalize_email(normalize_item(row, email, text, html, date), email)
      end

      # 组装 normalize_email 的入参 Hash（键按本端标准化候选键选取）
      # @param row [Hash] 列表元素
      # @param email [String] 收件人地址
      # @param text [String] 纯文本正文
      # @param html [String] HTML 正文
      # @param date [String] 时间
      # @return [Hash]
      def normalize_item(row, email, text, html, date)
        from_email = str_of(row["from_email"])
        from_email = str_of(row["from"]) if from_email.empty?
        from_email = str_of(row["from_name"]) if from_email.empty?

        {
          "id" => str_of(row["id"]),
          "from_email" => from_email,
          "to" => email,
          "subject" => str_of(row["subject"]),
          "text" => text,
          "html" => html,
          "receivedAt" => date,
          "is_seen" => read_of(row["is_seen"])
        }
      end

      # GET /view/{id} 提取邮件正文 HTML（同会话 Cookie，失败返回空串）
      # @param id [String] 邮件 id
      # @return [String]
      def view_detail(id)
        resp = Http.get("#{BASE_URL}/view/#{URI.encode_www_form_component(id)}",
                        headers: browser_headers(HOME_ACCEPT), timeout: 15)
        return "" unless resp.ok?

        store_cookies(resp)
        extract_detail(resp.body)
      rescue StandardError
        ""
      end

      # 按候选 class 依次提取详情区块，全失败回退 <main>/<article>
      # @param page [String] 详情页 HTML
      # @return [String]
      def extract_detail(page)
        DETAIL_CLASSES.each do |klass|
          re = %r{<[^>]+class="[^"]*\b#{Regexp.escape(klass)}\b[^"]*"[^>]*>(.*?)</(?:div|section|article)>}mi
          m = page.match(re)
          inner = m && m[1].to_s.strip
          return inner unless inner.nil? || inner.empty?
        end
        if (m = page.match(%r{<(main|article)[^>]*>(.*?)</\1>}mi))
          inner = m[2].to_s.strip
          return inner unless inner.empty?
        end
        ""
      end

      # 将接口字段值安全转换为字符串，nil 或非标量返回空串
      # @param v [Object]
      # @return [String]
      def str_of(v)
        case v
        when String then v
        when Numeric then v.to_i.to_s
        when true then "true"
        when false then "false"
        else ""
        end
      end

      # 将 is_seen 归一为布尔已读标记，兼容 bool / 数字(0|1) / string("true"|"1")
      # @param v [Object]
      # @return [Boolean]
      def read_of(v)
        case v
        when true then true
        when false then false
        when Numeric then v != 0
        when String
          s = v.strip
          s.casecmp("true").zero? || s == "1"
        else false
        end
      end
    end
  end
end