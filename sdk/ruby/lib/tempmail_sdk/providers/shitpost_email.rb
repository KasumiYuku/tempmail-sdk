# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # shitpost.email 渠道实现（公共实例，克隆自 shamu4life/throwaway-email）
    #
    # 无认证 REST：
    #   POST /api/create 建箱（username/domain/ttl → email/token/type/expires）
    #   GET /api/inbox?email=&token= 读信（messages[] 含 from/fromName/subject/text/html/date）
    # 域名池：shitpost.email / letsfuckingpiss.party
    module ShitpostEmail
      CHANNEL = "shitpost-email"
      BASE_URL = "https://shitpost.email"
      DOMAINS = %w[shitpost.email letsfuckingpiss.party].freeze

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " \
                        "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0"
      }.freeze

      module_function

      # 生成 "sdk"+10 位 [a-z0-9] 随机用户名
      CHARS = ("a".."z").to_a + ("0".."9").to_a
      def random_username
        "sdk" + Array.new(10) { CHARS.sample }.join
      end

      # 创建临时邮箱
      # POST /api/create，body 为 {username, domain, ttl}
      # @return [EmailInfo]
      def generate_email
        body = { "username" => random_username, "domain" => DOMAINS.sample, "ttl" => 3600 }
        resp = Http.post("#{BASE_URL}/api/create",
                         headers: HEADERS.merge("Content-Type" => "application/json"),
                         json: body, timeout: 15)
        resp.raise_for_status
        data = resp.json
        email = data.is_a?(Hash) ? data["email"].to_s.strip : ""
        token = data.is_a?(Hash) ? data["token"].to_s.strip : ""
        raise "shitpost-email: 建箱响应缺少 email 或 token" if email.empty? || token.empty?

        EmailInfo.new(channel: CHANNEL, email: email, token: token,
                      expires_at: data["expires"])
      end

      # 读取收件箱
      # GET /api/inbox?email=&token=
      # @param email [String] 邮箱地址
      # @param token [String] 建箱返回的令牌
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        tk = token.to_s
        raise "shitpost-email: token 为空" if tk.strip.empty?

        url = "#{BASE_URL}/api/inbox?email=#{URI.encode_www_form_component(addr)}" \
              "&token=#{URI.encode_www_form_component(tk)}"
        resp = Http.get(url, headers: HEADERS, timeout: 15)
        resp.raise_for_status
        data = resp.json
        messages = data.is_a?(Hash) ? data["messages"] : nil
        return [] unless messages.is_a?(Array)

        messages.filter_map do |m|
          next unless m.is_a?(Hash)

          row = {
            "id" => m["id"],
            "from" => m["from"],
            "to" => addr,
            "subject" => m["subject"],
            "text" => m["text"],
            "html" => m["html"],
            "date" => m["date"]
          }
          Normalize.normalize_email(row, addr)
        end
      end
    end
  end
end