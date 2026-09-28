# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # tempmails.io 渠道实现
    #
    # 无认证 REST：
    #   POST /api/temp-mail/generate 建箱（空 JSON body，响应 data.email / data.token / data.expires_at）
    #   GET /api/temp-mail/inbox/{token} 读信（messages[] 含 from_email / text_body / html_body / received_at）
    # 邮箱借用 uberip.com 等公共域（约 10 分钟自动过期）。
    module TempmailsIo
      CHANNEL = "tempmails-io"
      BASE_URL = "https://tempmails.io"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " \
                        "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0"
      }.freeze

      module_function

      # 创建 10 分钟临时邮箱
      # POST /api/temp-mail/generate（空 JSON body）
      # @return [EmailInfo]
      def generate_email
        resp = Http.post("#{BASE_URL}/api/temp-mail/generate",
                         headers: HEADERS.merge("Content-Type" => "application/json"),
                         json: {}, timeout: 15)
        resp.raise_for_status
        data = resp.json
        payload = data.is_a?(Hash) ? data["data"] : nil
        email = payload.is_a?(Hash) ? payload["email"].to_s.strip : ""
        token = payload.is_a?(Hash) ? payload["token"].to_s.strip : ""
        raise "tempmails-io: 建箱响应缺少 email 或 token" if email.empty? || token.empty?

        expires = payload.is_a?(Hash) ? payload["expires_at"].to_s : ""
        EmailInfo.new(channel: CHANNEL, email: email, token: token,
                      expires_at: expires.empty? ? nil : expires)
      end

      # 读取收件箱
      # 先 POST /api/temp-mail/fetch-emails/{token} 触发平台对上游信箱的主动同步，
      # 再 GET /api/temp-mail/inbox/{token} 读取；只轮询 inbox 会永远为空。
      # @param email [String] 邮箱地址
      # @param token [String] 建箱返回的令牌
      # @return [Array<Email>]
      def get_emails(email, token)
        tk = token.to_s.strip
        raise "tempmails-io: token 为空" if tk.empty?

        # 触发同步（失败不致命，仍尝试静态读）
        begin
          Http.post("#{BASE_URL}/api/temp-mail/fetch-emails/#{URI.encode_www_form_component(tk)}",
                    headers: HEADERS, timeout: 15)
        rescue StandardError
          nil
        end

        resp = Http.get("#{BASE_URL}/api/temp-mail/inbox/#{URI.encode_www_form_component(tk)}",
                        headers: HEADERS, timeout: 15)
        resp.raise_for_status
        data = resp.json
        payload = data.is_a?(Hash) ? data["data"] : nil
        messages = payload.is_a?(Hash) ? payload["messages"] : nil
        return [] unless messages.is_a?(Array)

        addr = email.to_s.strip
        messages.filter_map do |m|
          next unless m.is_a?(Hash)

          row = {
            "id" => m["id"],
            "from" => m["from_email"] || m["from"],
            "to" => addr,
            "subject" => m["subject"],
            "text" => m["text_body"] || m["text"],
            "html" => m["html_body"] || m["html"],
            "date" => m["received_at"] || m["date"],
            "attachments" => m["attachments"]
          }
          Normalize.normalize_email(row, addr)
        end
      end
    end
  end
end