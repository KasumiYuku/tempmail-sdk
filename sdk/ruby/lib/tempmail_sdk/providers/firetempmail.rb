# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # Firetempmail 渠道实现（firetempmail.com）
    #
    # 无认证 REST：建箱无需请求，本地生成 随机词+0-999@<域>（域池与官网一致：
    # offrework.click / service-today.click / jobsdeforyou.sa.com）；
    # 读信 GET https://mail.firetempmail.com/mail/get?address=<邮箱 URL 编码>，
    # 必须携带 Header Origin: https://firetempmail.com（否则 403 'Origin not allowed'）。
    # 响应形如 {"status":"ok","code":200,"msg":"...","stats":{},"mails":[...]}，
    # 邮件字段以 sender/subject/date + content-html/content-text/content-plain 多候选归一。
    module Firetempmail
      CHANNEL = "firetempmail"
      API_BASE = "https://mail.firetempmail.com"
      ORIGIN = "https://firetempmail.com"
      # 官网 JS 中的完整平台域池，顺序与官网一致
      DOMAINS = %w[offrework.click service-today.click jobsdeforyou.sa.com].freeze

      HEADERS = {
        "Accept" => "application/json",
        "Origin" => ORIGIN,
        "Referer" => "#{ORIGIN}/",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " \
                        "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0"
      }.freeze

      module_function

      # 生成 3-6 位小写随机词（模拟官网 1-5 字母词的观感）
      LETTERS = ("a".."z").to_a
      def random_word
        Array.new(rand(3..6)) { LETTERS.sample }.join
      end

      # 创建临时邮箱
      # 建箱无需请求，本地随机生成 随机词+0-999@域名；token 复用完整地址
      # @return [EmailInfo]
      def generate_email
        email = "#{random_word}#{rand(0..999)}@#{DOMAINS.sample}"
        EmailInfo.new(channel: CHANNEL, email: email, token: email)
      end

      # 读取收件箱
      # GET https://mail.firetempmail.com/mail/get?address=<URL 编码完整邮箱>，必带 Origin 头
      # @param email [String] 邮箱地址
      # @param _token [String] 令牌（本渠道忽略，以邮箱为准）
      # @return [Array<Email>]
      def get_emails(email, _token)
        addr = email.to_s.strip
        raise "firetempmail: 邮箱地址为空" if addr.empty?

        url = "#{API_BASE}/mail/get?address=#{URI.encode_www_form_component(addr)}"
        resp = Http.get(url, headers: HEADERS, timeout: 15)
        resp.raise_for_status
        data = resp.json
        return [] unless data.is_a?(Hash)

        # 错误与成功共用同一信封：status 非 ok 时抛错
        status = data["status"].to_s
        if !status.empty? && status != "ok"
          raise "firetempmail: #{data["msg"] || "读信失败"}"
        end

        mails = data["mails"]
        return [] unless mails.is_a?(Array)

        mails.filter_map do |m|
          next unless m.is_a?(Hash)

          row = {
            "from" => m["sender"] || m["from"] || m["from_address"],
            "to" => addr,
            "subject" => m["subject"] || m["title"],
            "date" => m["date"] || m["received_at"] || m["created_at"],
            "html" => m["content-html"] || m["html"],
            "text" => m["content-text"] || m["content-plain"] || m["text"]
          }
          Normalize.normalize_email(row, addr)
        end
      end
    end
  end
end