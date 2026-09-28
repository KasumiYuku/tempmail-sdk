# frozen_string_literal: true

require "json"

module TempmailSdk
  module Providers
    # GeneratorEmail 渠道实现（generator.email，PHP SSR 网页型）
    #
    # 流程：
    #   GET /inbox4/ 首页 SSR：解析 window.SITE_DATA 的 cur_user/cur_domain
    #     快照生成邮箱（邮箱=user@domain），同页 Set-Cookie inbox_ctx
    #     选中该邮箱会话
    #   GET /inbox4/（带 inbox_ctx Cookie）收信页：列为 class 含
    #     list-group-item2 的行（平台实测真实类为 list-group-item2，
    #     非 list-group-item），内含 from_div_45g45gg / subj_div_45g45gg /
    #     time_div_45g45gg 三要素块
    # 限制：本站不提供原文正文，按列表三要素归一。
    # token 语义：{email, domain, user} JSON 快照。
    module GeneratorEmail
      CHANNEL = "generator-email"
      BASE_URL = "https://generator.email"
      INBOX_URL = "#{BASE_URL}/inbox4/"

      USER_AGENT = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " \
                   "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

      USER_RE = /cur_user:"([^"]*)"/
      DOMAIN_RE = /cur_domain:"([^"]*)"/
      # 列表条目与三要素块（平台实测真实类为 list-group-item2，按类名后缀锚定）
      ITEM_RE = /<div[^>]*class="[^"]*list-group-item2[^"]*"[^>]*>(.*?)(?:<\/div>\s*){3}/m
      FROM_RE = /class="[^"]*from_div_45g45gg[^"]*"[^>]*>(.*?)<\/div>/m
      SUBJ_RE = /class="[^"]*subj_div_45g45gg[^"]*"[^>]*>(.*?)<\/div>/m
      TIME_RE = /class="[^"]*time_div_45g45gg[^"]*"[^>]*>(.*?)<\/div>/m
      SCRIPT_RE = /<(script|style)[\s\S]*?<\/\1>/i
      TAG_RE = /<[^>]+>/

      module_function

      # 请求收件箱渲染页并返回 HTML（邮箱由服务端依据 inbox_ctx Cookie 选择）
      # @return [String]
      def fetch_page
        headers = {
          "User-Agent" => USER_AGENT,
          "Accept" => "text/html,application/xhtml+xml,application/xml;q=0.9," \
                      "image/avif,image/webp,*/*;q=0.8",
          "Accept-Language" => "en-US,en;q=0.9",
          "Referer" => "#{BASE_URL}/"
        }
        ctx = @inbox_ctx
        headers["Cookie"] = "inbox_ctx=#{ctx}" if ctx && !ctx.empty?
        resp = Http.get(INBOX_URL, headers: headers, timeout: 20)
        # 收下响应 Set-Cookie 的 inbox_ctx 最新值（同页刷新）
        resp.set_cookies.each do |line|
          kv = line.split(";").first.to_s.strip
          @inbox_ctx = kv.delete_prefix("inbox_ctx=") if kv.start_with?("inbox_ctx=")
        end
        resp.raise_for_status
        resp.body.to_s
      end

      # 取正则第一个捕获组，无匹配返回空串
      # @param re [Regexp]
      # @param src [String]
      # @return [String]
      def match_first(re, src)
        m = re.match(src)
        m ? m[1].to_s : ""
      end

      # 去标签与脚本/样式块，压缩空白
      # @param s [String]
      # @return [String]
      def strip_tags(s)
        out = s.gsub(SCRIPT_RE, " ")
        out = out.gsub(TAG_RE, " ")
        out.gsub(/\s+/, " ").strip
      end

      # 创建 generator.email 临时邮箱
      # 解析首页 SITE_DATA 快照（cur_user/cur_domain）得到邮箱地址。
      # @return [EmailInfo]
      def generate_email
        src = fetch_page
        user = match_first(USER_RE, src)
        domain = match_first(DOMAIN_RE, src)
        if user.empty? || domain.empty?
          raise "generator-email: 首页未携带邮箱快照（cur_user/cur_domain）"
        end

        email = "#{user}@#{domain}"
        token = JSON.generate({ email: email, domain: domain, user: user })
        EmailInfo.new(channel: CHANNEL, email: email, token: token)
      end

      # 获取 generator.email 收件箱
      # 解析收件箱渲染页的列表条目（from/subj/time 三要素）；本站不提供
      # 原文正文，SDK 按摘要归一。
      # @param email [String] 邮箱地址
      # @param token [String] 会话凭据 JSON（{email, domain, user}）
      # @return [Array<Email>]
      def get_emails(email, token)
        sess = JSON.parse(token.to_s)
        raise "generator-email: 会话凭据解析失败（非对象）" unless sess.is_a?(Hash)

        raise "generator-email: 会话邮箱与查询邮箱不匹配" if sess["email"] != email

        sess_domain = sess["domain"].to_s
        src = fetch_page
        # 服务端当前渲染邮箱与 token 不一致说明 Cookie 上下文已被切换
        got_domain = match_first(DOMAIN_RE, src)
        if got_domain != sess_domain
          raise "generator-email: 会话域名已切换（token #{sess_domain}，服务端 #{got_domain}）"
        end

        # 列表区域锚定（#email-table ... #markodile 之间）
        list_start = src.index('id="email-table"')
        list_end = src.index('id="markodile"')
        region = if list_start && list_end && list_end > list_start
                   src[list_start...list_end]
                 else
                   ""
                 end

        out = []
        src.scan(ITEM_RE) do |raw|
          raw = raw.to_s
          # 跳过列表容器外的候选：要求条目文本来自列表区域
          next if !region.empty? && !region.include?(raw)

          from = strip_tags(match_first(FROM_RE, raw))
          subject = strip_tags(match_first(SUBJ_RE, raw))
          when_str = strip_tags(match_first(TIME_RE, raw))
          next if from.empty? && subject.empty? && when_str.empty?

          out << Normalize.normalize_email(
            { "from" => from, "to" => email, "subject" => subject, "date" => when_str },
            email
          )
        end
        out
      rescue JSON::ParserError => e
        raise "generator-email: 会话凭据解析失败: #{e.message}"
      end
    end
  end
end