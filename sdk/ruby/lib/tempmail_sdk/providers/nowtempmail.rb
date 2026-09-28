# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # NowtempMail 渠道实现（nowtempmail.com）
    #
    # POST /mailbox 建箱（无 body 或 {}，响应 token（JWT）/mailbox），
    # GET /messages 读信列表（Header Authorization: Bearer <token>，
    #   响应 {"messages":[...]}），GET /message/{id} 取单封详情（Bearer）。
    # 列表元素按多候选字段归一；详情拉取失败时以列表摘要兜底。
    module Nowtempmail
      CHANNEL = "nowtempmail"
      BASE_URL = "https://nowtempmail.com"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      RETRY_KEYS = %w[id Id slug messageId message_id].freeze

      module_function

      # 设置 nowtempmail 请求的通用请求头（可选附带 Bearer token）
      # @param token [String] 认证令牌
      # @return [Hash]
      def auth_headers(token)
        hdrs = HEADERS.dup
        hdrs["Authorization"] = "Bearer #{token}" unless token.to_s.empty?
        hdrs
      end

      # 创建 nowtempmail.com 临时邮箱
      # POST /mailbox（空 body）返回 token（JWT）与 mailbox 地址。
      # @return [EmailInfo]
      def generate_email
        resp = Http.post("#{BASE_URL}/mailbox",
                         headers: auth_headers("").merge("Content-Type" => "application/json"),
                         timeout: 15)
        raise "nowtempmail: 创建邮箱失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        raise "nowtempmail: 创建响应非对象" unless data.is_a?(Hash)

        token = data["token"].to_s.strip
        mailbox = data["mailbox"].to_s.strip
        raise "nowtempmail: 创建邮箱响应缺少必要字段" if token.empty? || mailbox.empty? || !mailbox.include?("@")

        EmailInfo.new(channel: CHANNEL, email: mailbox, token: token)
      end

      # 获取 nowtempmail.com 邮件列表
      # 流程：GET /messages 取列表，对每个元素按 id 逐封 GET /message/{id} 合并详情；
      # 详情失败时以列表摘要归一。
      # @param email [String] 邮箱地址
      # @param token [String] 建箱返回的 JWT 认证令牌
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        resp = Http.get("#{BASE_URL}/messages", headers: auth_headers(token), timeout: 15)
        raise "nowtempmail: 获取邮件列表失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        messages = data.is_a?(Hash) ? data["messages"] : nil
        return [] unless messages.is_a?(Array)

        messages.filter_map do |m|
          next unless m.is_a?(Hash)

          detail = get_detail(token, message_id_of(m))
          row = m.dup
          if detail.is_a?(Hash)
            detail.each { |k, v| row[k] = v unless row.key?(k) }
          end
          Normalize.normalize_email(row, addr)
        end
      end

      # 从列表元素中提取邮件 ID（候选键 id/Id/slug/messageId/message_id）
      # @param row [Hash]
      # @return [String]
      def message_id_of(row)
        RETRY_KEYS.each do |key|
          val = row[key]
          return val.to_s.strip unless val.nil? || val.to_s.strip.empty?
        end
        ""
      end

      # 获取 nowtempmail.com 单封邮件详情
      # GET /message/{id}（Bearer token），响应为单封对象，含 text/html 等完整字段。
      # @param token [String] JWT
      # @param id [String] 邮件 ID
      # @return [Hash, nil]
      def get_detail(token, id)
        return nil if id.to_s.empty?

        uri = "#{BASE_URL}/message/#{URI.encode_www_form_component(id)}"
        resp = Http.get(uri, headers: auth_headers(token), timeout: 15)
        return nil unless resp.ok?

        detail = resp.json
        detail.is_a?(Hash) ? detail : nil
      rescue StandardError
        nil
      end
    end
  end
end