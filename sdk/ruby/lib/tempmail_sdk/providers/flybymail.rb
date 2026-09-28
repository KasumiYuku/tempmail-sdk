# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # Flybymail 渠道实现（flybymail.com）
    #
    # POST /api/recipients 建箱（空 JSON body，响应 id/email/createdAt/expiresAt，
    #   expiresAt 为毫秒时间戳），GET /api/recipients/{email}/emails 读信
    #   （按邮箱地址、非 id 查询，响应 {"emails":[...]}）。
    # 信件字段：id/from/to/subject/body（纯文本）/htmlBody（HTML 正文）/preview/time/read。
    module Flybymail
      CHANNEL = "flybymail"
      BASE_URL = "https://flybymail.com"

      HEADERS = {
        "Content-Type" => "application/json",
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      module_function

      # 创建 flybymail.com 临时邮箱
      # POST /api/recipients（空 JSON body）返回 id/email/createdAt/expiresAt，
      # expiresAt 为毫秒时间戳（约 4 小时），转换为毫秒值供 EmailInfo 统一展示。
      # @return [EmailInfo]
      def generate_email
        resp = Http.post("#{BASE_URL}/api/recipients",
                         headers: HEADERS, json: {}, timeout: 15)
        raise "flybymail: 创建邮箱失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        raise "flybymail: 创建响应非对象" unless data.is_a?(Hash)

        id = data["id"].to_s.strip
        email = data["email"].to_s.strip
        raise "flybymail: 创建邮箱响应缺少必要字段" if id.empty? || email.empty? || !email.include?("@")

        expires_at = data["expiresAt"].to_i.positive? ? data["expiresAt"].to_i : nil
        EmailInfo.new(channel: CHANNEL, email: email, token: id, expires_at: expires_at)
      end

      # 获取 flybymail.com 邮件列表
      # GET /api/recipients/{email}/emails（按地址查询）返回 {"emails":[...]}。
      # @param email [String] 邮箱地址
      # @param token [String] 建箱返回的收件人 ID（读信端点按邮箱地址查询，令牌保留）
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        raise "flybymail: 邮箱地址为空或格式错误" if addr.empty? || !addr.include?("@")

        uri = "#{BASE_URL}/api/recipients/#{URI.encode_www_form_component(addr)}/emails"
        resp = Http.get(uri, headers: HEADERS, timeout: 15)
        raise "flybymail: 获取邮件列表失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        emails = data.is_a?(Hash) ? data["emails"] : nil
        return [] unless emails.is_a?(Array)

        emails.filter_map do |raw|
          next unless raw.is_a?(Hash)

          # id 为数字时按字符串归一，time 为毫秒时间戳时按 timestamp 归一
          row = {
            "id" => raw["id"].to_s.strip,
            "from" => raw["from"],
            "to" => raw["to"],
            "subject" => raw["subject"],
            "text" => raw["body"],
            "html" => raw["htmlBody"],
            "time" => raw["time"],
            "read" => raw["read"],
            "attachments" => raw["attachments"],
            "timestamp" => timestamp_of(raw)
          }
          Normalize.normalize_email(row, addr)
        end
      end

      # 归一邮件时间：候选 time/date 字段（time 为毫秒时间戳）
      # @param m [Hash]
      # @return [Object, nil]
      def timestamp_of(m)
        return m["time"] if m.key?("time") && !m["time"].nil?

        m["date"]
      end
    end
  end
end