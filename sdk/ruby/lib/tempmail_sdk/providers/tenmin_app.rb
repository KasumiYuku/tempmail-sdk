# frozen_string_literal: true

module TempmailSdk
  module Providers
    # Tenmin.app 渠道实现（真实 API 域 api.tenmin.app）
    #
    # 建箱+读信共用 GET /api/inbox/{localpart}（Accept: application/json）。
    # localpart 为随机 6 位小写十六进制串（首访即建箱，平台无显式创建接口）；
    # 响应：{"inboxId":..,"address":"..@tenmin.app","ttl":600,"count":0,"messages":[]}，
    # messages[] 元素字段：id/from/subject/text/html/receivedAt（from 为 {name,address} 对象）。
    module TenminApp
      CHANNEL = "tenmin-app"
      BASE_URL = "https://api.tenmin.app"
      DEFAULT_DOMAIN = "tenmin.app"

      HEADERS = {
        "Accept" => "application/json",
        "User-Agent" => "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " \
                        "(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36 Edg/146.0.0.0"
      }.freeze

      module_function

      HEX_CHARS = ("0".."9").to_a + ("a".."f").to_a

      # 生成 6 位小写十六进制随机 localpart
      def random_local
        Array.new(6) { HEX_CHARS.sample }.join
      end

      # 请求 /api/inbox/{localpart}
      # @param localpart [String] 邮箱 localpart
      # @return [Hash] 解析后的收件箱响应（解析失败返回空 Hash）
      def fetch_inbox(localpart)
        resp = Http.get("#{BASE_URL}/api/inbox/#{localpart}", headers: HEADERS, timeout: 15)
        resp.raise_for_status
        data = resp.json
        data.is_a?(Hash) ? data : {}
      end

      # 创建 tenmin.app 临时邮箱
      # 首次 GET 随机 localpart 即自动建箱（10 分钟 TTL），token 存储 localpart
      # @return [EmailInfo]
      def generate_email
        local = random_local
        data = fetch_inbox(local)
        address = data["address"].to_s.strip
        address = "#{local}@#{DEFAULT_DOMAIN}" if address.empty?
        ttl = data["ttl"].to_i
        expires_at = nil
        expires_at = ((Time.now.to_f + ttl) * 1000).to_i if ttl.positive?
        EmailInfo.new(channel: CHANNEL, email: address, token: local, expires_at: expires_at)
      end

      # 读取 tenmin.app 收件箱
      # 复用建箱同一 localpart 轮询；from 为 {name,address} 对象，拆开格式化后归一。
      # @param email [String] 邮箱地址
      # @param token [String] localpart
      # @return [Array<Email>]
      def get_emails(email, token)
        local = token.to_s.strip
        raise "tenmin-app: token 为空" if local.empty?

        data = fetch_inbox(local)
        messages = data["messages"]
        return [] unless messages.is_a?(Array)

        addr = email.to_s.strip
        messages.filter_map do |m|
          next unless m.is_a?(Hash)

          from = m["from"]
          # from 为对象（{name,address}）时拆出地址字段
          from_str = if from.is_a?(Hash)
                       fname = from["name"].to_s.strip
                       faddr = from["address"].to_s.strip
                       if !faddr.empty? && !fname.empty?
                         "#{fname} <#{faddr}>"
                       else
                         faddr.empty? ? fname : faddr
                       end
                     else
                       from.to_s
                     end

          row = {
            "id" => m["id"],
            "from" => from_str,
            "to" => addr,
            "subject" => m["subject"],
            "text" => m["text"],
            "html" => m["html"],
            "date" => m["receivedAt"] || m["created_at"] || m["date"]
          }
          Normalize.normalize_email(row, addr)
        end
      end
    end
  end
end