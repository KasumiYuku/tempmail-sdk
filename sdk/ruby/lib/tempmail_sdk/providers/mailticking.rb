# frozen_string_literal: true

module TempmailSdk
  module Providers
    # Mailticking 渠道实现（www.mailticking.com，旧域名 temporary-mail.net 的更名站）
    #
    # 实测协议（三类请求均以 application/json 交流，站点在 Cloudflare 后）：
    #   1. 建箱 POST /get-mailbox body {"types":["4"]}（4=独立域名，排除 Gmail 别名）
    #      响应 {"success":true,"email":"xxx@domain","activate_token":"..."}
    #   2. 激活 POST /activate-email body {"email":..,"source":"api","activate_token":..}
    #      响应 {"success":true}
    #   3. 列信 POST /get-emails?lang=en body {"email":..,"code":..}
    #      空箱实测响应 {"emails":[],"success":true}；空闲超时/被改绑后返回
    #      {"success":false,"needNewEmail":true,...}，此时返回错误提示换箱。
    #
    # 读信正文无公开端点：GetEmails 只具备列表能力，列表字段名采用多候选提取
    # 策略，命中多少映射多少（Normalize 既有候选字段负责兜底）。
    module Mailticking
      CHANNEL = "mailticking"
      BASE_URL = "https://www.mailticking.com"

      HEADERS = {
        "Accept" => "application/json",
        "Accept-Language" => "en-US,en;q=0.9",
        "Content-Type" => "application/json",
        "Referer" => "#{BASE_URL}/",
        "Origin" => BASE_URL,
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      module_function

      # 执行 JSON POST 请求并解析响应；响应非 2xx 或 success=false 时抛错
      # @param path [String] 请求路径
      # @param payload [Hash] JSON 请求体
      # @return [Hash]
      def do_post(path, payload)
        resp = Http.post("#{BASE_URL}#{path}", headers: HEADERS, json: payload, timeout: 15)
        data = resp.json
        unless resp.ok?
          msg = data.is_a?(Hash) ? (data["error"] || data["message"]).to_s : ""
          msg = resp.body.to_s if msg.empty?
          raise "mailticking: http #{resp.status_code}: #{msg}"
        end
        unless data.is_a?(Hash) && data["success"] == true
          msg = data.is_a?(Hash) ? (data["error"] || data["message"]).to_s : ""
          msg = "unknown error" if msg.empty?
          raise "mailticking: 请求失败: #{msg}"
        end
        data
      end

      # 创建 mailticking 邮箱账号
      # 请求 type=4（独立域名）固定取独立域名邮箱，建箱后立即激活会话；
      # token 必须携带 activate_token（列信协议依赖激活会话），不能为空。
      # @return [EmailInfo]
      def generate_email
        box = do_post("/get-mailbox", { "types" => ["4"] })
        token = box["code"].to_s.strip
        token = box["email"].to_s.strip if token.empty?
        email = box["email"].to_s.strip
        if token.empty? || email.empty?
          raise "mailticking: get-mailbox 返回空 email/activate_token"
        end

        # 激活邮箱，使后续列信请求与服务器记录的最新会话一致
        do_post("/activate-email", {
                  "email" => email,
                  "source" => "api",
                  "activate_token" => token
                })

        EmailInfo.new(channel: CHANNEL, email: email, token: token)
      end

      # 获取 mailticking 邮箱的邮件列表
      # 空箱返回空列表不报错；需要换箱的响应（needNewEmail）返回语义错误。
      # @param email [String] 邮箱地址
      # @param token [String] 建箱返回的激活令牌
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        tk = token.to_s.strip
        raise "mailticking: 邮箱地址为空" if addr.empty?
        raise "mailticking: activate code 为空" if tk.empty?

        resp = Http.post("#{BASE_URL}/get-emails?lang=en",
                         headers: HEADERS,
                         json: { "email" => addr, "code" => tk },
                         timeout: 15)
        data = resp.json
        unless resp.ok?
          msg = data.is_a?(Hash) ? (data["error"] || data["message"]).to_s : ""
          msg = resp.body.to_s if msg.empty?
          raise "mailticking: http #{resp.status_code}: #{msg}"
        end
        unless data.is_a?(Hash) && data["success"] == true
          raise "mailticking: 邮箱已过期，请重新建箱" if data.is_a?(Hash) && data["needNewEmail"] == true

          raise "mailticking: get-emails 失败"
        end
        emails = data["emails"]
        return [] unless emails.is_a?(Array)

        emails.filter_map do |raw|
          next unless raw.is_a?(Hash)

          Normalize.normalize_email(migrated_fields(raw), addr)
        end
      end

      # 尽量映射列表字段到统一字段名
      # 官网首页表格只有 SENDER/SUBJECT/TIME 三列，字段全名缺少可观测证据，
      # 因此对常见字段做多候选提取；未命中的字段留给 Normalize 自己处理。
      # @param raw [Hash]
      # @return [Hash]
      def migrated_fields(raw)
        m = raw.dup
        unless m.key?("from")
          %w[mail_from from_mail from_email sender_address from_address
             send_addr mail_addr address_from ho_from fromname fromS].each do |key|
            v = m[key]
            if v.is_a?(String) && v.strip != ""
              m["from"] = v
              break
            end
          end
        end
        if !m.key?("sender") && m["from"].is_a?(String) && m["from"].strip != ""
          m["sender"] = m["from"]
        end
        m["id"] = m["mail_id"] if !m.key?("id") && m.key?("mail_id")
        m["date"] = m["received_at"] if !m.key?("date") && m.key?("received_at")
        m
      end
    end
  end
end