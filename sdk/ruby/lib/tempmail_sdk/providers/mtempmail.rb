# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # Mtempmail 渠道实现（mtempmail.com，公共 key 认证）
    #
    # 建箱: POST /api/emails/{apiKey}（空 JSON body）→
    #   {"status":true,"data":{"email":"...@domain","domain":"..","ip":"..",
    #    "fingerprint":"..","expire_at":"..","created_at":"..","id":..,"email_token":"..."}}
    # 读信: GET /api/messages/{apiKey}/{email} →
    #   {"status":true,"mailbox":"..","email_token":"..","messages":[...]}
    #   消息为 mailgun 入站 webhook 风格：to/from 为对象数组，
    #   body 为 [{content_type:"text/html",value:".."},...] 分段。
    # 邮箱 24 小时有效（过期时间标注不一致，以服务端为准）。
    module Mtempmail
      CHANNEL = "mtempmail"
      BASE_URL = "https://mtempmail.com"

      # 公共固定 API key（mtempmail.com 官方提供）
      PUBLIC_KEY = "pub_nRn1hUwpdmZvxQNVWDfoXgKyF7dIm9nRIIIt1qDw"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " \
                        "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0"
      }.freeze

      # 剥离后台拼接的 "• " 前缀（或实收分隔符）
      BULLET_RE = /^[•·]+\s*/.freeze

      module_function

      # 清洗主题前导分隔符（后台可能以 "• " 开头拼接微件）
      # @param subject [String]
      # @return [String]
      def clean_subject(subject)
        subject.to_s.strip.sub(BULLET_RE, "")
      end

      # 拼接正文纯文本（body[].value 按序）
      # @param parts [Array]
      # @return [String]
      def body_text(parts)
        parts.filter_map { |p| p["value"].to_s unless p.nil? || !p.is_a?(Hash) || p["value"].to_s.empty? }
             .join("\n")
      end

      # 提取首个 text/html 段
      # @param parts [Array]
      # @return [String]
      def body_html(parts)
        parts.each do |p|
          next unless p.is_a?(Hash) && p["content_type"].to_s == "text/html"

          val = p["value"].to_s
          return val unless val.empty?
        end
        ""
      end

      # 创建 mtempmail 临时邮箱
      # POST /api/emails/{apiKey}（空 JSON body）
      # @return [EmailInfo]
      def generate_email
        resp = Http.post("#{BASE_URL}/api/emails/#{PUBLIC_KEY}",
                         headers: HEADERS.merge("Content-Type" => "application/json"),
                         json: {}, timeout: 15)
        resp.raise_for_status
        data = resp.json
        payload = data.is_a?(Hash) ? data["data"] : nil
        email = payload.is_a?(Hash) ? payload["email"].to_s.strip : ""
        if (data.is_a?(Hash) && !data["status"]) || email.empty?
          raise "mtempmail: 建箱响应缺少邮箱"
        end

        EmailInfo.new(channel: CHANNEL, email: email,
                      token: payload["email_token"].to_s,
                      expires_at: payload["expire_at"],
                      created_at: payload["created_at"])
      end

      # 读取 mtempmail 收件箱
      # GET /api/messages/{apiKey}/{email}；token 元数据仅为校验，不参与请求
      # @param email [String] 邮箱地址
      # @param token [String] email_token（仅校验非空）
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        raise "mtempmail: 邮箱为空" if addr.empty?
        raise "mtempmail: token 为空" if token.to_s.strip.empty?

        uri = "#{BASE_URL}/api/messages/#{PUBLIC_KEY}/#{URI.encode_www_form_component(addr)}"
        resp = Http.get(uri, headers: HEADERS, timeout: 15)
        resp.raise_for_status
        data = resp.json
        messages = data.is_a?(Hash) ? data["messages"] : nil
        return [] unless messages.is_a?(Array)

        messages.filter_map do |m|
          next unless m.is_a?(Hash)

          to_str = flatten_contact(m["to"])
          row = {
            "id" => m["id"],
            "from" => flatten_contact(m["from"]),
            "to" => to_str.empty? ? addr : to_str,
            "subject" => clean_subject(m["subject"]),
            "date" => m["created_at"] || m["date"]
          }
          body_parts = m["body"]
          if body_parts.is_a?(Array)
            parts = body_parts.select { |p| p.is_a?(Hash) }
            text = body_text(parts)
            row["text"] = text unless text.empty?
            html_part = body_html(parts)
            row["html"] = html_part unless html_part.empty?
          end
          Normalize.normalize_email(row, addr)
        end
      end

      # 将 mailgun 风格 [{"full":"Sender <a@b.com>"},{...}] 展平为字符串
      # 优先取 full 字段，否则回退 address；空列表返回空串
      # @param contacts [Object]
      # @return [String]
      def flatten_contact(contacts)
        return contacts.to_s unless contacts.is_a?(Array)
        return "" if contacts.empty?

        first = contacts.first
        return "" unless first.is_a?(Hash)

        full = first["full"].to_s.strip
        full.empty? ? first["address"].to_s.strip : full
      end
    end
  end
end