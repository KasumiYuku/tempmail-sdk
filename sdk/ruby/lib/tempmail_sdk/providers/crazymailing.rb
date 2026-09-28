# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # Crazymailing 渠道实现（crazymailing.com）
    #
    # Next.js 全栈站点，API 结构：
    #   POST /api/mailbox（空 JSON body）建箱，响应
    #     {"mailbox":{"id":"...","address":"...@crazymailing.com","expiresAt":"<RFC3339>"}}；
    #   GET /api/messages?mailbox=<完整地址 URL 编码> 读信，响应 {"messages":[...]}；
    #   GET /api/message/{id}/body 取单封正文（完整 HTML 页面）。
    # 请求需携带 Origin/Referer 浏览器形态头。
    module Crazymailing
      CHANNEL = "crazymailing"
      BASE_URL = "https://crazymailing.com"

      HEADERS = {
        "Content-Type" => "application/json",
        "Accept" => "application/json",
        "Origin" => BASE_URL,
        "Referer" => "#{BASE_URL}/",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      RETRY_KEYS = %w[id Id slug messageId message_id].freeze

      module_function

      # 创建临时邮箱
      # POST /api/mailbox（空 JSON body）；域名由服务端统一分配（当前仅 @crazymailing.com）。
      # @return [EmailInfo]
      def generate_email
        resp = Http.post("#{BASE_URL}/api/mailbox",
                         headers: HEADERS, json: {}, timeout: 15)
        raise "crazymailing: 创建邮箱失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        mailbox = data.is_a?(Hash) ? data["mailbox"] : nil
        raise "crazymailing: 创建响应缺少 mailbox" unless mailbox.is_a?(Hash)

        address = mailbox["address"].to_s.strip
        raise "crazymailing: 创建响应缺少 mailbox.address" if address.empty?

        EmailInfo.new(channel: CHANNEL, email: address,
                      token: mailbox["id"].to_s, expires_at: mailbox["expiresAt"].to_s)
      end

      # 读取收件箱
      # GET /api/messages?mailbox=<完整地址 URL 编码>，响应 {"messages":[...]}；
      # 对每个元素逐封 GET /api/message/{id}/body 拉取正文（失败不阻断列表）。
      # @param email [String] 完整邮箱地址
      # @param token [String] 建箱返回的 mailbox id
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        raise "crazymailing: 邮箱地址为空" if addr.empty?

        uri = "#{BASE_URL}/api/messages?mailbox=#{URI.encode_www_form_component(addr)}"
        resp = Http.get(uri, headers: HEADERS, timeout: 15)
        raise "crazymailing: 读取收件箱失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        messages = data.is_a?(Hash) ? data["messages"] : nil
        return [] unless messages.is_a?(Array)

        messages.filter_map do |m|
          next unless m.is_a?(Hash)

          row = m.dup
          row["to"] = addr unless row.key?("to")
          id = message_id_of(row)
          unless id.empty?
            # 列表元素为摘要，正文须逐封二拉（失败不阻断）
            html = get_body(id)
            row["html"] = html if !html.empty?
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

      # 拉取单封正文（GET /api/message/{id}/body，响应为完整 HTML 页面）
      # @param id [String] 邮件 ID
      # @return [String]
      def get_body(id)
        uri = "#{BASE_URL}/api/message/#{URI.encode_www_form_component(id)}/body"
        resp = Http.get(uri,
                        headers: {
                          "Accept" => "text/html,application/xhtml+xml,*/*;q=0.8",
                          "Origin" => BASE_URL,
                          "Referer" => "#{BASE_URL}/",
                          "User-Agent" => HEADERS["User-Agent"]
                        },
                        timeout: 15)
        return "" unless resp.ok?

        resp.body.to_s
      end
    end
  end
end