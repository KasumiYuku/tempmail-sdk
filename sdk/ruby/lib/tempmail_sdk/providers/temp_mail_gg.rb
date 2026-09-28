# frozen_string_literal: true

require "cgi"
require "json"
require "time"

module TempmailSdk
  module Providers
    # TempMailGG 渠道实现（temp-mail.gg，Laravel + Livewire v3）
    #
    # 全站无验证码，纯 HTTP 可实现：
    #   - GET https://temp-mail.gg/ 返回 data-csrf 属性令牌与初始
    #     wire:snapshot（JSON；data.email 初始为空，邮箱必须由 Livewire
    #     generateEmail 显式生成）。
    #   - POST https://temp-mail.gg/livewire/update 是唯一边界（同站
    #     fetch 协议）：JSON body 顶层 _token（=data-csrf）+
    #     components[0]：{snapshot, updates:{}, calls:[]}。
    #     generateEmail 生成邮箱并回传新 snapshot。
    #   - 轮询 update（calls 为空 = 平台 20 秒刷信形态）响应
    #     effects.html 含收件箱 UI；逐封 selectEmail 拉详情正文。
    #   - Laravel 每次 livewire/update 轮换会话 Cookie，同值重放会 419，
    #     必须用响应 Set-Cookie 覆写后续请求。
    #
    # 会话粘性：Generate 时把 {email, csrf, snapshot} 打成模块级凭据
    # 状态；token 保存 JSON 凭据串。GetEmails 复用快照轮询/点开详情，
    # 并以响应快照 data.email 断言会话仍指向本邮箱（防串箱）。
    # 本端无全局 Cookie 罐，会话 Cookie 由本渠道模块级维护并逐请求
    # 以显式 Cookie 头回填。邮箱约 30 分钟无活动过期。
    module TempMailGg
      CHANNEL = "temp-mail-gg"
      BASE_URL = "https://temp-mail.gg"

      # 渠道凭据串前缀，用于识别会话接管
      TOKEN_PREFIX = "temp-mail-gg|"

      USER_AGENT = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 " \
                   "(KHTML, like Gecko) Chrome/154.0.0.0 Safari/537.36"

      HOME_ACCEPT = "text/html,application/xhtml+xml,application/xml;q=0.9," \
                    "image/avif,image/webp,*/*;q=0.8"

      # 模块级会话状态：站点 Cookie 键值 + Generate 时打包的凭据串
      # （JSON：{email, csrf, snapshot}，snapshot 为解析后的 Hash）
      @cookies = {}
      @session = nil

      DATA_CSRF_RE = /data-csrf="([^"]*)"/
      SNAPSHOT_RE = /wire:snapshot="([^"]*)"/
      RELATIVE_RE = /^\s*(\d+)\s+(second|minute|hour|day)s?\s+ago\s*$/i

      module_function

      # 解析 Set-Cookie 头，覆写合并进模块级会话 Cookie 状态
      # @param resp [Http::Response]
      def store_cookies(resp)
        resp.set_cookies.each do |line|
          kv = line.split(";").first.to_s.strip
          next if kv.empty?

          k, v = kv.split("=", 2)
          @cookies[k.strip] = v.to_s if k && !k.strip.empty?
        end
      end

      # 由模块级 Cookie 状态拼出显式 Cookie 请求头
      # @return [String, nil] 无 Cookie 时为 nil
      def cookie_header
        return nil if @cookies.empty?

        @cookies.map { |k, v| "#{k}=#{v}" }.join("; ")
      end

      # 组装 livewire/update 同步请求头（同站 fetch 全套）
      # @return [Hash]
      def post_headers
        hdrs = {
          "User-Agent" => USER_AGENT,
          "Accept" => "text/html, application/xhtml+xml",
          "Accept-Language" => "en-US,en;q=0.9",
          "Content-Type" => "application/json",
          "X-Livewire" => "",
          "X-Requested-With" => "XMLHttpRequest",
          "Origin" => BASE_URL,
          "Referer" => "#{BASE_URL}/"
        }
        hdrs["Cookie"] = cookie_header if cookie_header
        hdrs
      end

      # 创建 temp-mail.gg 临时邮箱
      # GET 首页取 CSRF + 初始快照 -> update calls=generateEmail ->
      # 从响应快照取 data.email，并打包 {email, csrf, snapshot} 凭据串。
      # 邮箱约 30 分钟无活动过期。
      # @return [EmailInfo]
      def generate_email
        resp = Http.get(BASE_URL,
                        headers: {
                          "User-Agent" => USER_AGENT,
                          "Accept" => HOME_ACCEPT,
                          "Accept-Language" => "en-US,en;q=0.9"
                        },
                        timeout: 15)
        raise "temp-mail-gg: 首页 http #{resp.status_code}" unless resp.ok?

        store_cookies(resp)
        page = resp.body
        m_csrf = page.match(DATA_CSRF_RE)
        m_snap = page.match(SNAPSHOT_RE)
        if m_csrf.nil? || m_snap.nil?
          raise "temp-mail-gg: 首页缺少 data-csrf 或 wire:snapshot，无法建箱"
        end
        csrf = m_csrf[1]
        # 属性内快照 JSON 被 HTML 实体转义，一层反转义后还原为原始 JSON
        snap_raw = CGI.unescapeHTML(m_snap[1])

        # generateEmail: 平台免费额度为免登录每时段 5 个，耗尽时响应无 email
        update = livewire_update(
          { "snapshot" => snap_raw, "updates" => {}, "calls" => [{ "path" => "", "method" => "generateEmail", "params" => [] }] },
          csrf
        )
        comp = update["components"][0]
        raise "temp-mail-gg: generateEmail 响应异常（components 缺失）" if comp.nil?

        snapshot_text = comp["snapshot"].to_s
        if snapshot_text.strip.empty?
          raise "temp-mail-gg: generateEmail 响应异常（components 缺失）"
        end

        begin
          snapshot = JSON.parse(snapshot_text)
        rescue JSON::ParserError
          raise "temp-mail-gg: 解析 generateEmail 响应快照失败"
        end
        data = snapshot.is_a?(Hash) ? snapshot["data"] : nil
        email = data.is_a?(Hash) ? data["email"].to_s.strip : ""
        if email.empty?
          raise "temp-mail-gg: 建箱失败（响应快照无 email），可能已耗尽免登录配额（每时段 5 个）"
        end

        token = TOKEN_PREFIX + JSON.generate("email" => email, "csrf" => csrf, "snapshot" => snapshot)

        EmailInfo.new(channel: CHANNEL, email: email, token: token,
                      expires_at: ((Time.now.to_f + 30 * 60) * 1000).to_i)
      end

      # 读取 temp-mail.gg 收件箱
      # 流程：校验凭据串 -> 轮询 update（无 calls）取 Inbox 列表 ->
      # 对每封 selectEmail 提取详情正文。
      # @param email [String] 邮箱地址（与 token 内会话邮箱一致才继续）
      # @param token [String] 凭据串（prefix | {email,csrf,snapshot}）
      # @return [Array<Email>]
      def get_emails(email, token)
        addr = email.to_s.strip
        raise "temp-mail-gg: 邮箱为空，请重新 Generate" if addr.empty?

        session = decode_session(token)
        if session["email"].to_s.casecmp(addr) != 0
          raise "temp-mail-gg: 邮箱与凭据不匹配"
        end

        # 轮询刷新（calls 为空 = 平台 20s 自动刷新形态）
        poll = livewire_update(
          { "snapshot" => JSON.generate(session["snapshot"]), "updates" => {}, "calls" => [] },
          session["csrf"]
        )
        comp = poll["components"][0]
        raise "temp-mail-gg: 轮询响应异常（components 缺失）" if comp.nil?

        if comp["snapshot"].to_s.strip != ""
          begin
            poll_snap = JSON.parse(comp["snapshot"])
            if poll_snap.is_a?(Hash)
              current = poll_snap["data"].is_a?(Hash) ? poll_snap["data"]["email"].to_s.strip : ""
              if !current.empty? && current.casecmp(addr) != 0
                raise "temp-mail-gg: 会话已被切换"
              end
            end
          rescue JSON::ParserError
            # 快照解析失败不阻断本轮，透传原快照继续
          end
        end
        html_block = comp.dig("effects", "html").to_s.strip
        if html_block.empty?
          raise "temp-mail-gg: 轮询响应无 effects.html，会话可能已失效"
        end

        rows = parse_list(html_block)
        return [] if rows.empty?

        latest_snap = comp["snapshot"].to_s.strip
        latest_snap = JSON.generate(session["snapshot"]) if latest_snap.empty?

        rows.filter_map do |row|
          next if row[:id].empty?

          build_email(row, addr, session["csrf"], latest_snap)
        end
      end

      # 解析凭据串 -> 会话 Hash（email + csrf + snapshot）
      # @param token [String]
      # @return [Hash]
      def decode_session(token)
        t = token.to_s
        raise "temp-mail-gg: 凭据串前缀不符，请重新 Generate" unless t.start_with?(TOKEN_PREFIX)

        begin
          session = JSON.parse(t[TOKEN_PREFIX.length..])
        rescue JSON::ParserError
          raise "temp-mail-gg: 解析凭据串失败"
        end
        unless session.is_a?(Hash) && !session["email"].to_s.empty? && !session["snapshot"].to_s.empty?
          raise "temp-mail-gg: 凭据串缺失邮箱或快照，请重新 Generate"
        end

        session
      end

      # 调用 livewire/update（响应 Set-Cookie 覆写模块级 Cookie 状态）
      # @param component [Hash] components[0]：{snapshot, updates, calls}
      # @param csrf [String] 顶层 _token
      # @return [Hash] 解析后的响应 JSON
      def livewire_update(component, csrf)
        payload = JSON.generate("_token" => csrf, "components" => [component])
        resp = Http.post("#{BASE_URL}/livewire/update",
                         headers: post_headers, body: payload, timeout: 15)
        if resp.status_code == 419
          raise "temp-mail-gg: livewire 会话过期（419），请重新 Generate"
        end
        raise "temp-mail-gg: livewire/update http #{resp.status_code}" unless resp.ok?

        store_cookies(resp)
        data = resp.json
        raise "temp-mail-gg: livewire/update 响应非对象" unless data.is_a?(Hash)

        data
      end

      # 解析轮询 effects.html 的 Inbox 条目
      # 条目容器 <div wire:click="selectEmail(<数字id>)">，其内
      # h3=发件人、p=主题、p=正文预览、span=相对时间。
      # @param html_block [String]
      # @return [Array<Hash>]
      def parse_list(html_block)
        rows = []
        html_block.scan(/<div[^>]*wire:click="selectEmail\((\d+)\)"[^>]*>(.*?)<\/div>/mi).each do |cap|
          id, chunk = cap
          row = { id: id, from: "", subject: "", preview: "", when: "" }

          # 向内收窄提取：h3（类含 font-semibold）、p（类含 text-zinc-300 / line-clamp-2）、span（类含 text-xs）
          row[:from] = pick_class_text(chunk, "<h3", /font-semibold/)
          row[:subject] = pick_class_text(chunk, "<p", /text-zinc-300/)
          row[:preview] = pick_class_text(chunk, "<p", /line-clamp-2/)
          row[:when] = pick_class_text(chunk, "<span", /text-xs/)

          rows << row
        end
        rows
      end

      # 提取首个 class 含指定模式的标签内容并去 HTML 标签
      # @param html [String] 范围 HTML
      # @param tag [String] 起始标签（如 "<p"）
      # @param class_re [Regexp] class 匹配模式
      # @return [String]
      def pick_class_text(html, tag, class_re)
        m = html.match(/#{Regexp.escape(tag)}\s+class="[^"]*#{class_re.source}[^"]*"[^>]*>(.*?)<\//mi)
        return "" if m.nil?

        HtmlUtils.html_to_text(m[1])
      end

      # 逐封构建 Email：selectEmail 拉详情，失败回退列表字段
      # @param row [Hash] 列表行
      # @param email [String] 收件人
      # @param csrf [String] 凭据 CSRF
      # @param snapshot [String] 当前最新快照 JSON 串（每封详情后滚动更新）
      # @return [Email, nil]
      def build_email(row, email, csrf, snapshot)
        text = ""
        detail = livewire_update(
          { "snapshot" => snapshot, "updates" => {}, "calls" => [{ "path" => "", "method" => "selectEmail", "params" => [row[:id].to_i] }] },
          csrf
        )
        dcomp = detail["components"][0]
        unless dcomp.nil?
          dhtml = dcomp.dig("effects", "html").to_s
          parsed = parse_detail(dhtml)
          row[:from] = parsed[:from] unless parsed[:from].empty?
          row[:subject] = parsed[:subject] unless parsed[:subject].empty?
          text = parsed[:text] if !parsed[:text].empty? || text.empty?
        end

        subject = row[:subject]
        text = subject if text.empty?
        html = "<html><body><pre>#{HtmlUtils.escape(text)}</pre></body></html>"
        date = parse_relative(row[:when])

        Normalize.normalize_email(
          {
            "id" => row[:id],
            "from_email" => row[:from],
            "to" => email,
            "subject" => subject,
            "text" => text,
            "html" => html,
            "date" => date
          },
          email
        )
      rescue StandardError
        nil
      end

      # 解析 selectEmail 详情视图（模态框）：h3=主题、From: 前缀=发件人、
      # x-show 含 activeTab === 'text' 的 div=正文（含标签则去标签）。
      # @param html_block [String]
      # @return [Hash] {from:, subject:, text:}
      def parse_detail(html_block)
        out = { from: "", subject: "", text: "" }
        html_block.scan(/<h3[^>]*>(.*?)<\/h3>/mi).each do |m|
          cls = html_block[/<h3[^>]*class="([^"]*)"[^>]*>/i, 1].to_s
          next if cls.empty?

          next unless cls.split.include?("text-xl")

          out[:subject] = HtmlUtils.html_to_text(m[0])
          break
        end

        html_block.scan(/<span[^>]*>(.*?)<\/span>/mi).each do |m|
          inner = HtmlUtils.html_to_text(m[0])
          next unless inner.start_with?("From:")

          out[:from] = inner.sub(/\AFrom:\s*/i, "").strip
          break
        end

        if (m = html_block.match(%r{<div[^>]*x-show="[^"]*activeTab === 'text'[^"]*"[^>]*>(.*?)</div>}mi))
          inner = m[1]
          inner = HtmlUtils.html_to_text(inner) if inner =~ /<[^>]+>/
          out[:text] = inner.strip
        end
        out
      end

      # 解析相对时间（"N seconds/minutes/hours/days ago"）为 UTC ISO8601
      # @param s [String]
      # @return [String]
      def parse_relative(s)
        m = s.to_s.strip.match(RELATIVE_RE)
        return Time.now.utc.iso8601 if m.nil?

        n = m[1].to_i
        unit = { "second" => 1, "minute" => 60, "hour" => 3600, "day" => 86_400 }[m[2].downcase]
        (Time.now.utc - n * unit).iso8601
      rescue StandardError
        Time.now.utc.iso8601
      end
    end
  end
end