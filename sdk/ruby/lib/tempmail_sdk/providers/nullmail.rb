# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # Nullmail 渠道实现（nullmail.cc / maildock.store）
    #
    # 无认证 REST：
    #   POST /api/emails（空 JSON body）建箱，响应
    #   {"address":"...@maildock.store","expiry":"2026-09-27T02:25:28.778Z"}；
    #   GET /api/emails/{address}（URL 编码）读信，响应 {"expiry":"...","emails":[...]}，
    #   列表项只有 id/sender/subject/delivered，正文须逐封二拉 GET /api/emails/{addr}/body/{id}
    #   （响应 {"body":...}）。
    module Nullmail
      CHANNEL = "nullmail"
      BASE_URL = "https://www.nullmail.cc"
      DOMAIN = "maildock.store"

      HEADERS = {
        "Accept" => "application/json",
        "Origin" => BASE_URL,
        "Referer" => "#{BASE_URL}/",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " \
                        "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0"
      }.freeze

      module_function

      # 创建 nullmail 临时邮箱
      # POST /api/emails（空 JSON body），token 复用完整地址以便收件箱回查
      # @return [EmailInfo]
      def generate_email
        resp = Http.post("#{BASE_URL}/api/emails",
                         headers: HEADERS.merge("Content-Type" => "application/json"),
                         json: {}, timeout: 15)
        resp.raise_for_status
        data = resp.json
        addr = data.is_a?(Hash) ? data["address"].to_s.strip : ""
        raise "nullmail: 建箱响应缺少 address 字段" if addr.empty?

        EmailInfo.new(channel: CHANNEL, email: addr, token: addr,
                      expires_at: data["expiry"])
      end

      # 单封正文二拉
      # GET /api/emails/{addr}/body/{id}，响应 {"body":"<完整纯文本正文>"}
      # @param addr [String] 邮箱地址
      # @param id [Object] 邮件 ID
      # @return [String]
      def fetch_body(addr, id)
        uri = "#{BASE_URL}/api/emails/#{URI.encode_www_form_component(addr)}" \
              "/body/#{URI.encode_www_form_component(id.to_s)}"
        resp = Http.get(uri, headers: HEADERS, timeout: 15)
        resp.raise_for_status
        data = resp.json
        data.is_a?(Hash) ? data["body"].to_s : ""
      end

      # 读取收件箱
      # GET /api/emails/{address}（完整地址 URL 编码），列表项无正文，逐封二拉 body 端点；
      # 正文拉取失败降级留空不阻断列表。
      # @param email [String] 邮箱地址
      # @param _token [String] 令牌（本渠道忽略，以邮箱为准）
      # @return [Array<Email>]
      def get_emails(email, _token)
        addr = email.to_s.strip
        raise "nullmail: 邮箱地址为空" if addr.empty?

        uri = "#{BASE_URL}/api/emails/#{URI.encode_www_form_component(addr)}"
        resp = Http.get(uri, headers: HEADERS, timeout: 15)
        resp.raise_for_status
        data = resp.json
        emails = data.is_a?(Hash) ? data["emails"] : nil
        return [] unless emails.is_a?(Array)

        emails.filter_map do |m|
          next unless m.is_a?(Hash)

          row = {
            "id" => m["id"],
            "from" => m["sender"] || m["from"],
            "to" => addr,
            "subject" => m["subject"],
            # 列表只有 id/sender/subject/delivered，delivered 显式映射为 date
            "date" => m["delivered"] || m["date"]
          }
          body = fetch_body(addr, m["id"]) if m["id"]
          row["text"] = body unless body.to_s.empty?
          Normalize.normalize_email(row, addr)
        end
      end
    end
  end
end