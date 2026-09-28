# frozen_string_literal: true

require "digest"
require "uri"

module TempmailSdk
  module Providers
    # Nukemail 渠道实现（nukemail.app）
    #
    # 完整接入契约：
    #   挑战 GET /api/pow/challenge?difficulty=4 → 200 {"id","challenge","difficulty"}；
    #     求 nonce 使 SHA-256(challenge+nonce) 十六进制前 4 位为 0
    #     （前端 solvePow 逐 nonce 自 0 递增）。
    #   建箱 POST /api/inbox/create body {"address","domain","pow_id","pow_nonce"}
    #     → 200 {"token":"NUKE-xxxxxxxx","email":"名@域名"}，并
    #     Set-Cookie: nukemail_token=<token>（Secure; HttpOnly; SameSite=lax, 72h）。
    #   读信 GET /api/inbox（Cookie: nukemail_token=<token>）→ 200
    #     {"token","state","addresses":[...],"messages":[...],"is_premium"...}；
    #     messages 元素字段为 sender、sender_name、subject、body_html、body_text、
    #     received_at、read。
    #   会话恢复 POST /api/inbox/resume {"accessCode":<token>} 重设 Cookie，仅作兜底。
    # 会话隔离：nukemail_token 由生成结果持久化为 token，读信时以显式 Cookie 头携带。
    module Nukemail
      CHANNEL = "nukemail"
      BASE_URL = "https://nukemail.app"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) " \
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
      }.freeze

      module_function

      # 求解 PoW：返回使 SHA-256(challenge+nonce) 十六进制
      # 前 difficulty 位为 0 的最小 nonce（算法与前端 solvePow 逐字对齐）。
      # @param challenge [String] 挑战串
      # @param difficulty [Integer] 难度（十六进制前缀 0 位数）
      # @return [Integer] nonce
      def solve_pow(challenge, difficulty)
        prefix = "0" * difficulty
        nonce = 0
        loop do
          digest = Digest::SHA256.hexdigest("#{challenge}#{nonce}")
          break if digest.start_with?(prefix)

          nonce += 1
        end
        nonce
      end

      # 生成本地随机名（与前端 generateRandomName 等价形态）
      # @return [String]
      def random_address
        chars = "abcdefghijklmnopqrstuvwxyz0123456789"
        "nuke" + Array.new(10) { chars[rand(chars.length)] }.join
      end

      # 取第一个非 premium 的活跃域名
      # @return [String]
      def default_domain
        resp = Http.get("#{BASE_URL}/api/domains", headers: HEADERS, timeout: 15)
        raise "nukemail generate: domains http #{resp.status_code}" unless resp.ok?

        data = resp.json
        domains = data.is_a?(Hash) ? data["domains"] : nil
        raise "nukemail generate: domains 响应非对象" unless domains.is_a?(Array)

        domains.each do |d|
          next unless d.is_a?(Hash)

          dom = d["domain"].to_s.strip
          return dom if !dom.empty? && d["is_premium_only"] != true
        end
        raise "nukemail generate: 无可用非 premium 域名"
      end

      # 创建临时邮箱（PoW 建箱）
      # token 为平台返回的 NUKE-<随机> 访问码，读信时转成 nukemail_token Cookie 携带。
      # @return [EmailInfo]
      def generate_email
        # 1) 取 PoW 挑战
        resp = Http.get("#{BASE_URL}/api/pow/challenge?difficulty=4",
                        headers: HEADERS, timeout: 15)
        raise "nukemail generate: challenge http #{resp.status_code}" unless resp.ok?

        ch = resp.json
        raise "nukemail generate: challenge 响应非对象" unless ch.is_a?(Hash)

        ch_id = ch["id"].to_s.strip
        challenge = ch["challenge"].to_s.strip
        raise "nukemail generate: challenge 响应缺少 id/challenge" if ch_id.empty? || challenge.empty?

        difficulty = ch["difficulty"].to_i
        difficulty = 4 if difficulty <= 0

        # 2) 本地求 PoW 解（SHA-256 前缀 4 零）
        nonce = solve_pow(challenge, difficulty)

        # 3) 取域名并建箱
        dom = default_domain
        body = {
          "address" => random_address,
          "domain" => dom,
          "pow_id" => ch_id,
          "pow_nonce" => nonce.to_s
        }
        headers = {
          "Content-Type" => "application/json",
          "Accept" => "application/json",
          "User-Agent" => HEADERS["User-Agent"]
        }
        resp2 = Http.post("#{BASE_URL}/api/inbox/create",
                          headers: headers, json: body, timeout: 15)
        unless resp2.ok?
          raise "nukemail generate: create http #{resp2.status_code}"
        end
        data = resp2.json
        raise "nukemail generate: create 响应非对象" unless data.is_a?(Hash)

        token = data["token"].to_s.strip
        email = data["email"].to_s.strip
        raise "nukemail generate: create 响应缺少 token/email" if token.empty? || email.empty?

        EmailInfo.new(channel: CHANNEL, email: email, token: token)
      end

      # 读取收件箱
      # 主通道 GET /api/inbox 带 Cookie: nukemail_token=<token>；
      # 会话过期时经 POST /api/inbox/resume 显式重设会话后重试一次。
      # @param email [String] 邮箱地址
      # @param token [String] 建箱返回的 NUKE-<随机> 访问码
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        tk = token.to_s.strip
        raise "nukemail: token 为空" if tk.empty?

        cookie = "nukemail_token=#{tk}"

        fetch = lambda do
          resp = Http.get("#{BASE_URL}/api/inbox",
                          headers: HEADERS.merge("Cookie" => cookie), timeout: 15)
          raise "nukemail 读信: http #{resp.status_code}" unless resp.ok?

          resp.json
        end

        data = fetch.call
        raise "nukemail 读信: 响应非对象" unless data.is_a?(Hash)

        if data["state"].to_s.empty? || data["state"] == "expired"
          # 会话可能已过期：经 resume 重设会话后重试
          headers = {
            "Content-Type" => "application/json",
            "User-Agent" => HEADERS["User-Agent"]
          }
          begin
            Http.post("#{BASE_URL}/api/inbox/resume",
                      headers: headers, json: { "accessCode" => tk }, timeout: 15)
          rescue StandardError
            nil
          end
          data = fetch.call
          raise "nukemail 读信: 响应非对象" unless data.is_a?(Hash)
        end

        messages = data["messages"]
        return [] unless messages.is_a?(Array)

        messages.filter_map do |m|
          next unless m.is_a?(Hash)

          flat = m.dup
          flat["to"] = addr
          # 平台消息字段为 body_html/body_text，补齐 text/html 候选（若原字段缺失）
          flat["text"] = flat["body_text"] if flat["text"].nil?
          flat["html"] = flat["body_html"] if flat["html"].nil?
          flat["date"] = m["received_at"]
          flat["read"] = m["read"]
          # sender 是小写发件人地址，sender_name 是展示名
          flat["sender_email"] = m["sender"]
          Normalize.normalize_email(flat, addr)
        end
      end
    end
  end
end