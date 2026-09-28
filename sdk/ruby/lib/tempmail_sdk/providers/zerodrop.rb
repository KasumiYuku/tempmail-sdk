# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # Zerodrop 渠道实现（zerodrop.dev）
    #
    # 无认证 REST：建箱无需请求，本地生成 "sdk"+8 位随机名，地址为 <名>@zerodrop-sandbox.online；
    # 读信 GET /api/inbox/{name}?source=sdk，响应形如 {"emails":[...],"count":N}。
    # 平台邮件对象只有 id/from/to/subject/receivedAt/raw/otp/magicLink，正文仅存在于
    # raw（完整 MIME 原文），需剥离头部提取纯文本 body。
    module Zerodrop
      CHANNEL = "zerodrop"
      BASE_URL = "https://zerodrop.dev"
      DOMAIN = "zerodrop-sandbox.online"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " \
                        "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0"
      }.freeze

      module_function

      # 生成 "sdk"+8 位 [a-z0-9] 随机本地名
      CHARS = ("a".."z").to_a + ("0".."9").to_a
      def random_name
        "sdk" + Array.new(8) { CHARS.sample }.join
      end

      # 从 raw（完整 MIME 原文）提取纯文本正文：
      # 定位首个空行（RFC 5322 头部/正文分隔，\r\n\r\n 或 \n\n），其后部分即 body
      # @param raw [String] MIME 原文
      # @return [String]
      def raw_body(raw)
        idx = raw.index("\r\n\r\n")
        return raw[(idx + 4)..] if idx

        idx = raw.index("\n\n")
        return raw[(idx + 2)..] if idx

        ""
      end

      # 创建临时邮箱
      # 建箱无需请求，本地生成随机名，token 复用完整地址以便收件箱回查
      # @return [EmailInfo]
      def generate_email
        email = "#{random_name}@#{DOMAIN}"
        EmailInfo.new(channel: CHANNEL, email: email, token: email)
      end

      # 读取收件箱
      # GET /api/inbox/{name}?source=sdk
      # @param email [String] 邮箱地址
      # @param _token [String] 令牌（本渠道忽略，以邮箱为准）
      # @return [Array<Email>]
      def get_emails(email, _token)
        addr = email.to_s.strip
        parts = addr.split("@", 2)
        raise "zerodrop: 非 #{DOMAIN} 域邮箱地址" unless parts.size == 2 && parts[1] == DOMAIN

        url = "#{BASE_URL}/api/inbox/#{URI.encode_www_form_component(parts[0])}?source=sdk"
        resp = Http.get(url, headers: HEADERS, timeout: 15)
        resp.raise_for_status
        data = resp.json
        emails = data.is_a?(Hash) ? data["emails"] : nil
        return [] unless emails.is_a?(Array)

        emails.filter_map do |m|
          next unless m.is_a?(Hash)

          row = {
            "id" => m["id"],
            "from" => m["from"],
            "to" => addr,
            "subject" => m["subject"],
            "date" => m["receivedAt"] || m["date"]
          }
          # 正文仅存在于 raw（完整 MIME 原文）：提取纯文本 body 作 text
          raw = m["raw"].to_s
          body = raw_body(raw)
          row["text"] = body unless body.empty?
          Normalize.normalize_email(row, addr)
        end
      end
    end
  end
end