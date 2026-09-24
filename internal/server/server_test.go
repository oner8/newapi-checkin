package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/oner8/newapi-checkin/internal/checkin"
	"github.com/oner8/newapi-checkin/internal/config"
	"github.com/oner8/newapi-checkin/internal/db"
	"github.com/oner8/newapi-checkin/internal/model"
	"github.com/oner8/newapi-checkin/internal/notify"
	"github.com/oner8/newapi-checkin/internal/scheduler"
)

const (
	testAdminToken = "test-admin-token"
	// 站点 A 用 Cookie 凭据，站点 B 用访问令牌凭据（且未启用）。
	siteAName = "站点A"
	siteBName = "站点B"
	// 故意放长的完整密钥，用于断言响应里绝不出现原文。
	cookieSecret = "cookieSESSIONABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	accessToken  = "pat_abcdefghijklmnopqrstuvwxyz0123456789"
)

// fakeService 是 server.Service 的手写假实现，记录调用参数并返回预设结果。
type fakeService struct {
	runID        string
	triggerErr   error
	nextRun      time.Time
	running      bool
	lastRun      *checkin.Summary
	schedule     scheduler.Schedule
	triggerCalls int
	triggerSites []string
}

func (f *fakeService) Trigger(_ context.Context, siteName string) (string, error) {
	f.triggerCalls++
	f.triggerSites = append(f.triggerSites, siteName)
	if f.triggerErr != nil {
		return "", f.triggerErr
	}
	return f.runID, nil
}

func (f *fakeService) NextRun() time.Time           { return f.nextRun }
func (f *fakeService) Running() bool                { return f.running }
func (f *fakeService) LastRun() *checkin.Summary    { return f.lastRun }
func (f *fakeService) Schedule() scheduler.Schedule { return f.schedule }

// testEnv 汇总一次测试的服务端、假 Service 与临时数据库。
type testEnv struct {
	handler http.Handler
	svc     *fakeService
	db      *gorm.DB
	cfg     *config.Config
}

// newTestEnv 用 t.TempDir() 下的临时库构造一份完整的服务端（不监听端口）。
func newTestEnv(t *testing.T, adminToken string) *testEnv {
	t.Helper()

	gdb, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(gdb) })

	cfg := &config.Config{
		App:    config.App{Timezone: "Asia/Shanghai", Schedule: "08:00", DryRun: true},
		Server: config.Server{Enabled: true, Addr: ":0", AdminToken: adminToken},
		Sites: []config.Site{
			{
				Name:    siteAName,
				BaseURL: "https://a.example.com",
				Enabled: true,
				Credential: config.Credential{
					Type:   config.CredentialCookie,
					Cookie: "session=" + cookieSecret,
				},
			},
			{
				Name:    siteBName,
				BaseURL: "https://b.example.com",
				Enabled: false,
				Credential: config.Credential{
					Type:  config.CredentialAccessToken,
					Token: accessToken,
				},
			},
		},
	}

	now := time.Now()
	svc := &fakeService{
		runID:    "run-test-0001",
		nextRun:  now.Add(time.Hour),
		running:  true,
		schedule: scheduler.New(8, 0, 10*time.Minute, time.UTC),
		lastRun: &checkin.Summary{
			RunID:      "run-previous",
			Trigger:    "schedule",
			StartedAt:  now.Add(-time.Minute),
			FinishedAt: now,
			Results: []checkin.SiteResult{
				{SiteName: siteAName, Status: model.StatusSuccess, QuotaAwarded: 500},
				{SiteName: siteBName, Status: model.StatusAuthFailed, Message: "凭据失效"},
			},
		},
	}

	srv := New(Options{
		Addr:       ":0",
		AdminToken: adminToken,
		Config:     cfg,
		DB:         gdb,
		Service:    svc,
		Notifier:   notify.NewMulti(nil),
		Version:    "test",
		StartedAt:  now,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return &testEnv{handler: srv.Handler(), svc: svc, db: gdb, cfg: cfg}
}

// doRequest 用 ResponseRecorder 直接跑一次请求（不监听端口）；remoteAddr 非空时覆盖来源地址。
func doRequest(t *testing.T, h http.Handler, req *http.Request, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// postRun 触发 POST /api/run，token 为空时不带 X-Admin-Token。
func postRun(t *testing.T, env *testEnv, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, nil)
	if token != "" {
		req.Header.Set("X-Admin-Token", token)
	}
	return doRequest(t, env.handler, req, "")
}

type sitesResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Sites []struct {
			Name       string `json:"name"`
			BaseURL    string `json:"base_url"`
			Enabled    bool   `json:"enabled"`
			Credential struct {
				Type        string   `json:"type"`
				CookieNames []string `json:"cookie_names"`
				HasSecret   bool     `json:"has_secret"`
				NewAPIUser  bool     `json:"new_api_user"`
			} `json:"credential"`
			Last *struct {
				CheckinDate  string `json:"checkin_date"`
				Status       string `json:"status"`
				Message      string `json:"message"`
				QuotaAwarded int64  `json:"quota_awarded"`
			} `json:"last"`
		} `json:"sites"`
	} `json:"data"`
}

type runsResponse struct {
	Success bool `json:"success"`
	Data    struct {
		Runs []struct {
			RunID     string `json:"run_id"`
			Trigger   string `json:"trigger"`
			Total     int    `json:"total"`
			Succeeded int    `json:"succeeded"`
			Failed    int    `json:"failed"`
		} `json:"runs"`
	} `json:"data"`
}

type triggerResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		RunID string `json:"run_id"`
		Site  string `json:"site"`
	} `json:"data"`
}

// TestHealthz 校验健康检查返回状态与关键字段。
func TestHealthz(t *testing.T) {
	env := newTestEnv(t, testAdminToken)
	rec := doRequest(t, env.handler, httptest.NewRequest(http.MethodGet, "/healthz", nil), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，响应 %s", rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，原始内容 %s", err, rec.Body.String())
	}
	if payload["status"] != "ok" {
		t.Errorf("status = %v，期望 ok", payload["status"])
	}
	for _, key := range []string{"sites", "schedule", "timezone"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("响应缺少字段 %q: %s", key, rec.Body.String())
		}
	}
	if got := payload["sites"]; got != float64(len(env.cfg.EnabledSites())) {
		t.Errorf("sites = %v，期望启用站点数 %d", got, len(env.cfg.EnabledSites()))
	}
	if got := payload["schedule"]; got != env.svc.schedule.String() {
		t.Errorf("schedule = %v，期望 %q", got, env.svc.schedule.String())
	}
	if got := payload["timezone"]; got != env.cfg.App.Timezone {
		t.Errorf("timezone = %v，期望 %q", got, env.cfg.App.Timezone)
	}

	// last_run 来自假 Service 提供的 checkin.Summary。
	last, ok := payload["last_run"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 last_run 字段: %s", rec.Body.String())
	}
	if got := last["succeeded"]; got != float64(env.svc.lastRun.Succeeded()) {
		t.Errorf("last_run.succeeded = %v，期望 %d", got, env.svc.lastRun.Succeeded())
	}
	if got := last["failed"]; got != float64(env.svc.lastRun.Failed()) {
		t.Errorf("last_run.failed = %v，期望 %d", got, env.svc.lastRun.Failed())
	}
}

// TestListSitesMasksCredential 校验站点列表数量，并断言完整凭据原文不会出现在响应里。
func TestListSitesMasksCredential(t *testing.T) {
	env := newTestEnv(t, testAdminToken)

	record := &model.CheckinRecord{
		SiteName:     siteAName,
		CheckinDate:  "2024-06-01",
		Status:       string(model.StatusSuccess),
		Message:      "签到成功",
		QuotaAwarded: 500,
		RunID:        "run-test-0001",
		Trigger:      "manual",
	}
	if err := model.UpsertCheckin(env.db, record); err != nil {
		t.Fatalf("写入签到记录失败: %v", err)
	}

	rec := doRequest(t, env.handler, httptest.NewRequest(http.MethodGet, "/api/sites", nil), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，响应 %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// 关键安全断言：完整 Cookie / 访问令牌原文绝不能出现在响应体里。
	if strings.Contains(body, cookieSecret) {
		t.Errorf("响应泄露了完整 Cookie 值: %s", body)
	}
	if strings.Contains(body, "session="+cookieSecret) {
		t.Errorf("响应泄露了完整 Cookie 头: %s", body)
	}
	if strings.Contains(body, accessToken) {
		t.Errorf("响应泄露了完整访问令牌: %s", body)
	}

	var resp sitesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，原始内容 %s", err, body)
	}
	if len(resp.Data.Sites) != len(env.cfg.Sites) {
		t.Fatalf("data.sites 长度 = %d，期望配置里的站点数 %d", len(resp.Data.Sites), len(env.cfg.Sites))
	}

	byName := map[string]int{}
	for i, s := range resp.Data.Sites {
		byName[s.Name] = i
	}

	cookieIdx, ok := byName[siteAName]
	if !ok {
		t.Fatalf("响应缺少站点 %s: %s", siteAName, body)
	}
	cookieView := resp.Data.Sites[cookieIdx]
	if cookieView.Credential.Type != "cookie" {
		t.Errorf("cookie 站点凭据类型 = %q，期望 cookie", cookieView.Credential.Type)
	}
	if !cookieView.Credential.HasSecret {
		t.Error("cookie 站点应当标记 has_secret=true")
	}
	if len(cookieView.Credential.CookieNames) != 1 || cookieView.Credential.CookieNames[0] != "session" {
		t.Errorf("cookie 键名 = %v，期望 [session]", cookieView.Credential.CookieNames)
	}
	// 关键：/api/sites 是未鉴权接口，连"首尾各 4 个字符"这种掩码片段都不许出现。
	runes := []rune(cookieSecret)
	oldStyleMask := string(runes[:4]) + "…" + string(runes[len(runes)-4:])
	if strings.Contains(body, oldStyleMask) {
		t.Errorf("未鉴权的 /api/sites 不应出现凭据掩码片段 %q: %s", oldStyleMask, body)
	}
	// 插库的记录应被读到，作为该站点最近一次签到结果。
	if cookieView.Last == nil {
		t.Fatalf("站点 %s 缺少 last 字段: %s", siteAName, body)
	}
	if cookieView.Last.Status != string(model.StatusSuccess) || cookieView.Last.QuotaAwarded != 500 {
		t.Errorf("站点 %s 的 last = %+v，期望 status=success quota_awarded=500", siteAName, *cookieView.Last)
	}

	tokenIdx, ok := byName[siteBName]
	if !ok {
		t.Fatalf("响应缺少站点 %s: %s", siteBName, body)
	}
	tokenView := resp.Data.Sites[tokenIdx]
	if tokenView.Credential.Type != "access_token" {
		t.Errorf("令牌站点凭据类型 = %q，期望 access_token", tokenView.Credential.Type)
	}
	if !tokenView.Credential.HasSecret {
		t.Error("令牌站点应当标记 has_secret=true")
	}
	if len(tokenView.Credential.CookieNames) != 0 {
		t.Errorf("访问令牌模式不应返回 Cookie 键名: %v", tokenView.Credential.CookieNames)
	}
	if strings.Contains(body, accessToken) {
		t.Errorf("响应泄露了完整访问令牌: %s", body)
	}
}

// TestListRunsReadsDatabase 校验批次接口读到了库里插入的 RunLog。
func TestListRunsReadsDatabase(t *testing.T) {
	env := newTestEnv(t, testAdminToken)

	run := model.RunLog{
		RunID:      "run-listed-0001",
		Trigger:    "manual",
		StartedAt:  time.Now().Add(-time.Minute),
		FinishedAt: time.Now(),
		Total:      2,
		Succeeded:  1,
		Failed:     1,
		Summary:    "trigger=manual total=2 ok=1 failed=1",
	}
	if err := env.db.Create(&run).Error; err != nil {
		t.Fatalf("写入批次记录失败: %v", err)
	}

	rec := doRequest(t, env.handler, httptest.NewRequest(http.MethodGet, "/api/runs?limit=5", nil), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，响应 %s", rec.Code, rec.Body.String())
	}

	var resp runsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，原始内容 %s", err, rec.Body.String())
	}
	if len(resp.Data.Runs) == 0 {
		t.Fatalf("没有读到任何批次记录: %s", rec.Body.String())
	}
	if len(resp.Data.Runs) > 5 {
		t.Errorf("runs 条数 = %d，超过 limit=5", len(resp.Data.Runs))
	}
	found := false
	for _, got := range resp.Data.Runs {
		if got.RunID != run.RunID {
			continue
		}
		found = true
		if got.Trigger != run.Trigger || got.Total != run.Total || got.Succeeded != run.Succeeded || got.Failed != run.Failed {
			t.Errorf("批次记录 = %+v，期望与插入值一致 %+v", got, run)
		}
	}
	if !found {
		t.Errorf("响应里没有找到刚插入的批次 %s: %s", run.RunID, rec.Body.String())
	}
}

// TestTriggerRunRequiresAdminToken 覆盖配置了 admin_token 时的鉴权分支。
func TestTriggerRunRequiresAdminToken(t *testing.T) {
	env := newTestEnv(t, testAdminToken)

	rec := postRun(t, env, "/api/run", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("不带 X-Admin-Token 时状态码 = %d，期望 401，响应 %s", rec.Code, rec.Body.String())
	}

	rec = postRun(t, env, "/api/run", "wrong-token")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("携带错误 token 时状态码 = %d，期望 401，响应 %s", rec.Code, rec.Body.String())
	}
	if env.svc.triggerCalls != 0 {
		t.Fatalf("鉴权失败时不应调用 Trigger，实际调用 %d 次", env.svc.triggerCalls)
	}

	rec = postRun(t, env, "/api/run", testAdminToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("携带正确 token 时状态码 = %d，期望 202，响应 %s", rec.Code, rec.Body.String())
	}
	var resp triggerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，原始内容 %s", err, rec.Body.String())
	}
	if resp.Data.RunID == "" {
		t.Errorf("data.run_id 为空: %s", rec.Body.String())
	}
	if env.svc.triggerCalls != 1 {
		t.Errorf("Trigger 调用次数 = %d，期望 1", env.svc.triggerCalls)
	}
}

// TestTriggerRunWithoutAdminToken 覆盖未配置 admin_token 时按来源 IP 放行/拒绝的行为。
func TestTriggerRunWithoutAdminToken(t *testing.T) {
	env := newTestEnv(t, "")

	// 本机回环来源应放行：httptest.NewRequest 默认 RemoteAddr 是 192.0.2.1，
	// 不是本机地址，因此这里显式改写成回环地址来模拟本机请求。
	req := httptest.NewRequest(http.MethodPost, "/api/run", nil)
	rec := doRequest(t, env.handler, req, "127.0.0.1:34567")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("未配置 admin_token 时本机请求状态码 = %d，期望 202，响应 %s", rec.Code, rec.Body.String())
	}
	if env.svc.triggerCalls != 1 {
		t.Errorf("Trigger 调用次数 = %d，期望 1", env.svc.triggerCalls)
	}

	// 非本机来源应拒绝（403）。
	req = httptest.NewRequest(http.MethodPost, "/api/run", nil)
	rec = doRequest(t, env.handler, req, "192.0.2.1:1234")
	if rec.Code != http.StatusForbidden {
		t.Errorf("未配置 admin_token 时非本机请求状态码 = %d，期望 403，响应 %s", rec.Code, rec.Body.String())
	}
	if env.svc.triggerCalls != 1 {
		t.Errorf("非本机请求不应调用 Trigger，实际调用 %d 次", env.svc.triggerCalls)
	}
}

// TestTriggerRunMapsSentinelErrors 校验 Trigger 的哨兵错误被映射成对应状态码。
func TestTriggerRunMapsSentinelErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "批次进行中", err: checkin.ErrRunInProgress, want: http.StatusConflict},
		{name: "站点不存在", err: checkin.ErrSiteNotFound, want: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, testAdminToken)
			env.svc.triggerErr = tc.err

			rec := postRun(t, env, "/api/run", testAdminToken)
			if rec.Code != tc.want {
				t.Errorf("状态码 = %d，期望 %d，响应 %s", rec.Code, tc.want, rec.Body.String())
			}
			if env.svc.triggerCalls != 1 {
				t.Errorf("Trigger 调用次数 = %d，期望 1", env.svc.triggerCalls)
			}
		})
	}
}

// TestTriggerRunPassesSiteQuery 校验 ?site= 原样传给 Service.Trigger。
func TestTriggerRunPassesSiteQuery(t *testing.T) {
	env := newTestEnv(t, testAdminToken)
	const site = "某站点"

	rec := postRun(t, env, "/api/run?site="+url.QueryEscape(site), testAdminToken)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202，响应 %s", rec.Code, rec.Body.String())
	}
	if env.svc.triggerCalls != 1 {
		t.Fatalf("Trigger 调用次数 = %d，期望 1", env.svc.triggerCalls)
	}
	if got := env.svc.triggerSites[0]; got != site {
		t.Errorf("Trigger 收到的站点名 = %q，期望 %q", got, site)
	}

	var resp triggerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v，原始内容 %s", err, rec.Body.String())
	}
	if resp.Data.Site != site {
		t.Errorf("data.site = %q，期望 %q", resp.Data.Site, site)
	}
}

// TestListRunsLimitParsing 校验非法或越界的 limit 不会让服务端报错。
func TestListRunsLimitParsing(t *testing.T) {
	env := newTestEnv(t, testAdminToken)

	run := model.RunLog{
		RunID:      "run-limit-0001",
		Trigger:    "manual",
		StartedAt:  time.Now().Add(-time.Minute),
		FinishedAt: time.Now(),
		Total:      1,
		Succeeded:  1,
		Summary:    "trigger=manual total=1 ok=1 failed=0",
	}
	if err := env.db.Create(&run).Error; err != nil {
		t.Fatalf("写入批次记录失败: %v", err)
	}

	cases := []struct {
		name  string
		query string
	}{
		{"非数字 limit", "limit=abc"},
		{"超过上限 limit", "limit=999"},
		{"零 limit", "limit=0"},
		{"负数 limit", "limit=-3"},
		{"空 limit", "limit="},
		{"未带 limit", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target := "/api/runs"
			if tc.query != "" {
				target += "?" + tc.query
			}
			rec := doRequest(t, env.handler, httptest.NewRequest(http.MethodGet, target, nil), "")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s 状态码 = %d，期望 200，响应 %s", target, rec.Code, rec.Body.String())
			}
			var resp runsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("响应不是合法 JSON: %v，原始内容 %s", err, rec.Body.String())
			}
			if !resp.Success {
				t.Errorf("GET %s success = false，响应 %s", target, rec.Body.String())
			}
			if len(resp.Data.Runs) > 200 {
				t.Errorf("GET %s runs 条数 = %d，不应超过硬上限 200", target, len(resp.Data.Runs))
			}
		})
	}
}

// TestNilDBNotPanics 校验未接入数据库时站点与批次接口仍能正常返回，且凭据依旧掩码。
func TestNilDBNotPanics(t *testing.T) {
	cfg := &config.Config{
		App: config.App{Timezone: "Asia/Shanghai"},
		Sites: []config.Site{{
			Name:    siteAName,
			BaseURL: "https://a.example.com",
			Enabled: true,
			Credential: config.Credential{
				Type:   config.CredentialCookie,
				Cookie: "session=" + cookieSecret,
			},
		}},
	}
	srv := New(Options{
		AdminToken: testAdminToken,
		Config:     cfg,
		Service:    &fakeService{schedule: scheduler.New(8, 0, 0, time.UTC)},
		Version:    "test",
		StartedAt:  time.Now(),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	h := srv.Handler()

	for _, target := range []string{"/api/sites", "/api/runs?limit=abc", "/api/runs?limit=999"} {
		rec := doRequest(t, h, httptest.NewRequest(http.MethodGet, target, nil), "")
		if rec.Code != http.StatusOK {
			t.Errorf("DB 为 nil 时 GET %s 状态码 = %d，期望 200，响应 %s", target, rec.Code, rec.Body.String())
		}
	}

	rec := doRequest(t, h, httptest.NewRequest(http.MethodGet, "/api/sites", nil), "")
	if strings.Contains(rec.Body.String(), cookieSecret) {
		t.Errorf("DB 为 nil 时响应泄露了完整 Cookie 值: %s", rec.Body.String())
	}
}
