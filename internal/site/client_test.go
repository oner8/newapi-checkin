package site

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"newapi-checkin/internal/config"
)

// fakeAPI 是一个可控的假 new-api 站点。
type fakeAPI struct {
	mu sync.Mutex

	checkinEnabled bool
	turnstile      bool
	checkedToday   bool
	quota          int64
	award          int64

	selfFailures int    // 前 N 次 /api/user/self 返回 500
	authFail     bool   // 认证接口统一返回「未登录」
	postMessage  string // 覆盖 POST 返回的 message
	statusBody   string // 覆盖 /api/status 的响应体
	statusCode   int    // /api/status 的 HTTP 状态码
	postCode     int    // POST 的 HTTP 状态码
	retryAfter   string

	selfCalls int
	postCalls int
	lastAuth  http.Header
}

func (f *fakeAPI) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.statusCode != 0 {
			w.WriteHeader(f.statusCode)
		}
		if f.statusBody != "" {
			_, _ = w.Write([]byte(f.statusBody))
			return
		}
		writeJSON(w, map[string]any{
			"success": true,
			"data": map[string]any{
				"version":         "v1.0.0-rc.40",
				"system_name":     "测试站",
				"checkin_enabled": f.checkinEnabled,
				"turnstile_check": f.turnstile,
				"quota_per_unit":  500000,
			},
		})
	})

	mux.HandleFunc("/api/user/self", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.selfCalls++
		f.lastAuth = r.Header.Clone()

		if f.selfFailures > 0 {
			f.selfFailures--
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if f.authFail {
			writeJSON(w, map[string]any{"success": false, "message": "未登录，请先登录"})
			return
		}
		writeJSON(w, map[string]any{
			"success": true,
			"data": map[string]any{
				"id":         7,
				"username":   "tester",
				"quota":      f.quota,
				"used_quota": 123,
			},
		})
	})

	mux.HandleFunc("/api/user/checkin", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastAuth = r.Header.Clone()

		if f.authFail {
			writeJSON(w, map[string]any{"success": false, "message": "未登录，请先登录"})
			return
		}

		if r.Method == http.MethodPost {
			f.postCalls++
			if f.retryAfter != "" {
				w.Header().Set("Retry-After", f.retryAfter)
			}
			if f.postCode != 0 {
				w.WriteHeader(f.postCode)
				_, _ = w.Write([]byte(`{"success":false,"message":"boom"}`))
				return
			}
			if f.postMessage != "" {
				writeJSON(w, map[string]any{"success": false, "message": f.postMessage})
				return
			}
			f.quota += f.award
			f.checkedToday = true
			writeJSON(w, map[string]any{
				"success": true,
				"message": "签到成功",
				"data": map[string]any{
					"quota_awarded": f.award,
					"checkin_date":  "2026-09-23",
				},
			})
			return
		}

		writeJSON(w, map[string]any{
			"success": true,
			"data": map[string]any{
				"stats": map[string]any{
					"checked_in_today": f.checkedToday,
					"total_quota":      f.quota,
					"total_checkins":   3,
					"records":          []any{map[string]any{"checkin_date": "2026-09-22"}},
				},
			},
		})
	})

	return mux
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func newFakeServer(t *testing.T, api *fakeAPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, baseURL string, cred Credential) *Client {
	t.Helper()
	client, err := NewClient(Options{
		Name:       "测试站",
		BaseURL:    baseURL,
		Credential: cred,
		Timeout:    3 * time.Second,
	})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	return client
}

func cookieCred() Credential {
	return Credential{Type: config.CredentialCookie, Cookie: "session=test-session"}
}

func TestStatusParsesFields(t *testing.T) {
	api := &fakeAPI{checkinEnabled: true, turnstile: true}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status 失败: %v", err)
	}
	if !status.CheckinEnabled || !status.TurnstileCheck {
		t.Errorf("布尔字段解析错误: %+v", status)
	}
	if status.Version != "v1.0.0-rc.40" || status.SystemName != "测试站" {
		t.Errorf("版本/站点名解析错误: %+v", status)
	}
	if status.QuotaPerUnit != 500000 {
		t.Errorf("quota_per_unit 解析错误: %v", status.QuotaPerUnit)
	}
}

// TestStatusWithoutCheckinField 覆盖「是 new-api/fork，但 /api/status 不返回 checkin_enabled」。
//
// 这不代表不能签到（fork 可能把签到接口改成了自己的路径，例如 /api/user/sign_in），
// 因此按「未声明」处理并继续尝试，由签到请求本身给出结论，而不是在这里判站点失败。
func TestStatusWithoutCheckinField(t *testing.T) {
	api := &fakeAPI{statusBody: `{"success":true,"data":{"version":"v0.3.0","system_name":"旧站"}}`}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("缺少 checkin_enabled 不应报错（交给签到请求判定），实际 %v", err)
	}
	if !status.CheckinSupportUnknown {
		t.Error("应标记为「未声明签到开关」")
	}
	if !status.CheckinEnabled {
		t.Error("未声明时应按开启处理，以便继续尝试签到")
	}
	if status.Version != "v0.3.0" || status.SystemName != "旧站" {
		t.Errorf("版本信息解析错误: %+v", status)
	}
}

// TestStatusNotNewAPIWithoutAnyMarker 覆盖真正的「不像 new-api」：既没有 checkin_enabled，也没有版本信息。
func TestStatusNotNewAPIWithoutAnyMarker(t *testing.T) {
	api := &fakeAPI{statusBody: `{"success":true,"data":{"foo":"bar"}}`}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	if _, err := client.Status(context.Background()); !IsKind(err, KindNotNewAPI) {
		t.Fatalf("没有任何 new-api 特征时应判 not_newapi，实际 %v（%v）", KindOf(err), err)
	}
}

func TestStatusHTMLResponse(t *testing.T) {
	// 用中性 HTML（不含挑战页特征）：这条测的是「非 JSON 的 HTML」这条通用路径，
	// 真正的 WAF/挑战页由 TestBlockedPageDetection 覆盖。
	api := &fakeAPI{statusBody: "<html><body><h1>503 Service Unavailable</h1></body></html>"}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	_, err := client.Status(context.Background())
	if !IsKind(err, KindDecode) {
		t.Fatalf("HTML 响应应判为 decode: %v", err)
	}
	if !Retryable(err) {
		t.Error("decode 错误应当可重试")
	}
	if got := err.Error(); !strings.Contains(got, "HTML") {
		t.Errorf("错误信息应当提示可能是 WAF 拦截: %s", got)
	}
}

func TestSelfSuccess(t *testing.T) {
	api := &fakeAPI{quota: 500000}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	self, err := client.Self(context.Background())
	if err != nil {
		t.Fatalf("Self 失败: %v", err)
	}
	if self.ID != 7 || self.Username != "tester" || self.Quota != 500000 || self.UsedQuota != 123 {
		t.Errorf("Self 字段解析错误: %+v", self)
	}
	if got := api.lastAuth.Get("Cookie"); got != "session=test-session" {
		t.Errorf("请求未带上 Cookie: %q", got)
	}
}

func TestSelfUnauthorized(t *testing.T) {
	api := &fakeAPI{authFail: true}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	_, err := client.Self(context.Background())
	if !IsKind(err, KindAuthFailed) {
		t.Fatalf("应当判为凭据失效: %v", err)
	}
	if Retryable(err) {
		t.Error("凭据失效不应重试")
	}
}

func TestSelfHTTP401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"success":false,"message":"未授权"}`))
	}))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL, cookieCred())
	_, err := client.Self(context.Background())
	if !IsKind(err, KindAuthFailed) {
		t.Fatalf("401 应判为凭据失效: %v", err)
	}
	if e := AsError(err); e == nil || e.HTTPStatus != 401 {
		t.Errorf("应当记录 HTTP 状态码: %+v", AsError(err))
	}
}

func TestAccessTokenHeaders(t *testing.T) {
	api := &fakeAPI{checkinEnabled: true}
	cred := Credential{Type: config.CredentialAccessToken, Token: "sk-token", NewAPIUser: "42"}
	client := newTestClient(t, newFakeServer(t, api).URL, cred)

	if _, err := client.Self(context.Background()); err != nil {
		t.Fatalf("Self 失败: %v", err)
	}
	if got := api.lastAuth.Get("Authorization"); got != "Bearer sk-token" {
		t.Errorf("Authorization 头不对: %q", got)
	}
	if got := api.lastAuth.Get("New-Api-User"); got != "42" {
		t.Errorf("New-Api-User 头不对: %q", got)
	}
}

func TestCheckinStatusParsed(t *testing.T) {
	api := &fakeAPI{checkinEnabled: true, checkedToday: true, quota: 1234}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	stats, err := client.CheckinStatus(context.Background(), "2026-09")
	if err != nil {
		t.Fatalf("CheckinStatus 失败: %v", err)
	}
	if !stats.CheckedInToday {
		t.Error("checked_in_today 应为 true")
	}
	if stats.TotalQuota != 1234 || stats.TotalCheckins != 3 || stats.MonthlyCheckins != 1 {
		t.Errorf("统计字段解析错误: %+v", stats)
	}
}

func TestDoCheckinSuccess(t *testing.T) {
	api := &fakeAPI{checkinEnabled: true, award: 10000, quota: 500000}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	result, err := client.DoCheckin(context.Background())
	if err != nil {
		t.Fatalf("DoCheckin 失败: %v", err)
	}
	if result.QuotaAwarded != 10000 || result.CheckinDate != "2026-09-23" {
		t.Errorf("签到结果解析错误: %+v", result)
	}
}

func TestDoCheckinBusinessErrors(t *testing.T) {
	cases := []struct {
		name          string
		message       string
		want          Kind
		wantRetryable bool
	}{
		{"今日已签到", "今日已签到", KindAlreadyCheckedIn, false},
		{"Turnstile 拦截", "Turnstile token 为空", KindTurnstile, false},
		{"Turnstile 校验失败", "Turnstile 校验失败，请刷新重试！", KindTurnstile, false},
		{"未开启签到", "签到功能未启用", KindCheckinDisabled, false},
		{"凭据失效", "未登录，请先登录", KindAuthFailed, false},
		// 站点自己让我们稍后重试：这属于可恢复的服务端抖动，必须可重试，
		// 否则一次瞬时故障就会白白放弃当天的签到。
		{"站点要求稍后重试", "签到失败，请稍后重试", KindServer, true},
		{"系统繁忙", "系统繁忙，请稍后再试", KindServer, true},
		// 真正的业务错误重试也不会成功，因此不可重试。
		{"其他业务错误", "余额不足", KindBusiness, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{checkinEnabled: true, postMessage: tc.message}
			client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

			_, err := client.DoCheckin(context.Background())
			if !IsKind(err, tc.want) {
				t.Fatalf("期望分类 %v，实际 %v（%v）", tc.want, KindOf(err), err)
			}
			if got := Retryable(err); got != tc.wantRetryable {
				t.Errorf("Retryable = %v，期望 %v（%v）", got, tc.wantRetryable, err)
			}
		})
	}
}

// TestDoCheckinHTTP400BodyIsInspected 覆盖"fork 用 400 表达非失败结果"的场景。
//
// 上游 new-api 的 DoCheckin 一律返回 HTTP 200（"今日已签到"也是 200 + success:false），
// 但部分 fork 会用 400 携带同样的 JSON。若把整个 4xx 直接当"意图明确的业务失败"，
// 已经签到成功的站点会被记成失败并推送告警，用户重试也永远修不好。
func TestDoCheckinHTTP400BodyIsInspected(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		want  Kind
		retry bool
	}{
		{"400 + 已签到", `{"success":false,"message":"今日已签到"}`, KindAlreadyCheckedIn, false},
		{"400 + 未开启签到", `{"success":false,"message":"签到功能未启用"}`, KindCheckinDisabled, false},
		{"400 + Turnstile", `{"success":false,"message":"Turnstile 校验失败"}`, KindTurnstile, false},
		{"400 + 真实业务错误", `{"success":false,"message":"参数错误"}`, KindBusiness, false},
		{"400 + 非 JSON（WAF/网关页面）", `<html><body>400 Bad Request</body></html>`, KindBusiness, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)

			client := newTestClient(t, srv.URL, cookieCred())
			_, err := client.DoCheckin(context.Background())
			if !IsKind(err, tc.want) {
				t.Fatalf("期望分类 %v，实际 %v（%v）", tc.want, KindOf(err), err)
			}
			if got := Retryable(err); got != tc.retry {
				t.Errorf("Retryable = %v，期望 %v（%v）", got, tc.retry, err)
			}
		})
	}
}

func TestDoCheckinServerErrorIsRetryable(t *testing.T) {
	api := &fakeAPI{checkinEnabled: true, postCode: http.StatusInternalServerError}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	_, err := client.DoCheckin(context.Background())
	if !IsKind(err, KindServer) {
		t.Fatalf("5xx 应判为服务端错误: %v", err)
	}
	if !Retryable(err) {
		t.Error("5xx 应当可重试")
	}
}

func TestDoCheckinRateLimitedKeepsRetryAfter(t *testing.T) {
	api := &fakeAPI{checkinEnabled: true, postCode: http.StatusTooManyRequests, retryAfter: "7"}
	client := newTestClient(t, newFakeServer(t, api).URL, cookieCred())

	_, err := client.DoCheckin(context.Background())
	if !IsKind(err, KindServer) {
		t.Fatalf("429 应判为可重试的服务端错误: %v", err)
	}
	if e := AsError(err); e == nil || e.RetryAfter != 7*time.Second {
		t.Errorf("应当解析 Retry-After: %+v", AsError(err))
	}
}

func TestRequestTimeoutIsNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		writeJSON(w, map[string]any{"success": true})
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(Options{Name: "慢站", BaseURL: srv.URL, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	_, err = client.Status(context.Background())
	if !IsKind(err, KindNetwork) {
		t.Fatalf("超时应判为网络错误: %v", err)
	}
	if !Retryable(err) {
		t.Error("超时应当可重试")
	}
}

func TestSubPathDeployment(t *testing.T) {
	// new-api 部署在子路径下时，/api/... 应当拼在子路径之后。
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, map[string]any{"success": true, "data": map[string]any{"checkin_enabled": true}})
	}))
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL+"/panel", cookieCred())
	if _, err := client.Status(context.Background()); err != nil {
		t.Fatalf("Status 失败: %v", err)
	}
	if gotPath != "/panel/api/status" {
		t.Errorf("子路径拼接错误: %q", gotPath)
	}
}

// TestSelfGenericFailureIsAuthFailure 验证兜底分类：当 /api/user/self 返回 success=false
// 但文案里没有任何可识别关键词时，仍必须判为「凭据失效」。
//
// 这条分支容易被漏掉：老版本与各类 fork 的提示文案五花八门（甚至被自定义中间件换成
// "请求被拒绝"）。若降级成普通业务错误，用户只会看到含糊的失败原因，不会意识到
// 该重新复制 Cookie 了 —— 而这恰恰是这个服务最需要提醒的情况。
func TestSelfGenericFailureIsAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"success": false, "message": "请求被拒绝"})
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL, cookieCred())
	self, err := client.Self(context.Background())
	if err == nil {
		t.Fatalf("success=false 时应当返回错误，实际拿到 %+v", self)
	}
	if kind := KindOf(err); kind != KindAuthFailed {
		t.Errorf("应分类为 auth_failed，实际 %s（错误: %v）", kind, err)
	}
	if Retryable(err) {
		t.Error("凭据失效不应被判为可重试，否则会白白重试并放大站点压力")
	}
}

// TestForbiddenWithHTMLIsWAF 验证 403 的两种含义被区分开：
// 返回 HTML（WAF/Cloudflare 挑战页）→ not_newapi；返回 JSON（真的没权限/没登录）→ auth_failed。
func TestForbiddenWithChallengeIsBlocked(t *testing.T) {
	waf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><head><title>Just a moment...</title></head></html>"))
	}))
	t.Cleanup(waf.Close)

	_, err := newTestClient(t, waf.URL, cookieCred()).Self(context.Background())
	if !IsKind(err, KindBlocked) {
		t.Fatalf("403 + Cloudflare 拦截页应判为 blocked，实际 %v（%v）", KindOf(err), err)
	}
	if Retryable(err) {
		t.Error("被拦截不应重试")
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]any{"success": false, "message": "无权进行此操作"})
	}))
	t.Cleanup(denied.Close)

	_, err = newTestClient(t, denied.URL, cookieCred()).Self(context.Background())
	if !IsKind(err, KindAuthFailed) {
		t.Fatalf("403 + JSON 应判为凭据失效，实际 %v（%v）", KindOf(err), err)
	}
}

// TestRetryAfterHTTPDate 验证 Retry-After 支持 HTTP-date 形式，而不只是秒数。
func TestRetryAfterHTTPDate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", time.Now().Add(20*time.Second).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"success":false,"message":"rate limited"}`))
	}))
	t.Cleanup(srv.Close)

	_, err := newTestClient(t, srv.URL, cookieCred()).Status(context.Background())
	e := AsError(err)
	if e == nil {
		t.Fatalf("应当返回分类错误，实际 %v", err)
	}
	if e.RetryAfter <= 0 || e.RetryAfter > 25*time.Second {
		t.Errorf("HTTP-date 形式的 Retry-After 应解析为正的等待时长，实际 %v", e.RetryAfter)
	}
}

// TestRetryAfterParsing 覆盖秒数、非法值等边界。
func TestRetryAfterParsing(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{"", 0},
		{"-5", 0},
		{"not-a-date", 0},
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.in); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v，期望 %v", tc.in, got, tc.want)
		}
	}
}

// TestBlockedPageDetection 覆盖「请求被站点前置 WAF/防护拦截」的识别。
//
// 这类响应的共同点：HTTP 可能仍是 200，但返回的是 JS 挑战页或边缘拒绝，请求根本没到 new-api。
// 必须单独分类且不可重试——否则会白等三次指数退避（实测阿里云 WAF 就是 200 + 挑战页），
// 或者被误报成「非 new-api 站点」，让用户跑去检查 base_url。
func TestBlockedPageDetection(t *testing.T) {
	aliyun := `<html><script>var arg1='EECC88D1399A38572569D5939F1FBC11C2A3EF4D';(function(a,c){var G=a0j,d=a();while(!![]){try{document.cookie='acw_sc__v2='+G(0x117)}catch(e){}}})();</script></html>`

	cases := []struct {
		name    string
		headers map[string]string
		body    string
		want    Kind
		retry   bool
	}{
		{"阿里云 ESA 拒绝（x-tengine-error）", map[string]string{"x-tengine-error": "denied by http_custom"}, aliyun, KindBlocked, false},
		{"阿里云 WAF 挑战页（无边缘头，仅 JS 特征）", nil, aliyun, KindBlocked, false},
		{"阿里云 WAF（只有 acw_tc Cookie 是线索）", map[string]string{"Set-Cookie": "acw_tc=abc; Path=/"}, "<html><body>denied</body></html>", KindBlocked, false},
		{"Cloudflare 拦截（cf-mitigated）", map[string]string{"cf-mitigated": "challenge"}, "<html><body>blocked</body></html>", KindBlocked, false},
		{"server 头是 ESA", map[string]string{"Server": "ESA"}, "<html><body>blocked</body></html>", KindBlocked, false},
		{"普通网关 HTML 错误页：仍按可重试的解析失败处理", nil, "<html><body><h1>502 Bad Gateway</h1></body></html>", KindDecode, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers, body := tc.headers, tc.body
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range headers {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)

			_, err := newTestClient(t, srv.URL, cookieCred()).DoCheckin(context.Background())
			if !IsKind(err, tc.want) {
				t.Fatalf("期望分类 %v，实际 %v（%v）", tc.want, KindOf(err), err)
			}
			if got := Retryable(err); got != tc.retry {
				t.Errorf("Retryable = %v，期望 %v（%v）", got, tc.retry, err)
			}
			if tc.want == KindBlocked && !strings.Contains(err.Error(), "通行 Cookie") {
				t.Errorf("拦截提示应给出可操作建议（整段 Cookie + UA），实际: %v", err)
			}
		})
	}
}

// TestCustomCheckinPathUsed 验证 checkin_path 覆盖后，查状态与提交都打到该路径，
// 且响应里没有 data/额度字段时也能正常解析（额度由调用方按余额差补齐）。
func TestCustomCheckinPathUsed(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()

		if r.URL.Path != "/api/user/sign_in" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			writeJSON(w, map[string]any{"success": true, "message": "签到成功，获得 $25 额度"})
			return
		}
		// 该 fork 没有签到状态查询接口。
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(Options{
		Name:        "fork",
		BaseURL:     srv.URL,
		Credential:  cookieCred(),
		Timeout:     3 * time.Second,
		CheckinPath: "/api/user/sign_in",
	})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	result, err := client.DoCheckin(context.Background())
	if err != nil {
		t.Fatalf("DoCheckin 失败: %v", err)
	}
	if result.Message != "签到成功，获得 $25 额度" {
		t.Errorf("消息解析错误: %q", result.Message)
	}
	if result.QuotaAwarded != 0 {
		t.Errorf("响应没有 quota_awarded 时应为 0，实际 %d", result.QuotaAwarded)
	}
	if !strings.Contains(result.Raw, "签到成功，获得 $25 额度") {
		t.Errorf("应当保留站点原始响应片段（供日志展示），实际 %q", result.Raw)
	}

	// 状态查询打到同一路径，站点回 404 → 业务类失败（调用方据此降级，不判站点失败）。
	if _, err := client.CheckinStatus(context.Background(), "2026-09"); !IsKind(err, KindBusiness) {
		t.Errorf("GET 该路径应得到 404 业务失败，实际 %v（%v）", KindOf(err), err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("应当只请求自定义路径两次（POST + GET），实际 %v", seen)
	}
	for _, p := range seen {
		if strings.Contains(p, "/api/user/checkin") {
			t.Errorf("不应再访问默认路径，实际请求过 %s", p)
		}
	}
}

const (
	acwTestArg1  = "EEFA790EB387F45DA18DA984FFBBBFA66EAF0D6E"
	acwTestValue = "6ab47a8d3b0f8b9ef98d608d7a6c860a807be30a"
)

// acwChallengePage 返回一个阿里云 WAF 的 JS 挑战页（HTTP 200 + HTML）。
func acwChallengePage() string {
	return `<html><script>var arg1='` + acwTestArg1 + `';(function(a,c){var G=a0j,d=a();` +
		`while(!![]){try{document.cookie='acw_sc__v2='+G(0x117)}catch(e){}}})();</script></html>`
}

// TestWAFChallengeSolvedAutomatically 覆盖"自动求解阿里云 WAF 挑战"：
// 第一次请求被挑战页拦下（同时下发 acw_tc / cdn_sec_tc），算出 acw_sc__v2 重放一次即通过。
func TestWAFChallengeSolvedAutomatically(t *testing.T) {
	var mu sync.Mutex
	var cookieHeaders []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		mu.Lock()
		cookieHeaders = append(cookieHeaders, cookie)
		mu.Unlock()

		if strings.Contains(cookie, "acw_sc__v2="+acwTestValue) {
			writeJSON(w, map[string]any{
				"success": true,
				"data":    map[string]any{"id": 7, "username": "tester", "quota": 100},
			})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "acw_tc", Value: "abc", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "cdn_sec_tc", Value: "def", Path: "/"})
		w.Header().Set("x-tengine-error", "denied by http_custom")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(acwChallengePage()))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(Options{
		Name:       "waf",
		BaseURL:    srv.URL,
		Credential: cookieCred(),
		Timeout:    3 * time.Second,
		SolveWAF:   true,
	})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	self, err := client.Self(context.Background())
	if err != nil {
		t.Fatalf("自动求解后应当拿到 JSON，实际 %v", err)
	}
	if self.Username != "tester" {
		t.Errorf("用户信息解析错误: %+v", self)
	}
	if !client.WAFSolved() {
		t.Error("应当标记为已自动通过前置防护挑战")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cookieHeaders) != 2 {
		t.Fatalf("应当发生两次请求（挑战 + 重放），实际 %d 次：%v", len(cookieHeaders), cookieHeaders)
	}
	replay := cookieHeaders[1]
	if !strings.Contains(replay, "acw_sc__v2="+acwTestValue) {
		t.Errorf("重放时应带上算出的 acw_sc__v2: %s", replay)
	}
	if !strings.Contains(replay, "acw_tc=abc") || !strings.Contains(replay, "cdn_sec_tc=def") {
		t.Errorf("重放时应带上服务端在挑战响应里下发的 Cookie: %s", replay)
	}
	if !strings.Contains(replay, "session=test-session") {
		t.Errorf("重放时不能丢掉凭据 Cookie: %s", replay)
	}
}

// TestWAFChallengeNotSolvedWhenDisabled 覆盖 app.waf_challenge=off：不求解，直接报 blocked。
func TestWAFChallengeNotSolvedWhenDisabled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-tengine-error", "denied by http_custom")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(acwChallengePage()))
	}))
	t.Cleanup(srv.Close)

	client, err := NewClient(Options{
		Name:       "waf",
		BaseURL:    srv.URL,
		Credential: cookieCred(),
		Timeout:    3 * time.Second,
		SolveWAF:   false,
	})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	_, err = client.Self(context.Background())
	if !IsKind(err, KindBlocked) {
		t.Fatalf("关闭自动求解时应判为 blocked，实际 %v（%v）", KindOf(err), err)
	}
	if client.WAFSolved() {
		t.Error("关闭自动求解时不应标记为已通过")
	}
	if Retryable(err) {
		t.Error("blocked 不应可重试")
	}
}

// TestStaleWAFCookieDroppedOnSolve 覆盖"凭据里粘贴了过期通行 Cookie"的情形：
// 求解后重放时必须丢掉凭据里的旧 acw_sc__v2，否则同名 Cookie 里服务端会取到过期那个。
func TestStaleWAFCookieDroppedOnSolve(t *testing.T) {
	var mu sync.Mutex
	var cookieHeaders []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie := r.Header.Get("Cookie")
		mu.Lock()
		cookieHeaders = append(cookieHeaders, cookie)
		mu.Unlock()

		if strings.Contains(cookie, "acw_sc__v2="+acwTestValue) {
			writeJSON(w, map[string]any{
				"success": true,
				"data":    map[string]any{"id": 7, "username": "tester", "quota": 100},
			})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(acwChallengePage()))
	}))
	t.Cleanup(srv.Close)

	stale := Credential{Type: config.CredentialCookie, Cookie: "session=abc; acw_sc__v2=stale-value"}
	client, err := NewClient(Options{
		Name:       "waf",
		BaseURL:    srv.URL,
		Credential: stale,
		Timeout:    3 * time.Second,
		SolveWAF:   true,
	})
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	if _, err := client.Self(context.Background()); err != nil {
		t.Fatalf("应当自动求解成功，实际 %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(cookieHeaders) != 2 {
		t.Fatalf("应当发生两次请求，实际 %d 次", len(cookieHeaders))
	}
	replay := cookieHeaders[1]
	if strings.Contains(replay, "stale-value") {
		t.Errorf("重放时不应再带凭据里过期的 acw_sc__v2: %s", replay)
	}
	if !strings.Contains(replay, "acw_sc__v2="+acwTestValue) {
		t.Errorf("重放时应带算出的 acw_sc__v2: %s", replay)
	}
}
