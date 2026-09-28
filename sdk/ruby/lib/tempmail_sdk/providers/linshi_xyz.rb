# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # LinshiXYZ 渠道实现（linshi.xyz）
    #
    # 无建箱请求：本地随机 6 位 hex 前缀（官网 client 即用短 id）+ @linshi.xyz。
    # 读信 GET https://linshi.xyz/api/mails/{前缀}，无邮件时返回空数组 []，
    #   有邮件时为邮件对象数组（元素含 headers{from,to,subject,date} 与 html 正文）。
    # 归一化时将 headers 平铺并注入收件人地址；非数组骨架（如 {ok:false}）整体失败，
    # 交由上层 fallback 渠道处理。
    module LinshiXyz
      CHANNEL = "linshi-xyz"
      BASE_URL = "https://linshi.xyz"
      DOMAIN = "linshi.xyz"
      HEX_DIGITS = "0123456789abcdef"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      module_function

      # 生成本地随机 6 位 hex 前缀（与官网 client 相同格式）
      # @return [String]
      def local_name
        Array.new(6) { HEX_DIGITS[rand(HEX_DIGITS.length)] }.join
      end

      # 创建 linshi.xyz 临时邮箱
      # 无需建箱请求，本地随机 6 位 hex 前缀；token 复用完整地址。
      # @return [EmailInfo]
      def generate_email
        addr = "#{local_name}@#{DOMAIN}"
        EmailInfo.new(channel: CHANNEL, email: addr, token: addr)
      end

      # 读取 linshi.xyz 收件箱
      # GET /api/mails/{前缀}（URL 编码），响应为邮件对象数组。
      # @param email [String] 完整邮箱地址（前缀@linshi.xyz）
      # @param token [String] 复用完整地址
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        raise "linshi-xyz: 邮箱地址无效: #{addr.inspect}" if addr.empty? || !addr.include?("@")

        local = addr.split("@", 2).first.to_s
        resp = Http.get("#{BASE_URL}/api/mails/#{URI.encode_www_form_component(local)}",
                        headers: HEADERS, timeout: 15)
        raise "linshi-xyz: 读取收件箱失败 http #{resp.status_code}" unless resp.ok?

        list = resp.json
        raise "linshi-xyz: 收件箱响应非数组" unless list.is_a?(Array)

        list.filter_map do |m|
          next unless m.is_a?(Hash)

          Normalize.normalize_email(normalize_row(m, addr), addr)
        end
      end

      # 将 linshi.xyz 邮件对象归一化：headers 平铺为顶层字段并注入收件人。
      # @param m [Hash] 原始邮件对象
      # @param email [String] 收件人地址
      # @return [Hash]
      def normalize_row(m, email)
        flat = m.dup
        headers = m["headers"]
        flat.merge!(headers) if headers.is_a?(Hash)
        flat["to"] = email unless flat.key?("to")
        flat
      end
    end
  end
end