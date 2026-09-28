# frozen_string_literal: true

require "uri"

module TempmailSdk
  module Providers
    # smails.dev 渠道实现
    #
    # 流程：
    #   POST /api/mailbox 建箱（空 JSON body，响应 address/token）
    #   GET /api/mailbox/messages 读信（Header Authorization: Bearer <token>，数组响应）
    #   GET /api/mailbox/messages/{id} 取单封详情（Bearer）
    # 列表元素按 from/subject/text/html/date 多候选归一；详情拉取失败时以列表摘要兜底。
    module Smails
      CHANNEL = "smails"
      BASE_URL = "https://smails.dev"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " \
                        "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0"
      }.freeze

      # 可行 ID 候选键，按序提取（结果转为字符串）
      ID_KEYS = %w[id Id slug messageId message_id].freeze

      module_function

      # 设置 smails.dev 请求的通用请求头
      # @param token [String] 认证令牌，非空时携带 Authorization: Bearer
      # @return [Hash]
      def auth_headers(token = "")
        hdrs = HEADERS.dup
        hdrs["Authorization"] = "Bearer #{token}" unless token.to_s.empty?
        hdrs
      end

      # 创建 smails.dev 临时邮箱
      # POST /api/mailbox（空 JSON body）返回 address 与 token
      # @return [EmailInfo]
      def generate_email
        resp = Http.post("#{BASE_URL}/api/mailbox",
                         headers: HEADERS.merge("Content-Type" => "application/json"),
                         json: {}, timeout: 15)
        resp.raise_for_status
        data = resp.json
        address = data.is_a?(Hash) ? data["address"].to_s.strip : ""
        token = data.is_a?(Hash) ? data["token"].to_s.strip : ""
        raise "smails: 创建邮箱响应缺少必要字段" if address.empty? || token.empty?

        EmailInfo.new(channel: CHANNEL, email: address, token: token)
      end

      # 获取 smails.dev 邮件列表
      # GET /api/mailbox/messages；对每个元素按 id 逐封请求详情合并，失败时以摘要归一。
      # @param token [String] Bearer 令牌
      # @param email [String] 邮箱地址
      # @return [Array<Email>]
      def get_emails(token, email)
        tk = token.to_s.strip
        raise "smails: token 为空" if tk.empty?

        resp = Http.get("#{BASE_URL}/api/mailbox/messages", headers: auth_headers(tk), timeout: 15)
        resp.raise_for_status
        list = resp.json
        return [] unless list.is_a?(Array)

        addr = email.to_s.strip
        list.filter_map do |m|
          next unless m.is_a?(Hash)

          Normalize.normalize_email(detail_row(tk, m, addr), addr)
        end
      end

      # 从列表元素中提取消息 ID（候选键 id/Id/slug/messageId/message_id）
      # @param row [Hash]
      # @return [String]
      def message_id_of(row)
        ID_KEYS.each do |key|
          val = row[key]
          return val.to_s.strip unless val.nil? || val.to_s.strip.empty?
        end
        ""
      end

      # 逐封拉取详情并合并到列表摘要；无 ID 或详情失败时以摘要归一
      # @param token [String] Bearer 令牌
      # @param summary [Hash] 列表元素摘要
      # @param email [String] 收件人邮箱
      # @return [Hash] 归一化输入行
      def detail_row(token, summary, email)
        id = message_id_of(summary)
        uri = "#{BASE_URL}/api/mailbox/messages/#{URI.encode_www_form_component(id)}"
        resp = Http.get(uri, headers: auth_headers(token), timeout: 15)
        merged = summary
        if resp.ok?
          detail = begin
            resp.json
          rescue StandardError
            nil
          end
          # 详情缺失字段不覆盖列表摘要（包含 null 的键跳过）
          merged = summary.merge(detail.reject { |_k, v| v.nil? }) if detail.is_a?(Hash)
        end
        to_normalized_row(merged, email)
      rescue StandardError
        to_normalized_row(summary, email)
      end

      # 将原始消息 Hash 转换为归一化输入行
      # @param raw [Hash]
      # @param email [String]
      # @return [Hash]
      def to_normalized_row(raw, email)
        {
          "id" => raw["id"] || raw["messageId"] || raw["message_id"],
          "from" => raw["from"],
          "to" => email,
          "subject" => raw["subject"],
          "text" => raw["text"],
          "html" => raw["html"],
          "date" => raw["date"]
        }
      end
    end
  end
end