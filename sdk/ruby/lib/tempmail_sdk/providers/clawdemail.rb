# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # ClawdEmail 渠道实现（clawdemail.com，API 域 api.clawdemail.com）
    #
    # POST /register 建箱（无验证码，body {"name":"xxx"}，响应 success/email/token），
    # GET /inbox 读信列表（Header Authorization: Bearer <token>，
    #   响应 success/email/count/unread/emails[]），GET /email/{id} 取单封详情（Bearer）。
    # 信件保留 30 分钟，仅接收不发送，正文仅纯文本。
    module Clawdemail
      CHANNEL = "clawdemail"
      BASE_URL = "https://api.clawdemail.com"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      RETRY_KEYS = %w[id Id slug messageId message_id].freeze

      module_function

      # 设置 clawdemail 请求的通用请求头（可选附带 Bearer token）
      # @param token [String] 认证令牌
      # @return [Hash]
      def auth_headers(token)
        hdrs = HEADERS.dup
        hdrs["Authorization"] = "Bearer #{token}" unless token.to_s.empty?
        hdrs
      end

      # 创建 clawdemail.com 临时邮箱
      # POST /register（body {"name":""}）返回 email 与 token。
      # @return [EmailInfo]
      def generate_email
        resp = Http.post("#{BASE_URL}/register",
                         headers: auth_headers("").merge("Content-Type" => "application/json"),
                         json: { "name" => "" }, timeout: 15)
        raise "clawdemail: 创建邮箱失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        raise "clawdemail: 创建响应非对象" unless data.is_a?(Hash)

        email = data["email"].to_s.strip
        token = data["token"].to_s.strip
        raise "clawdemail: 创建邮箱响应缺少必要字段" if email.empty? || token.empty? || !email.include?("@")

        EmailInfo.new(channel: CHANNEL, email: email, token: token)
      end

      # 获取 clawdemail.com 邮件列表
      # 流程：GET /inbox?limit=50 取列表，对每个元素按 id 逐封 GET /email/{id} 合并详情；
      # 详情失败时以列表摘要归一。
      # @param email [String] 邮箱地址
      # @param token [String] 注册返回的认证令牌
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        resp = Http.get("#{BASE_URL}/inbox?limit=50",
                        headers: auth_headers(token), timeout: 15)
        raise "clawdemail: 获取邮件列表失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        raise "clawdemail: 收件箱响应非对象" unless data.is_a?(Hash)

        unless data["success"] == true
          raise "clawdemail: 读取收件箱失败: #{data["error"]}"
        end
        emails = data["emails"]
        return [] unless emails.is_a?(Array)

        emails.filter_map do |m|
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

      # 获取 clawdemail.com 单封邮件详情
      # GET /email/{id}（Bearer token），响应含 email 嵌套对象（from_addr/body_text/received_at）
      # 时提升嵌套对象。
      # @param token [String] 认证令牌
      # @param id [String] 邮件 ID
      # @return [Hash, nil]
      def get_detail(token, id)
        return nil if id.to_s.empty?

        uri = "#{BASE_URL}/email/#{URI.encode_www_form_component(id)}"
        resp = Http.get(uri, headers: auth_headers(token), timeout: 15)
        return nil unless resp.ok?

        detail = resp.json
        return nil unless detail.is_a?(Hash)

        nested = detail["email"]
        nested.is_a?(Hash) ? nested : detail
      rescue StandardError
        nil
      end
    end
  end
end