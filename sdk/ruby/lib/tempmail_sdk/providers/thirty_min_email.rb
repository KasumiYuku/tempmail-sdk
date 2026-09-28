# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # 30minemail 渠道实现（30minemail.com）
    #
    # 官网邮箱由服务端 16 位 hex 本地名识别：
    #   GET /?generate 返回完整 HTML 页面，从中解析 <地址>@30minemail.com；
    #   本地名无效邮箱访问 messages.php 时返回 ok:false/expired:true，故必须经服务端建箱。
    # 读信 GET /messages.php?email=<完整地址>&_=<unix毫秒>，响应
    #   {"ok":true,"expired":false,"count":0,"emails":[],"expires_in":...}。
    # emails 元素字段实测：{id,from,to,subject,date,html}（html 为完整正文）。
    # 无认证、无 Cookie、无 CSRF。
    module ThirtyMinEmail
      CHANNEL = "30minemail"
      BASE_URL = "https://30minemail.com"
      DOMAIN = "30minemail.com"

      HEADERS = {
        "Accept" => "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      module_function

      # 创建 30minemail.com 临时邮箱
      # GET /?generate 返回 HTML 页面，从其中解析 16 位 hex 本地名地址；
      # token 复用完整地址（服务端以地址定位收件箱）。
      # @return [EmailInfo]
      def generate_email
        resp = Http.get("#{BASE_URL}/?generate", headers: HEADERS, timeout: 15)
        resp.raise_for_status
        page = resp.body.to_s
        idx = page.index("@#{DOMAIN}")
        raise "30minemail: 创建页面未找到邮箱地址" if idx.nil?

        # 向前查找本地名起点：空白或 > 之后
        start = idx
        while start.positive? && ![" ", "\n", "\t", ">", '"'].include?(page[start - 1])
          start -= 1
        end
        local = page[start...idx].to_s.strip
        raise "30minemail: 创建页面解析地址异常" if local.length < 8

        addr = "#{local}@#{DOMAIN}"
        EmailInfo.new(channel: CHANNEL, email: addr, token: addr)
      end

      # 读取 30minemail.com 收件箱
      # GET /messages.php?email=<完整地址>&_=<unix毫秒>，模拟官方轮询参数。
      # @param email [String] 完整邮箱地址
      # @param token [String] 复用完整地址（服务端以地址定位收件箱）
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        raise "30minemail: 邮箱地址为空" if addr.empty?

        uri = "#{BASE_URL}/messages.php?email=#{URI.encode_www_form_component(addr)}&_=#{(Time.now.to_f * 1000).to_i}"
        resp = Http.get(uri,
                        headers: {
                          "Accept" => "application/json",
                          "User-Agent" => HEADERS["User-Agent"]
                        },
                        timeout: 15)
        raise "30minemail: 读取收件箱失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        raise "30minemail: 收件箱响应非对象" unless data.is_a?(Hash)

        unless data["ok"] == true && data["expired"] != true
          raise "30minemail: 收件箱不可用或已过期"
        end
        emails = data["emails"]
        return [] unless emails.is_a?(Array)

        emails.filter_map do |m|
          next unless m.is_a?(Hash)

          row = m.dup
          # 列表元素无 to 字段时注入收件人地址
          row["to"] = addr unless row.key?("to")
          Normalize.normalize_email(row, addr)
        end
      end
    end
  end
end