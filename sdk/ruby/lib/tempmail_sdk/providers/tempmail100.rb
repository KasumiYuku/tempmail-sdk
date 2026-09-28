# frozen_string_literal: true

module TempmailSdk
  module Providers
    # TempMail100 渠道实现（tempmail100.com）
    #
    # POST /init 建箱（空 body，响应 code/data.token（JWT），无 Cookie），
    # POST /web/generate 建随机地址（Header Authorization: <token>，响应 data.address），
    # GET /web/emails 读信列表（Header Authorization: <token>，响应 data.list[]/data.total）。
    #
    # 平台限制（实测确认，非 SDK 缺陷）：
    # 列表元素 content 恒为空字符串，详情端点对真实 token 返回 HTTP 200 + 空 body，
    # 因此正文永久不可得，本渠道客观为「列表-only」：subject/fromAddress/fromName/
    # timestamp/read 可正确输出，正文恒为空属平台限制。
    module Tempmail100
      CHANNEL = "tempmail100"
      BASE_URL = "https://tempmail100.com"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      module_function

      # 设置 tempmail100 请求的通用请求头
      # 前端使用 Authorization: <token> 不带 Bearer。
      # @param token [String] JWT
      # @return [Hash]
      def auth_headers(token)
        hdrs = HEADERS.dup
        hdrs["Authorization"] = token unless token.to_s.empty?
        hdrs
      end

      # 创建 tempmail100.com 临时邮箱
      # 流程：POST /init 取得 JWT token，再 POST /web/generate 创建地址。
      # @return [EmailInfo]
      def generate_email
        # 第一步：初始化取得 token
        resp = Http.post("#{BASE_URL}/init", headers: auth_headers(""), timeout: 15)
        raise "tempmail100: 初始化失败 http #{resp.status_code}" unless resp.ok?

        init = resp.json
        raise "tempmail100: 初始化响应非对象" unless init.is_a?(Hash)

        token = init.dig("data", "token").to_s.strip
        raise "tempmail100: 初始化响应异常" if init["code"] != 0 || token.empty?

        # 第二步：创建随机地址（Authorization: <token> 不带 Bearer）
        resp2 = Http.post("#{BASE_URL}/web/generate",
                          headers: auth_headers(token), timeout: 15)
        raise "tempmail100: 创建地址失败 http #{resp2.status_code}" unless resp2.ok?

        gen = resp2.json
        raise "tempmail100: 创建地址响应非对象" unless gen.is_a?(Hash)

        address = gen.dig("data", "address").to_s.strip
        unless gen["code"] == 0 && !address.empty? && address.include?("@")
          raise "tempmail100: 创建地址响应异常"
        end

        EmailInfo.new(channel: CHANNEL, email: address, token: token)
      end

      # 获取 tempmail100.com 邮件列表
      # 流程：GET /web/emails（Authorization: <token> 不带 Bearer）返回 data.list/data.total。
      # @param email [String] 邮箱地址
      # @param token [String] 初始化返回的 JWT 认证令牌
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        resp = Http.get("#{BASE_URL}/web/emails",
                        headers: auth_headers(token), timeout: 15)
        raise "tempmail100: 获取邮件列表失败 http #{resp.status_code}" unless resp.ok?

        data = resp.json
        raise "tempmail100: 获取邮件列表响应非对象" unless data.is_a?(Hash)

        unless data["code"] == 0
          raise "tempmail100: 获取邮件列表响应异常: #{data["message"]}"
        end
        inner = data["data"]
        list = inner.is_a?(Hash) ? inner["list"] : nil
        return [] unless list.is_a?(Array)

        list.filter_map do |m|
          next unless m.is_a?(Hash)

          Normalize.normalize_email(normalize_item(m, addr), addr)
        end
      end

      # 将 /web/emails 列表元素归一为统一邮件结构。
      # fromName+fromAddress 组合为 "Name <address>" 填入 from；
      # timestamp 为毫秒值（>1e12 时按 UnixMilli 解析）；read 为布尔已读标记。
      # @param item [Hash] 列表元素
      # @param email [String] 收件人地址
      # @return [Hash]
      def normalize_item(item, email)
        from_name = str_of(item["fromName"]).strip
        from_address = str_of(item["fromAddress"]).strip
        unless from_name.empty? || from_name.casecmp(from_address).zero? || !from_address.include?("@")
          from_address = "#{from_name} <#{from_address}>"
        end

        {
          "id" => str_of(item["uuid"]),
          "from" => from_address,
          "to" => str_of(item["toAddress"]).strip,
          "subject" => str_of(item["subject"]),
          "content" => str_of(item["content"]),
          "timestamp" => item["timestamp"],
          "isRead" => read_of(item["read"])
        }
      end

      # 将接口字段值安全转换为字符串，nil 或非标量返回空串
      # @param v [Object]
      # @return [String]
      def str_of(v)
        case v
        when String then v
        when Numeric then v.to_i.to_s
        when true then "true"
        when false then "false"
        else ""
        end
      end

      # 将 read 字段归一为布尔已读标记，兼容 bool / 数字(0|1) / string("true"|"1")
      # @param v [Object]
      # @return [Boolean]
      def read_of(v)
        case v
        when true then true
        when false then false
        when Numeric then v != 0
        when String
          s = v.strip
          s.casecmp("true").zero? || s == "1"
        else false
        end
      end
    end
  end
end