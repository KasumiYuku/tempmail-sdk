# frozen_string_literal: true

require "json"

module TempmailSdk
  module Providers
    # Tmpkit 渠道实现（tmpkit.com，Next.js tRPC 前端 + Go-Guerrilla SMTP 后端）
    #
    # 流程：
    #   POST /api/rpc/tempmail/initSession（body 为 tRPC 包裹 {"json":{}}）建箱，
    #     响应 session.sessionId 与 session.email（sessionId 同值 Set-Cookie）
    #   POST /api/rpc/tempmail/getEmails（{"json":{"offset":0,"limit":20}} +
    #     Cookie tempmail_session=<sessionId>）列信（列表键 mailId，无 id）
    #   POST /api/rpc/tempmail/getEmailDetail（{"json":{"mailId":<数字>}}）取正文
    # 会话粘性：token 保存 sessionId（tempMailSession=<sid>），读信显式携带
    #   Cookie 请求头，防并行会话串箱。
    module Tmpkit
      CHANNEL = "tmpkit"
      BASE_URL = "https://tmpkit.com"
      RPC_PREFIX = "#{BASE_URL}/api/rpc/tempmail"

      USER_AGENT = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " \
                   "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

      module_function

      # tRPC 调用请求头（Content-Type JSON、Accept 通配、同站 Origin/Referer）
      # @param cookie [String] sessionId，非空时携带 tempmail_session Cookie
      # @return [Hash]
      def rpc_headers(cookie = "")
        hdrs = {
          "User-Agent" => USER_AGENT,
          "Accept" => "*/*",
          "Content-Type" => "application/json",
          "Origin" => BASE_URL,
          "Referer" => "#{BASE_URL}/en"
        }
        hdrs["Cookie"] = "tempmail_session=#{cookie}" unless cookie.empty?
        hdrs
      end

      # 对 tmpkit 发起 rpc 调用（tRPC 包裹 {"json":<reqBody>}）
      # 响应外层 {"json":{...}} 解析为 Hash 返回。
      # @param procedure [String] rpc 过程名
      # @param req_body [Hash] 过程参数
      # @param cookie [String] sessionId
      # @return [Hash]
      def rpc(procedure, req_body, cookie = "")
        resp = Http.post("#{RPC_PREFIX}/#{procedure}",
                         headers: rpc_headers(cookie),
                         body: JSON.generate({ json: req_body }),
                         timeout: 15)
        resp.raise_for_status
        outer = resp.json
        payload = outer.is_a?(Hash) ? outer["json"] : nil
        raise "tmpkit: #{procedure} 响应缺 json 载荷" unless payload.is_a?(Hash)

        payload
      end

      # 创建 tmpkit.com 临时邮箱
      # 调 initSession（tRPC 包裹 {"json":{}}），sessionId 与邮箱地址同返。
      # @return [EmailInfo]
      def generate_email
        data = rpc("initSession", {})
        sess = data["session"]
        raise "tmpkit: 创建会话响应缺 session 字段" unless sess.is_a?(Hash)

        email = sess["email"].to_s.strip
        session_id = sess["sessionId"].to_s.strip
        raise "tmpkit: 创建会话响应缺少必要字段（email/sessionId）" if email.empty? || session_id.empty? || !email.include?("@")

        EmailInfo.new(channel: CHANNEL, email: email,
                      token: "tempMailSession=#{session_id}")
      end

      # 获取 tmpkit.com 邮件列表
      # getEmails（offset 0 / limit 20）取摘要，逐封 getEmailDetail 拉详情并
      # 将详情键并入摘要；详情失败回退列表摘要。getEmails 返回的 session.email
      # 与收信邮箱不符时报错，防止会话被覆盖。
      # @param email [String] 邮箱地址
      # @param token [String] 会话凭据串（tempMailSession=<sessionId>）
      # @return [Array<Email>]
      def get_emails(email, token)
        raw = token.to_s.strip
        mailbox = raw.start_with?("tempMailSession=") ? raw.delete_prefix("tempMailSession=") : raw
        raise "tmpkit: 会话 token 为空" if mailbox.empty?

        data = rpc("getEmails", { offset: 0, limit: 20 }, mailbox)

        # 会话指向校验：getEmails 响应自带 session.email（无 Cookie 时
        # session 为 null，同样视为会话失效）
        sess = data["session"]
        if sess.is_a?(Hash)
          got = sess["email"].to_s.strip
          raise "tmpkit: 会话邮箱不匹配（响应 #{got}，请求 #{email}）" if !got.empty? && got != email
        else
          raise "tmpkit: 会话已失效（getEmails 返回空会话）"
        end

        list = data["emails"]
        raise "tmpkit: 邮件列表响应缺 emails 字段" unless list.is_a?(Array)

        list.map do |item|
          next unless item.is_a?(Hash)

          m = item.dup
          # mailId 为数字：合法时逐封拉详情（详情键并入摘要，跳过会话类键）
          mail_id = m["mailId"]
          if mail_id.is_a?(Numeric) && mail_id.positive?
            begin
              detail = rpc("getEmailDetail", { mailId: mail_id.to_i }, mailbox)
              if detail.is_a?(Hash)
                detail.each do |k, v|
                  next if %w[session emails total error].include?(k)

                  m[k] = v
                end
              end
            rescue StandardError
              # 详情失败回退列表摘要
            end
          end
          Normalize.normalize_email(m, email)
        end.compact
      end
    end
  end
end