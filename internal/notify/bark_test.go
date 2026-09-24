package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"newapi-checkin/internal/config"
)

// testBarkConfig 构造一份可用的 Bark 配置，server 由假服务端地址填充。
func testBarkConfig(server string) config.Bark {
	return config.Bark{
		Enabled: true,
		Server:  server,
		Key:     "testkey",
		Group:   "签到",
		Level:   "active",
		Icon:    "https://example.com/icon.png",
		Sound:   "bell",
		Mode:    config.BarkModeSummary,
	}
}

// barkCapture 记录假 Bark 服务端最近一次收到的请求。
type barkCapture struct {
	method      string
	path        string
	contentType string
	payload     map[string]any
}

// newBarkTestServer 启动一个假 Bark 服务端，固定返回 status+body，并暴露捕获到的请求。
func newBarkTestServer(t *testing.T, status int, body string) (*httptest.Server, *barkCapture) {
	t.Helper()
	captured := &barkCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.contentType = r.Header.Get("Content-Type")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读取请求体失败: %v", err)
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &captured.payload); err != nil {
				t.Errorf("请求体不是合法 JSON: %v，原始内容 %s", err, raw)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// TestBarkEnabled 覆盖 key 为空 / enabled=false / 配置齐全 三种情况。
func TestBarkEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Bark
		want bool
	}{
		{name: "key 为空", cfg: config.Bark{Enabled: true, Server: "https://api.day.app", Key: ""}, want: false},
		{name: "key 只有空白", cfg: config.Bark{Enabled: true, Server: "https://api.day.app", Key: "   "}, want: false},
		{name: "enabled=false", cfg: config.Bark{Enabled: false, Server: "https://api.day.app", Key: "testkey"}, want: false},
		{name: "server 为空", cfg: config.Bark{Enabled: true, Server: "", Key: "testkey"}, want: false},
		{name: "配置齐全", cfg: config.Bark{Enabled: true, Server: "https://api.day.app", Key: "testkey"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewBark(tc.cfg, nil).Enabled(); got != tc.want {
				t.Fatalf("Enabled() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestBarkSendRequestShape 校验正常推送时的请求方法、路径、Content-Type 与请求体字段。
func TestBarkSendRequestShape(t *testing.T) {
	srv, captured := newBarkTestServer(t, http.StatusOK, `{"code":200,"message":"success"}`)
	cfg := testBarkConfig(srv.URL)
	bark := NewBark(cfg, nil)

	msg := Message{Title: "签到结果", Body: "全部成功", Group: "自定义分组", Level: "timeSensitive"}
	if err := bark.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send 返回错误: %v", err)
	}

	if captured.method != http.MethodPost {
		t.Errorf("请求方法 = %q，期望 POST", captured.method)
	}
	if captured.path != "/"+cfg.Key {
		t.Errorf("请求路径 = %q，期望 %q", captured.path, "/"+cfg.Key)
	}
	if !strings.Contains(captured.contentType, "application/json") {
		t.Errorf("Content-Type = %q，期望包含 application/json", captured.contentType)
	}
	for field, want := range map[string]any{
		"title": msg.Title,
		"body":  msg.Body,
		"group": msg.Group,
		"level": msg.Level,
	} {
		if got := captured.payload[field]; got != want {
			t.Errorf("payload[%q] = %v，期望 %v", field, got, want)
		}
	}

	// 配置里的 server 末尾带 "/" 时，路径也应拼接成 /<key>，不能出现双斜杠。
	cfgWithSlash := testBarkConfig(srv.URL + "/")
	if err := NewBark(cfgWithSlash, nil).Send(context.Background(), msg); err != nil {
		t.Fatalf("server 带结尾斜杠时 Send 返回错误: %v", err)
	}
	if captured.path != "/"+cfg.Key {
		t.Errorf("server 带结尾斜杠时请求路径 = %q，期望 %q", captured.path, "/"+cfg.Key)
	}
}

// TestBarkSendFallsBackToConfig 校验 Message 的 level/group 为空时回落到配置值。
func TestBarkSendFallsBackToConfig(t *testing.T) {
	srv, captured := newBarkTestServer(t, http.StatusOK, `{"code":200}`)
	cfg := testBarkConfig(srv.URL)

	if err := NewBark(cfg, nil).Send(context.Background(), Message{Title: "标题", Body: "正文"}); err != nil {
		t.Fatalf("Send 返回错误: %v", err)
	}
	if got := captured.payload["level"]; got != cfg.Level {
		t.Errorf("Message.Level 为空时 level = %v，期望回落到配置值 %q", got, cfg.Level)
	}
	if got := captured.payload["group"]; got != cfg.Group {
		t.Errorf("Message.Group 为空时 group = %v，期望回落到配置值 %q", got, cfg.Group)
	}
}

// TestBarkSendResponseCodes 覆盖业务码 400、HTTP 500 与业务码 200 三种返回。
func TestBarkSendResponseCodes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "业务码 400", status: http.StatusOK, body: `{"code":400,"message":"bad key"}`, wantErr: true},
		{name: "HTTP 500", status: http.StatusInternalServerError, body: `internal error`, wantErr: true},
		{name: "业务码 200", status: http.StatusOK, body: `{"code":200,"message":"success"}`, wantErr: false},
		{name: "业务码 0", status: http.StatusOK, body: `{"code":0}`, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newBarkTestServer(t, tc.status, tc.body)
			err := NewBark(testBarkConfig(srv.URL), nil).
				Send(context.Background(), Message{Title: "标题", Body: "正文"})
			if tc.wantErr && err == nil {
				t.Fatalf("期望返回错误，实际为 nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望无错误，实际为 %v", err)
			}
		})
	}
}

// TestBarkSendTruncatesLongBody 校验超长中文正文不报错且被截断到上限以内。
func TestBarkSendTruncatesLongBody(t *testing.T) {
	srv, captured := newBarkTestServer(t, http.StatusOK, `{"code":200}`)
	body := strings.Repeat("签", 3000)

	if err := NewBark(testBarkConfig(srv.URL), nil).
		Send(context.Background(), Message{Title: "超长正文", Body: body}); err != nil {
		t.Fatalf("超长正文不应导致错误: %v", err)
	}

	got, _ := captured.payload["body"].(string)
	if got == "" {
		t.Fatalf("payload.body 为空")
	}
	if n := len([]rune(got)); n > maxBarkBody {
		t.Errorf("正文长度 %d 超过上限 %d", n, maxBarkBody)
	}
	if got == body {
		t.Errorf("正文未被截断，长度仍为 %d", len([]rune(body)))
	}
}

// TestMultiDisabledChannels 覆盖 Multi 在存在/不存在可用渠道时的行为。
func TestMultiDisabledChannels(t *testing.T) {
	disabled := NewBark(config.Bark{Enabled: false, Server: "https://api.day.app", Key: "testkey"}, nil)
	enabled := NewBark(testBarkConfig("https://api.day.app"), nil)

	allDisabled := NewMulti(nil, disabled)
	if allDisabled.Enabled() {
		t.Errorf("只有未启用渠道时 Enabled() 应为 false")
	}
	if names := allDisabled.Names(); len(names) != 0 {
		t.Errorf("只有未启用渠道时 Names() 应为空，实际 %v", names)
	}
	if err := allDisabled.Notify(context.Background(), Message{Title: "标题"}); err != nil {
		t.Errorf("没有可用渠道时 Notify 应返回 nil，实际 %v", err)
	}

	none := NewMulti(nil)
	if none.Enabled() {
		t.Errorf("没有任何渠道时 Enabled() 应为 false")
	}
	if err := none.Notify(context.Background(), Message{Title: "标题"}); err != nil {
		t.Errorf("没有任何渠道时 Notify 应返回 nil，实际 %v", err)
	}

	mixed := NewMulti(nil, disabled, enabled)
	if !mixed.Enabled() {
		t.Errorf("存在启用渠道时 Enabled() 应为 true")
	}
	names := mixed.Names()
	if len(names) != 1 || names[0] != "bark" {
		t.Errorf("Names() = %v，期望只包含启用的 [bark]", names)
	}
}

// TestBarkSendDisabledMakesNoRequest 校验未启用时 Send 直接返回且不发出任何 HTTP 请求。
func TestBarkSendDisabledMakesNoRequest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":200,"message":"success"}`)
	}))
	t.Cleanup(srv.Close)

	cases := []struct {
		name string
		cfg  config.Bark
	}{
		{"key 为空", config.Bark{Enabled: true, Server: srv.URL, Key: ""}},
		{"key 只有空白", config.Bark{Enabled: true, Server: srv.URL, Key: "   "}},
		{"enabled=false", config.Bark{Enabled: false, Server: srv.URL, Key: "testkey"}},
		{"server 为空", config.Bark{Enabled: true, Server: "", Key: "testkey"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := NewBark(tc.cfg, nil).Send(context.Background(), Message{Title: "标题", Body: "正文"}); err != nil {
				t.Fatalf("未启用时 Send 应返回 nil，实际 %v", err)
			}
		})
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("未启用时不应发出任何请求，实际收到 %d 次", got)
	}
}

// TestBarkSendEmptyBodyIsSuccess 200 + 空响应体应视为成功。
//
// 部分自建 Bark / 反向代理会这样返回；若判为失败就会释放通知去重键，
// 导致同一条提醒在后续每次运行时被反复推送。
func TestBarkSendEmptyBodyIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	bark := NewBark(config.Bark{Enabled: true, Server: srv.URL, Key: "testkey"}, nil)
	if err := bark.Send(context.Background(), Message{Title: "标题", Body: "正文"}); err != nil {
		t.Errorf("200 + 空响应体应视为成功，实际 %v", err)
	}
}

// TestBarkSendNonJSONBodyIsError 200 + 非空且非 JSON（例如 HTML 错误页）必须判为失败，
// 否则去重键会被永久占用、当天的提醒再也推不出去。
func TestBarkSendNonJSONBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}))
	t.Cleanup(srv.Close)

	bark := NewBark(config.Bark{Enabled: true, Server: srv.URL, Key: "testkey"}, nil)
	if err := bark.Send(context.Background(), Message{Title: "标题", Body: "正文"}); err == nil {
		t.Error("200 + HTML 应判为失败")
	}
}
