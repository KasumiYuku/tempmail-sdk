# frozen_string_literal: true

require "json"

module TempmailSdk
  module Providers
    # Internxt 渠道实现（internxt.com/temporary-email，Next.js + OpenNext）
    #
    # 流程：
    #   GET /temporary-email 夺取 XSRF-TOKEN Cookie（同气 csrfSecret）
    #   GET /api/temp-mail/create-email（头 csrf-token=<XSRF-TOKEN 值>）建箱
    #   GET /api/temp-mail/get-inbox?email=&token=（顶层数组）列信
    #   GET /api/temp-mail/get-message?email=&token=&messageId= 取单封全文
    # CSRF 要点：csrf-token 头的值必须是罐中 XSRF-TOKEN（每次 API 响应都会
    #   刷新该 Cookie，故每次请求前重取），不是 csrfSecret。
    # token 语义：{address, token} JSON。
    module Internxt
      CHANNEL = "internxt"
      SITE = "https://internxt.com"
      REF = "#{SITE}/temporary-email"
      API_BASE = "#{SITE}/api/temp-mail"

      USER_AGENT = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " \
                   "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

      # 简单查询编码（取值均为 URL 安全字符）
      REQUIRE_URI = true

      module_function

      # 收下响应 Set-Cookie：XSRF-TOKEN/csrfSecret 同名覆写模块级状态
      # @param resp [Http::Response]
      def merge_cookies(resp)
        resp.set_cookies.each do |line|
          kv = line.split(";").first.to_s.strip
          name, value = kv.split("=", 2)
          @xsrf = value if name == "XSRF-TOKEN" && value
          @secret = value if name == "csrfSecret" && value
        end
      end

      # 组装 Cookie 请求头（有则携带）
      # @return [String]
      def cookie_header
        parts = []
        parts << "csrfSecret=#{@secret}" if @secret && !@secret.empty?
        parts << "XSRF-TOKEN=#{@xsrf}" if @xsrf && !@xsrf.empty?
        parts.join("; ")
      end

      # 确保已取得 XSRF-TOKEN：无则 GET /temporary-email 夺取
      # @return [String] 罐中 XSRF-TOKEN 最新值
      def prepare_xsrf
        return @xsrf if @xsrf && !@xsrf.empty?

        resp = Http.get(REF, headers: {
          "User-Agent" => USER_AGENT,
          "Accept" => "text/html,application/xhtml+xml,application/xml;q=0.9," \
                      "image/avif,image/webp,*/*;q=0.8",
          "Accept-Language" => "en-US,en;q=0.9"
        }, timeout: 20)
        resp.raise_for_status
        merge_cookies(resp)
        raise "internxt: 未取得 XSRF-TOKEN Cookie" if @xsrf.to_s.empty?

        @xsrf
      end

      # 带 CSRF 头请求 internxt 数据接口（GET），返回解析后的 JSON。
      # csrf-token 头取罐中 XSRF-TOKEN 最新值（每次请求前重取，因每个 API
      # 响应都会刷新该 Cookie）。
      # @param path [String] 接口路径
      # @param params [Hash, nil] 查询参数
      # @return [Object]
      def api_get(path, params = nil)
        csrf = prepare_xsrf
        url = "#{API_BASE}#{path}"
        unless params.nil? || params.empty?
          qs = params.map { |k, v| "#{k}=#{v}" }.join("&")
          url = "#{url}?#{qs}"
        end
        headers = {
          "User-Agent" => USER_AGENT,
          "Accept" => "application/json, text/plain, */*",
          "Origin" => SITE,
          "Referer" => REF,
          "csrf-token" => csrf
        }
        cookie = cookie_header
        headers["Cookie"] = cookie unless cookie.empty?
        resp = Http.get(url, headers: headers, timeout: 20)
        merge_cookies(resp)
        resp.raise_for_status
        body = resp.body.to_s.strip
        body.empty? ? nil : JSON.parse(body)
      end

      # 创建 internxt.com 临时邮箱
      # 先确保已取得 XSRF-TOKEN，再 GET /api/temp-mail/create-email
      # （带 csrf-token 头），响应 {"address","token"}。
      # @return [EmailInfo]
      def generate_email
        prepare_xsrf
        data = api_get("/create-email")
        unless data.is_a?(Hash)
          raise "internxt: 解析建箱响应失败"
        end

        address = data["address"].to_s.strip
        api_token = data["token"].to_s.strip
        if address.empty? || api_token.empty? || !address.include?("@")
          raise "internxt: 创建邮箱响应缺少必要字段（address/token）"
        end

        token = JSON.generate({ address: address, token: api_token })
        EmailInfo.new(channel: CHANNEL, email: address, token: token)
      end

      # 从列表元素中提取邮件 ID（字符串形态，与 Go 端 messageIDOf 一致）
      # @param row [Hash]
      # @return [String]
      def message_id_of(row)
        %w[id messageId message_id].each do |key|
          val = row[key]
          return val.to_s.strip unless val.nil? || val.to_s.strip.empty?
        end
        ""
      end

      # 获取 internxt.com 收件箱
      # get-inbox 返回顶层数组（列表元素含 id/from/subject/date/seen 等）；
      # 逐条 get-message 拉单封全文（响应为单封对象，含 html 渲染全文），
      # 详情失败回退列表摘要。
      # @param email [String] 邮箱地址
      # @param token [String] 会话凭据 JSON（address/token）
      # @return [Array<Email>]
      def get_emails(email, token)
        begin
          sess = JSON.parse(token.to_s)
        rescue JSON::ParserError => e
          raise "internxt: 会话凭据解析失败: #{e.message}"
        end
        raise "internxt: 会话凭据解析失败（非对象）" unless sess.is_a?(Hash)

        address = sess["address"].to_s.strip
        api_token = sess["token"].to_s.strip
        raise "internxt: 会话凭据缺少必要字段" if address.empty? || api_token.empty?

        raise "internxt: 会话邮箱与查询邮箱不匹配" if address != email

        inbox = api_get("/get-inbox", email: address, token: api_token)
        raise "internxt: 解析收件箱响应失败" unless inbox.is_a?(Array)

        inbox.filter_map do |item|
          next unless item.is_a?(Hash)

          m = item.dup
          mid = message_id_of(m)
          unless mid.empty?
            begin
              detail = api_get("/get-message",
                               email: address, token: api_token, messageId: mid)
              if detail.is_a?(Hash)
                # 列表字段优先，详情仅补齐缺失字段
                detail.each { |k, v| m[k] = v unless m.key?(k) }
              end
            rescue StandardError
              # 详情失败回退列表摘要
            end
          end
          Normalize.normalize_email(m, email)
        end
      end
    end
  end
end