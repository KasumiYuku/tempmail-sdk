# frozen_string_literal: true

module TempmailSdk
  module Providers
    # Shadowmail 渠道实现（shadowmail.win）
    #
    # 完整接入契约：
    #   注册 POST /api/register {"email":"<随机前缀>@gmail.com","password":"Abcd1234!"}
    #     → 200 {"message":"Successfully Registered"}；
    #   登录 POST /api/login → 200 {"message":"Successfull Login"}
    #     并 Set-Cookie: sessionId=<uuid>（HttpOnly; Secure; Max-Age 3600）；
    #   建箱 POST /api/new-address → 200 {...,"address":"<id>@shadowmail.win","id":<id>}；
    #   读信 POST /api/get-emails {"address":"<地址>"} → 200 {"message":"Emails read",
    #     "mails":[...]}；mails 元素字段 id/address_id/sender/subject/body/created_at。
    # 会话隔离：全域使用显式 Cookie 头（sessionId=<uuid>），凭据串 token 持久化
    #   注册邮箱/密码/会话 id，便于会话过期后自动重生。
    module Shadowmail
      CHANNEL = "shadowmail"
      BASE_URL = "https://shadowmail.win"
      # 固定注册密码（平台无自选密码入口，注册即固定）
      PW = "Abcd1234!"
      # 平台唯一收信域
      DOMAIN = "shadowmail.win"
      # 本渠道凭据串前缀
      TOKEN_PREFIX = "shadowmail|"

      HEADERS = {
        "Content-Type" => "application/json",
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      module_function

      # 生成随机注册邮箱前缀（sdk+8 位小写字母）
      # @return [String]
      def random_account
        chars = "abcdefghijklmnopqrstuvwxyz"
        "sdk" + Array.new(8) { chars[rand(chars.length)] }.join
      end

      # 携带显式 Cookie 的 JSON POST 请求，返回 [body, status, set_cookies]
      # @param path [String] 请求路径
      # @param body [Hash] JSON 请求体
      # @param cookie [String] Cookie 头（可为空）
      # @return [Array(Array, Integer, Array<String>)]
      def do_post(path, body, cookie)
        headers = HEADERS.dup
        headers["Cookie"] = cookie unless cookie.to_s.empty?
        resp = Http.post("#{BASE_URL}#{path}", headers: headers, json: body, timeout: 15)
        [resp.json, resp.status_code, resp.set_cookies]
      end

      # 从 Set-Cookie 中提取 sessionId 值（纯 uuid，不含键名）
      # @param cookies [Array<String>]
      # @return [String]
      def session_from(cookies)
        cookies.each do |line|
          kv = line.split(";").first.to_s.strip
          return kv.split("=", 2)[1].to_s if kv.start_with?("sessionId=")
        end
        ""
      end

      # 注册或登录（POST /api/register、/api/login），返回会话 sessionId
      # @param account [String] 注册邮箱
      # @param password [String] 密码
      # @param is_login [Boolean] true 走登录，false 走注册
      # @return [String] sessionId 值
      def register_login(account, password, is_login)
        path = is_login ? "/api/login" : "/api/register"
        body = { "email" => account, "password" => password }
        data, status, cookies = do_post(path, body, "")
        raise "shadowmail #{path}: http #{status}" if status < 200 || status >= 300

        msg = data.is_a?(Hash) ? data["message"].to_s : ""
        if is_login
          raise "shadowmail login: #{msg}" unless msg == "Successfull Login"
        else
          # 重复注册（幂等）：消息为 Email already in use 时视为账号已存在，继续走登录
          unless ["Successfully Registered", "Email already in use"].include?(msg)
            raise "shadowmail register: #{msg}"
          end
        end
        session = session_from(cookies)
        if is_login && session.empty?
          raise "shadowmail login: 未下发 sessionId Cookie"
        end
        session
      end

      # 注册账号并创建临时邮箱地址
      # @return token 凭据串格式："shadowmail|<account>|<password>|<sessionId>"
      # @return [EmailInfo]
      def generate_email
        account = "#{random_account}@gmail.com"

        # 1) 注册（幂等：已存在同名账号则跳过）
        register_login(account, PW, false)
        # 2) 登录取得 sessionId
        session = register_login(account, PW, true)
        # 3) 创建地址（每账号 12 槽）：SDK 层显式 Cookie 头传 sessionId
        data, status, = do_post("/api/new-address", {}, "sessionId=#{session}")
        raise "shadowmail new-address: http #{status}" if status < 200 || status >= 300

        address = data.is_a?(Hash) ? data["address"].to_s.strip : ""
        raise "shadowmail new-address: 响应缺少有效地址" unless address.end_with?("@#{DOMAIN}")

        # Token 持久化：account|password|sessionId（sessionId 为 uuid，无分隔符冲突）
        token = TOKEN_PREFIX + [account, PW, session].join("|")
        EmailInfo.new(channel: CHANNEL, email: address.downcase.strip, token: token)
      end

      # 解析凭据串为 account/password/sessionId 三元组
      # @param token [String]
      # @return [Array(String, String, String)]
      def parse_token(token)
        raise "shadowmail: token 格式错误" unless token.to_s.start_with?(TOKEN_PREFIX)

        parts = token.to_s[TOKEN_PREFIX.length..].to_s.split("|", -1)
        raise "shadowmail: token 字段缺失" unless parts.length == 3

        account, password, session = parts
        raise "shadowmail: token 凭据字段为空" if account.empty? || password.empty? || session.empty?

        [account, password, session]
      end

      # 读取收件箱
      # 会话失效时自动以凭据内 account/password 重新登录换新 sessionId。
      # @param email [String] 平台新地址（<id>@shadowmail.win）
      # @param token [String] 建箱下发的凭据串
      # @return [Array<Email>]
      def get_emails(email, token)
        account, password, session = parse_token(token)
        addr = email.to_s.strip

        do_fetch = lambda do
          do_post("/api/get-emails", { "address" => addr }, "sessionId=#{session}")
        end

        data, status, = do_fetch.call
        # sessionId 最长 1 小时（Max-Age 3600），过期后重登录重试一次
        if status == 401 || status == 404
          new_session = register_login(account, password, true)
          unless new_session.empty?
            session = new_session
            data, status, = do_fetch.call
          end
        end
        raise "shadowmail get-emails: http #{status}" if status < 200 || status >= 300

        msg = data.is_a?(Hash) ? data["message"].to_s : ""
        raise "shadowmail get-emails: #{msg}" unless msg == "Emails read"

        mails = data.is_a?(Hash) ? data["mails"] : nil
        return [] unless mails.is_a?(Array)

        mails.filter_map do |m|
          next unless m.is_a?(Hash)

          flat = m.dup
          flat["from"] = m["sender"]
          flat["to"] = addr
          flat["date"] = m["created_at"]
          # 平台无 text/html 区分，body 为正文（默认按纯文本处理，普通化可按需互转）
          flat["text"] = m["body"]
          Normalize.normalize_email(flat, addr)
        end
      end
    end
  end
end