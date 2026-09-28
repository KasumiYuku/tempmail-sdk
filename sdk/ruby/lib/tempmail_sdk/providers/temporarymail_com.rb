# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # Temporarymail 渠道实现（temporarymail.com）
    #
    # 无认证 REST（key 为空即随机建箱）：
    #   GET /api/?action=requestEmailAccess&key=&value=random 建箱，
    #   响应 {"address":"...","secretKey":"..."}，secretKey 用于后续 checkInbox。
    # 读信 GET /api/?action=checkInbox&value=<secretKey>，响应有两种形态：
    #   空收件箱为 []，有信时为 map[id]→邮件元数据对象。
    # 详情 POST /api/?action=getEmail&value=<id> 覆盖真实主题（列表常为 "[No Subject]"）；
    # 全文 GET /view/?i=<id> 返回 HTML 化网页，本地剥标签还原纯文本。
    # 地址最长周期固定为 4 小时。
    module TemporarymailCom
      CHANNEL = "temporarymail-com"
      BASE_URL = "https://temporarymail.com"

      # 备用浏览器 UA（403 重试用，规避共享池随机 UA 耗尽）
      ALT_UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
               "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

      HEADERS = {
        "Accept" => "application/json, text/plain, */*",
        "Accept-Language" => "en-US,en;q=0.9",
        "Sec-Fetch-Site" => "same-origin",
        "Sec-Fetch-Mode" => "cors",
        "Sec-Fetch-Dest" => "empty",
        "Referer" => "#{BASE_URL}/",
        "Origin" => BASE_URL,
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      module_function

      # 构造 /api/ 请求头（403 时换备用 UA 重试）
      # @param ua [String] User-Agent
      # @return [Hash]
      def api_headers(ua)
        HEADERS.merge("User-Agent" => ua)
      end

      # 创建 temporarymail.com 临时邮箱
      # GET /api/?action=requestEmailAccess&key=&value=random；token 复用 secretKey。
      # @return [EmailInfo]
      def generate_email
        resp = Http.get("#{BASE_URL}/api/?action=requestEmailAccess&key=&value=random",
                        headers: api_headers(HEADERS["User-Agent"]), timeout: 15)
        if resp.status_code == 429
          raise "temporarymail: 创建邮箱平台限流(429)，请稍后重试"
        end
        resp.raise_for_status
        data = resp.json
        raise "temporarymail: 创建响应非对象" unless data.is_a?(Hash)

        addr = data["address"].to_s.strip
        key = data["secretKey"].to_s.strip
        raise "temporarymail: 创建响应缺少 address 或 secretKey" if addr.empty? || key.empty?

        EmailInfo.new(channel: CHANNEL, email: addr, token: key)
      end

      # 解析平台双形态响应（[] 或 map[id]→对象）为数组
      # @param data [Object] JSON 解析结果
      # @return [Array<Hash>, nil]
      def list_of(data)
        return data if data.is_a?(Array)

        data.values if data.is_a?(Hash)
      end

      # 读取 temporarymail 收件箱
      # 列表主题常为 "[No Subject]"：逐封拉详情覆盖真实主题，并逐封抓 /view/ 全文。
      # @param email [String] 邮箱地址
      # @param token [String] 建箱返回的 secretKey
      # @return [Array<Email>]
      def get_emails(email, token)
        tk = token.to_s.strip
        raise "temporarymail: token 为空" if tk.empty?

        body = check_inbox(tk)
        list = list_of(body)
        return [] unless list.is_a?(Array)

        addr = email.to_s.strip
        list.filter_map do |m|
          next unless m.is_a?(Hash)

          row = m.dup
          row["to"] = addr unless row.key?("to")
          id = row["id"].to_s.strip
          unless id.empty?
            # 详情覆盖真实主题/发件人（失败不致命：列表元数据兜底）
            det = fetch_detail(id)
            if det.is_a?(Hash)
              row["subject"] = det["subject"] if det["subject"].to_s.strip != ""
              row["from"] = det["from"] if det["from"].to_s.strip != ""
            end
            # /view/ 渲染端点全文（失败不致命：列表元数据兜底）
            text = fetch_view(id)
            row["text"] = text if !text.empty?
          end
          Normalize.normalize_email(row, addr)
        end
      end

      # 拉取 checkInbox 响应体；403 换备用 UA 重试一次，429 报平台限流。
      # @param token [String] secretKey
      # @return [Object] JSON 解析结果
      def check_inbox(token)
        uas = [HEADERS["User-Agent"], ALT_UA]
        last_status = 0
        last_raw = ""
        uas.each do |ua|
          uri = "#{BASE_URL}/api/?action=checkInbox&value=#{URI.encode_www_form_component(token)}"
          resp = Http.get(uri, headers: api_headers(ua), timeout: 15)
          if resp.status_code == 429
            raise "temporarymail: 读取收件箱平台限流(429)，请拉大轮询间隔"
          end
          if resp.ok?
            return resp.json
          end
          last_status = resp.status_code
          last_raw = resp.body.to_s
          # 403/404 疑似 UA 键控风控，换备用 UA 重试一次
          next if [403, 404].include?(resp.status_code)

          raise "temporarymail: 读取收件箱失败 http #{resp.status_code}: #{last_raw}"
        end
        raise "temporarymail: 读取收件箱失败 http #{last_status}（两次尝试均被拒）"
      end

      # 拉取单封详情（POST /api/?action=getEmail&value=<id>），风控失败返回 nil 降级
      # @param id [String] 邮件 ID
      # @return [Hash, nil]
      def fetch_detail(id)
        uri = "#{BASE_URL}/api/?action=getEmail&value=#{URI.encode_www_form_component(id)}"
        resp = Http.post(uri, headers: api_headers(HEADERS["User-Agent"]), timeout: 15)
        return nil if resp.status_code == 429 || !resp.ok?

        data = resp.json
        return nil unless data.is_a?(Hash)

        data.values.first
      rescue StandardError
        nil
      end

      # 抓取 /view/ 渲染端点全文并还原纯文本
      # @param id [String] 邮件 ID
      # @return [String]
      def fetch_view(id)
        uri = "#{BASE_URL}/view/?i=#{URI.encode_www_form_component(id)}&width=800"
        resp = Http.get(uri,
                        headers: {
                          "Accept" => "text/html, */*",
                          "User-Agent" => HEADERS["User-Agent"],
                          "Referer" => "#{BASE_URL}/"
                        },
                        timeout: 15)
        return "" unless resp.ok?

        view_to_text(resp.body.to_s)
      end

      # 将 /view/ 响应剥标签还原为纯文本（<br>/<p> 换行保留）
      # @param src [String] HTML 化网页原文
      # @return [String]
      def view_to_text(src)
        s = src.to_s
        %w[<br\ /> <br/> <br> <p> </p>].each { |tag| s = s.gsub(tag, "\n") }
        s = s.gsub(/<script\b[^>]*>.*?<\/script>/mi, " ")
        s = s.gsub(/<style\b[^>]*>.*?<\/style>/mi, " ")
        s = s.gsub(/<[^>]+>/, " ")
        s = s.gsub("&nbsp;", "&").gsub("&nbsp;", " ").gsub("&gt;", ">").gsub("&lt;", "<")
        s = s.gsub("&quot;", '"').gsub("&#39;", "'").gsub("&amp;", "&")
        s.split("\n").map { |ln| ln.strip }.join("\n").strip
      end
    end
  end
end