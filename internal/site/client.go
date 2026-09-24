package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"newapi-checkin/internal/config"
)

// defaultUserAgent 用浏览器 UA：不少中转站前面有 WAF/Cloudflare，默认 UA 会被直接拦掉。
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// maxBodyBytes 限制读取的响应体大小，防止异常站点把内存打满。
const maxBodyBytes = 1 << 20

// Options 构造站点客户端所需的参数。
type Options struct {
	Name       string
	BaseURL    string
	Credential Credential
	Headers    map[string]string
	Timeout    time.Duration
	UserAgent  string
	// CheckinPath 覆盖签到接口路径；为空时用 config.DefaultCheckinPath。
	CheckinPath string
	// SolveWAF 为 true 时，遇到可离线求解的前置防护挑战（阿里云 acw_sc__v2）会自动算 Cookie 并重放。
	SolveWAF bool
	// HTTPClient 允许注入自定义客户端（测试用）。
	HTTPClient *http.Client
}

// Client 是单个 new-api 站点的客户端。
type Client struct {
	name        string
	base        *url.URL
	cred        Credential
	extra       http.Header
	timeout     time.Duration
	hc          *http.Client
	checkinPath string
	solveWAF    bool
	solvedWAF   bool
}

// NewClient 构造客户端。
func NewClient(opts Options) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(opts.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("站点 %s 的 base_url 无法解析: %w", opts.Name, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("站点 %s 的 base_url 缺少协议或主机名: %q", opts.Name, opts.BaseURL)
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	extra := http.Header{}
	extra.Set("Accept", "application/json, text/plain, */*")
	extra.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	ua := strings.TrimSpace(opts.UserAgent)
	if ua == "" {
		ua = defaultUserAgent
	}
	extra.Set("User-Agent", ua)

	origin := base.Scheme + "://" + base.Host
	extra.Set("Referer", origin+"/")
	extra.Set("Origin", origin)

	// 用户自定义头最后应用，可以覆盖上面任何默认值（例如换 UA 过 WAF）。
	for k, v := range opts.Headers {
		if strings.TrimSpace(k) == "" {
			continue
		}
		extra.Set(k, v)
	}

	// 自己持有 http.Client：需要挂 Cookie Jar —— 通过前置防护挑战后，服务端下发的 acw_tc /
	// cdn_sec_tc 与算出来的 acw_sc__v2 都要在后续请求里带上。
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConnsPerHost:   2,
			ForceAttemptHTTP2:     true,
		},
		Jar: jar,
	}
	if opts.HTTPClient != nil {
		// 尊重调用方注入的客户端（测试用于控制超时/传输层），但 Cookie Jar 由我们持有。
		if opts.HTTPClient.Timeout > 0 {
			hc.Timeout = opts.HTTPClient.Timeout
		}
		if opts.HTTPClient.Transport != nil {
			hc.Transport = opts.HTTPClient.Transport
		}
		hc.CheckRedirect = opts.HTTPClient.CheckRedirect
	}

	// 部分 fork 把签到接口挪到别的路径（例如 /api/user/sign_in），允许按站点覆盖。
	checkinPath := strings.TrimSpace(opts.CheckinPath)
	if checkinPath == "" {
		checkinPath = config.DefaultCheckinPath
	}
	checkinPath = "/" + strings.TrimLeft(checkinPath, "/")

	return &Client{
		name:        opts.Name,
		base:        base,
		cred:        opts.Credential,
		extra:       extra,
		timeout:     timeout,
		hc:          hc,
		checkinPath: checkinPath,
		solveWAF:    opts.SolveWAF,
	}, nil
}

// NewClientFromSite 用配置构造客户端。
func NewClientFromSite(s config.Site, app config.App) (*Client, error) {
	return NewClient(Options{
		Name:        s.Name,
		BaseURL:     s.BaseURL,
		Credential:  NewCredential(s.Credential),
		Headers:     s.Headers,
		Timeout:     app.Timeout(),
		CheckinPath: s.CheckinPath,
		SolveWAF:    app.WAFChallenge != config.WAFChallengeOff,
	})
}

// Name 返回站点名。
func (c *Client) Name() string { return c.name }

// BaseURL 返回站点根地址。
func (c *Client) BaseURL() string { return c.base.String() }

// Credential 返回当前使用的凭据（只读，用于诊断展示）。
func (c *Client) Credential() Credential { return c.cred }

// WithCredential 复制一个使用其它凭据的客户端（--probe 用它试不同组合）。
func (c *Client) WithCredential(cred Credential) *Client {
	clone := *c
	clone.cred = cred
	return &clone
}

// Status 是 GET /api/status 里我们关心的字段。
type Status struct {
	SystemName     string
	Version        string
	QuotaPerUnit   float64
	CheckinEnabled bool
	TurnstileCheck bool
	// SupportsCheckin 表示站点返回了 checkin_enabled 字段（即站点明确声明了签到开关）。
	SupportsCheckin bool
	// CheckinSupportUnknown 表示站点响应里没有 checkin_enabled 字段。
	// 有些 fork（例如签到接口被改成 /api/user/sign_in 的站点）的 /api/status 就不返回它，
	// 这并不代表不能签到，因此按「未声明」处理并继续尝试，而不是判站点失败。
	CheckinSupportUnknown bool
}

// Self 是 GET /api/user/self 里我们关心的字段。
type Self struct {
	ID        int64
	Username  string
	Quota     int64
	UsedQuota int64
}

// CheckinStats 是"签到状态查询"（GET 签到接口）的统计信息。
type CheckinStats struct {
	CheckedInToday  bool
	TotalQuota      int64
	TotalCheckins   int64
	MonthlyCheckins int
	// Raw 是站点响应的原始片段（截断、压平空白），仅用于日志展示"站点到底回了什么"。
	Raw string
}

// CheckinResult 是"提交签到"（POST 签到接口）的结果。
type CheckinResult struct {
	QuotaAwarded int64
	CheckinDate  string
	Message      string
	// Raw 是站点响应的原始片段（截断、压平空白），仅用于日志展示"站点到底回了什么"。
	Raw string
}

// response 是一次 HTTP 往返的原始结果。
type response struct {
	status  int
	header  http.Header
	body    []byte
	latency time.Duration
}

// envelope 是 new-api 统一的 {"success":bool,"message":string,"data":...} 包装。
type envelope struct {
	Success bool
	Message string
	Data    json.RawMessage
	raw     map[string]any
}

// dataMap 把 data 解析成 map；data 不是对象时返回空 map。
func (e *envelope) dataMap() map[string]any {
	if len(e.Data) == 0 {
		return nil
	}
	return decodeObject(e.Data)
}

// Status 读取公开的 /api/status，用来判断站点是否 new-api、是否开启签到。
func (c *Client) Status(ctx context.Context) (*Status, error) {
	const op = "GET /api/status"
	env, _, err := c.request(ctx, http.MethodGet, "/api/status", nil, false)
	if err != nil {
		return nil, err
	}

	merged := map[string]any{}
	for k, v := range env.raw {
		merged[k] = v
	}
	for k, v := range env.dataMap() {
		merged[k] = v
	}

	status := &Status{
		SystemName:   asString(merged["system_name"]),
		Version:      asString(merged["version"]),
		QuotaPerUnit: asFloat(merged["quota_per_unit"]),
	}
	if v, ok := merged["checkin_enabled"]; ok {
		status.SupportsCheckin = true
		status.CheckinEnabled = asBool(v)
	}
	if v, ok := merged["turnstile_check"]; ok {
		status.TurnstileCheck = asBool(v)
	}

	if !status.SupportsCheckin {
		// 连 version/system_name 都没有，才认为「不像 new-api」。
		if status.Version == "" && status.SystemName == "" {
			return nil, NewError(KindNotNewAPI, op,
				"站点响应里既没有 checkin_enabled 也没有 version/system_name，不像 new-api 站点", nil)
		}
		// 是 new-api（或 fork），但没声明签到开关：按「开启」继续尝试，
		// 真正的结论交给签到请求本身——旧版无签到接口的站点会在那里 404，
		// 而 fork 改过签到接口的站点则能正常签到（真机实测就有这种站点）。
		status.CheckinSupportUnknown = true
		status.CheckinEnabled = true
	}
	return status, nil
}

// Self 读取当前账号信息，用于校验凭据是否有效以及获取签到前后余额。
func (c *Client) Self(ctx context.Context) (*Self, error) {
	const op = "GET /api/user/self"
	env, _, err := c.request(ctx, http.MethodGet, "/api/user/self", nil, true)
	if err != nil {
		return nil, err
	}
	if !env.Success {
		// /api/user/self 需要登录态，因此任何业务失败都按凭据问题处理（除非更具体的分类）。
		kind := classifyMessage(env.Message)
		if kind == KindBusiness {
			kind = KindAuthFailed
		}
		return nil, &Error{Kind: kind, Op: op, Message: env.Message, HTTPStatus: 200}
	}
	data := env.dataMap()
	if len(data) == 0 {
		return nil, NewError(KindDecode, op, "响应中没有 data 字段", nil)
	}
	return &Self{
		ID:        asInt64(data["id"]),
		Username:  asString(data["username"]),
		Quota:     asInt64(data["quota"]),
		UsedQuota: asInt64(data["used_quota"]),
	}, nil
}

// CheckinStatus 读取当月签到状态。
func (c *Client) CheckinStatus(ctx context.Context, month string) (*CheckinStats, error) {
	op := "GET " + c.checkinPath
	if strings.TrimSpace(month) == "" {
		month = time.Now().Format("2006-01")
	}
	query := url.Values{"month": {month}}
	env, resp, err := c.request(ctx, http.MethodGet, c.checkinPath, query, true)
	if err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, &Error{Kind: classifyMessage(env.Message), Op: op, Message: env.Message, HTTPStatus: 200}
	}

	stats := &CheckinStats{Raw: rawSnippet(resp)}
	data := env.dataMap()
	if rawStats, ok := data["stats"]; ok {
		if statsMap := asMap(rawStats); statsMap != nil {
			stats.CheckedInToday = asBool(statsMap["checked_in_today"])
			stats.TotalQuota = asInt64(statsMap["total_quota"])
			stats.TotalCheckins = asInt64(statsMap["total_checkins"])
			stats.MonthlyCheckins = asInt64SliceLen(statsMap["records"])
		}
	} else {
		return nil, NewError(KindDecode, op, "响应 data 中缺少 stats 字段", nil)
	}
	return stats, nil
}

// DoCheckin 提交签到。注意接口无请求体。
func (c *Client) DoCheckin(ctx context.Context) (*CheckinResult, error) {
	op := "POST " + c.checkinPath
	env, resp, err := c.request(ctx, http.MethodPost, c.checkinPath, nil, true)
	if err != nil {
		return nil, err
	}
	if !env.Success {
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			// 少数 fork 失败时也不给 message，此时把原始响应带上，否则日志里只剩一句空话。
			msg = "站点返回 success=false 但没有 message，原始响应: " + rawSnippet(resp)
		}
		return nil, &Error{Kind: classifyMessage(msg), Op: op, Message: msg, HTTPStatus: 200}
	}

	result := &CheckinResult{Message: env.Message, Raw: rawSnippet(resp)}
	data := env.dataMap()
	result.QuotaAwarded = asInt64(data["quota_awarded"])
	result.CheckinDate = asString(data["checkin_date"])
	if result.CheckinDate == "" {
		result.CheckinDate = time.Now().Format("2006-01-02")
	}
	return result, nil
}

// request 执行一次请求并解析响应包装。
// request 发一次请求；遇到可离线求解的前置防护挑战（阿里云 acw_sc__v2）时会自动算出通行
// Cookie 并把同一个请求重放一次，因此调用方不必关心这类挑战。
func (c *Client) request(ctx context.Context, method, path string, query url.Values, useCred bool) (*envelope, *response, error) {
	op := method + " " + path

	target := *c.base
	target.Path = strings.TrimRight(c.base.Path, "/") + path
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}

	env, result, err := c.doRequest(ctx, op, &target, method, useCred)
	if err == nil {
		return env, result, nil
	}
	if c.solveWAF && result != nil {
		if arg1, ok := extractACWArg1(result.body); ok {
			if value := acwScV2(arg1); value != "" {
				c.rememberWAFCookie(&target, value)
				c.solvedWAF = true
				return c.doRequest(ctx, op, &target, method, useCred)
			}
		}
	}
	return env, result, err
}

// stripCookie 从 Cookie 头里删掉指定名字的 Cookie；删空时连头一起去掉。
func stripCookie(h http.Header, name string) {
	raw := h.Get("Cookie")
	if raw == "" {
		return
	}
	kept := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ";") {
		kv := strings.TrimSpace(part)
		if kv == "" {
			continue
		}
		if key, _, ok := strings.Cut(kv, "="); ok && strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		kept = append(kept, kv)
	}
	if len(kept) == 0 {
		h.Del("Cookie")
		return
	}
	h.Set("Cookie", strings.Join(kept, "; "))
}

// WAFSolved 表示本次会话里是否成功自动通过了前置防护挑战（供日志说明用）。
func (c *Client) WAFSolved() bool { return c.solvedWAF }

// rememberWAFCookie 把算出的通行 Cookie 放进 Cookie Jar；后续请求会自动带上。
func (c *Client) rememberWAFCookie(target *url.URL, value string) {
	if c.hc == nil || c.hc.Jar == nil {
		return
	}
	c.hc.Jar.SetCookies(target, []*http.Cookie{{Name: acwCookieName, Value: value, Path: "/"}})
}

// doRequest 真正发请求并做响应分类（不含挑战重放）。
func (c *Client) doRequest(ctx context.Context, op string, target *url.URL, method string, useCred bool) (*envelope, *response, error) {
	var body io.Reader
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, nil, NewError(KindNetwork, op, "构造请求失败", err)
	}
	for k, vs := range c.extra {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	if useCred {
		c.cred.Apply(req.Header)
		if c.solvedWAF {
			// 凭据里可能粘贴了过期的 WAF 通行 Cookie（acw_sc__v2）。同名 Cookie 在同一个头里
			// 出现两次时服务端通常取第一个，也就是过期那个，会把我们刚算出来的值废掉。
			stripCookie(req.Header, acwCookieName)
		}
	}

	start := time.Now()
	resp, err := c.hc.Do(req)
	latency := time.Since(start)
	if err != nil {
		kind, message := classifyTransportError(err, c.timeout)
		return nil, nil, &Error{Kind: kind, Op: op, Message: message, Err: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	result := &response{status: resp.StatusCode, header: resp.Header, body: raw, latency: latency}
	if readErr != nil {
		return nil, result, NewError(KindNetwork, op, "读取响应失败", readErr)
	}

	if kind, decided := classifyHTTPStatus(resp.StatusCode); decided && resp.StatusCode >= 400 {
		// 少数 fork 用 HTTP 400 表达"今日已签到"这类非失败结果（上游 new-api 用 200 + success:false）。
		// 这类响应体仍是合法 JSON：若直接按"业务失败"处理，会把实际已签到的站点记成失败并推送告警，
		// 用户却怎么重试都"修不好"。因此这里先看响应体能否给出明确的非失败语义。
		if kind == KindBusiness {
			if env, perr := parseEnvelope(raw); perr == nil {
				switch classifyMessage(env.Message) {
				case KindAlreadyCheckedIn, KindCheckinDisabled, KindTurnstile:
					return env, result, nil
				}
			}
		}

		msg := snippet(raw)
		if resp.StatusCode == http.StatusTooManyRequests {
			msg = "触发限流 (HTTP 429)"
		}
		// 403 且返回 HTML：几乎都是 Cloudflare/WAF 挑战页，而不是凭据问题。
		// 若按"凭据失效"上报，用户会反复重取 Cookie 却永远修不好。
		if resp.StatusCode == http.StatusForbidden && looksLikeHTML(raw) {
			if hint, blocked := blockedHint(resp, raw, "HTTP 403 返回 HTML"); blocked {
				kind = KindBlocked
				msg = hint
			} else {
				kind = KindNotNewAPI
				msg = "被 WAF/Cloudflare 拦截（HTTP 403 返回 HTML）: " + msg
			}
		}
		return nil, result, &Error{
			Kind:       kind,
			Op:         op,
			Message:    msg,
			HTTPStatus: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	env, err := parseEnvelope(raw)
	if err != nil {
		// 先判断是不是"被前置防护拦截"：这类响应重试多少次都不会变成 JSON，
		// 必须换成带通行 Cookie 的请求，所以单独分类并让调用方不要重试。
		if hint, blocked := blockedHint(resp, raw, ""); blocked {
			return nil, result, &Error{
				Kind:       KindBlocked,
				Op:         op,
				Message:    hint,
				HTTPStatus: resp.StatusCode,
				Err:        err,
			}
		}
		return nil, result, &Error{
			Kind:       KindDecode,
			Op:         op,
			Message:    decodeHint(raw),
			HTTPStatus: resp.StatusCode,
			Err:        err,
		}
	}
	return env, result, nil
}

// parseEnvelope 解析 {"success":...,"message":...,"data":...}，同时保留原始 map。
func parseEnvelope(raw []byte) (*envelope, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("响应体为空")
	}

	env := &envelope{}
	if err := json.Unmarshal(trimmed, env); err != nil {
		return nil, err
	}
	env.raw = decodeObject(trimmed)
	if env.raw == nil {
		return nil, errors.New("响应不是 JSON 对象")
	}
	return env, nil
}

// decodeObject 用 UseNumber 解析 JSON 对象，避免大整数额度被 float64 截断。
func decodeObject(raw []byte) map[string]any {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil
	}
	return out
}

// classifyTransportError 区分超时与其它网络错误。
func classifyTransportError(err error, timeout time.Duration) (Kind, string) {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
		return KindNetwork, fmt.Sprintf("请求超时（超过 %s）", timeout)
	case errors.Is(err, context.Canceled):
		return KindNetwork, "请求被取消"
	default:
		return KindNetwork, "网络错误"
	}
}

// strongChallengeHints 是"只会在挑战页里出现"的强特征，不要求响应里还有 <script。
var strongChallengeHints = []string{
	"just a moment", "cf_chl_opt", "cdn-cgi/challenge-platform",
	"enable javascript and cookies to continue", "var arg1=",
}

// jsChallengeHints 是通用 JS 混淆特征，必须与 <script 同时出现才算，
// 否则普通 HTML 页面（只要写了 location.href）也会被误判成"被拦截"。
var jsChallengeHints = []string{
	"document.cookie", "location.href", "location.reload",
	"window.location", "navigator.useragent", "settimeout(", "_0x",
}

// blockedHint 判断响应是否来自"站点前置的 WAF/防护"的拦截，并给出可操作的提示。
//
// 触发特征（任一命中即算）：
//   - 边缘节点的拒绝标记：x-tengine-error（阿里云 ESA/Tengine）、cf-mitigated（Cloudflare）
//   - server 头是 ESA，或 Set-Cookie 里出现阿里云 WAF 的挑战 Cookie（acw_tc / cdn_sec_tc / acw_sc__v2）
//   - 响应体是 HTML 且含挑战特征（just a moment / var arg1= / <script + document.cookie 等）
//
// 为什么要单独分类（实测教训）：阿里云 WAF 的 JS 挑战返回的是 HTTP 200 + HTML，
// 若归入"响应无法解析为 JSON（可重试）"，会白等三次指数退避仍然失败；
// 若归入"非 new-api 站点"，又会误导用户去怀疑 base_url。两者都不如直说"被拦了、要通行 Cookie"。
func blockedHint(resp *http.Response, raw []byte, cause string) (string, bool) {
	if resp == nil {
		return "", false
	}
	lowerBody := strings.ToLower(string(raw))

	var who string
	switch {
	case strings.TrimSpace(resp.Header.Get("x-tengine-error")) != "":
		who = "阿里云 ESA/Tengine（" + strings.TrimSpace(resp.Header.Get("x-tengine-error")) + "）"
	case strings.Contains(strings.ToLower(resp.Header.Get("Server")), "esa"):
		who = "阿里云 ESA"
	case strings.TrimSpace(resp.Header.Get("cf-mitigated")) != "":
		who = "Cloudflare（cf-mitigated: " + strings.TrimSpace(resp.Header.Get("cf-mitigated")) + "）"
	}
	if who == "" {
		for _, c := range resp.Header.Values("Set-Cookie") {
			lc := strings.ToLower(c)
			if strings.Contains(lc, "acw_tc") || strings.Contains(lc, "cdn_sec_tc") || strings.Contains(lc, "acw_sc__v2") {
				who = "阿里云 WAF"
				break
			}
		}
	}

	challenge := looksLikeHTML(raw) && (containsAnyFold(lowerBody, strongChallengeHints) ||
		(strings.Contains(lowerBody, "<script") && containsAnyFold(lowerBody, jsChallengeHints)))
	if who == "" && !challenge {
		return "", false
	}
	if who == "" {
		who = "站点前置的防护/反爬"
	}

	var sb strings.Builder
	if cause != "" {
		fmt.Fprintf(&sb, "被 %s 拦截（%s）", who, cause)
	} else {
		fmt.Fprintf(&sb, "被 %s 拦截（HTTP %d，返回的不是 JSON）", who, resp.StatusCode)
	}
	sb.WriteString("：请求没有到达 new-api，属于前置防护的 JS 挑战/边缘拒绝，重试也不会成功。")
	sb.WriteString("（阿里云 WAF 的 acw_sc__v2 挑战本工具会自动求解后重放；这里仍报这条，说明不是那种挑战，或站点还校验了别的特征。）")
	sb.WriteString("处理方式：在浏览器里打开该站并通过挑战，然后 F12 → Network → 复制任意请求的整段 Cookie")
	sb.WriteString("（通常含 acw_sc__v2 / cf_clearance 等通行 Cookie，必须连同 session 一起整段粘贴），填进 credential.cookie；")
	sb.WriteString("再把 headers.User-Agent 设成与取 Cookie 时相同的浏览器 UA。")
	sb.WriteString("注意通行 Cookie 一般与 IP/UA 绑定且有有效期，过期后需要重新获取。原始响应：")
	sb.WriteString(snippet(raw))
	return sb.String(), true
}

// decodeHint 在响应不是 JSON 时给出可操作的提示（最常见的成因是 WAF 拦截页）。
func decodeHint(raw []byte) string {
	text := snippet(raw)
	if looksLikeHTML(raw) {
		return "站点返回的是 HTML 而不是 JSON（可能是反向代理/网关的错误页，或 base_url 填错）: " + text
	}
	return "响应无法解析为 JSON: " + text
}

// rawSnippet 返回响应的原始片段，用于在日志里展示站点返回了什么。
func rawSnippet(resp *response) string {
	if resp == nil || len(resp.body) == 0 {
		return ""
	}
	return snippet(resp.body)
}

// snippet 截取响应片段用于错误提示。
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return s
}

// parseRetryAfter 解析 Retry-After 响应头，支持秒数与 HTTP-date 两种形式。
func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil {
		if seconds <= 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	// HTTP-date 形式（RFC 7231）：换算成距当前的时长，已过期则视为 0。
	if at, err := http.ParseTime(header); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return 0
}

// ---- JSON 值转换工具（额度等数字可能以字符串或整数形式出现） ----

func asMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes":
			return true
		default:
			return false
		}
	case json.Number:
		n, err := t.Int64()
		return err == nil && n != 0
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	default:
		return false
	}
}

func asInt64(v any) int64 {
	switch t := v.(type) {
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
		if f, err := t.Float64(); err == nil {
			return int64(f)
		}
		return 0
	case float64:
		return int64(t)
	case int:
		return int64(t)
	case int64:
		return t
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return int64(f)
		}
		return 0
	default:
		return 0
	}
}

func asFloat(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, _ := t.Float64()
		return f
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	default:
		return 0
	}
}

// asInt64SliceLen 返回数组长度（用于统计本月签到记录条数）。
func asInt64SliceLen(v any) int {
	if list, ok := v.([]any); ok {
		return len(list)
	}
	return 0
}

// looksLikeHTML 判断响应体是不是 HTML（WAF/Cloudflare 挑战页的典型特征）。
func looksLikeHTML(raw []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(raw)), "<")
}
