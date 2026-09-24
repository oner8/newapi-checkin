package checkin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"newapi-checkin/internal/config"
	"newapi-checkin/internal/db"
	"newapi-checkin/internal/model"
	"newapi-checkin/internal/notify"
	"newapi-checkin/internal/site"
)

// fakeSite 是一个可控的假 new-api 站点，用来驱动 runner 的完整流程。
type fakeSite struct {
	mu sync.Mutex

	checkinEnabled bool
	turnstile      bool
	checkedToday   bool
	quota          int64
	award          int64

	// checked/quotas 按账号（session Cookie 的值）分别记录状态，用来模拟同一站点上的多个用户。
	// 键为空表示请求没带 session Cookie，此时沿用上面的 checkedToday/quota 字段。
	checked map[string]bool
	quotas  map[string]int64

	selfFailures int
	authFail     bool
	postMessage  string
	statusBody   string
	// checkinDate 覆盖 POST 返回的 checkin_date；为空时用当天，避免测试写死日期后过期。
	checkinDate string
	// delay 让每次请求慢下来，用于验证 latency_ms 真的记录了耗时。
	delay time.Duration

	// checkinPath 覆盖签到接口路径（默认 /api/user/checkin），用于测试 fork 改了路径的站点。
	checkinPath string
	// forkStyle 模拟「签到接口被改成自己的路径」的 fork：只提供 POST 签到接口（没有状态查询），
	// 响应形如 {"success":true,"message":"签到成功，获得 $25 额度"}，没有 data/额度字段。
	forkStyle bool
	// statusQueries 记录状态预查（GET 签到接口）被请求了几次。
	statusQueries int

	selfCalls int
	postCalls int
}

func (f *fakeSite) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		if f.statusBody != "" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.statusBody))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
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
		if f.selfFailures > 0 {
			f.selfFailures--
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if f.authFail {
			json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "未登录，请先登录"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"id": 7, "username": "tester", "quota": f.quotaOf(accountKey(r)), "used_quota": 0},
		})
	})

	checkinPath := strings.TrimSpace(f.checkinPath)
	if checkinPath == "" {
		checkinPath = "/api/user/checkin"
	}
	mux.HandleFunc(checkinPath, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodPost {
			f.statusQueries++
		}
		if f.forkStyle && r.Method != http.MethodPost {
			// 该 fork 没有签到状态查询接口。
			http.NotFound(w, r)
			return
		}
		if f.authFail {
			json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "未登录，请先登录"})
			return
		}
		if r.Method == http.MethodPost {
			f.postCalls++
			if f.postMessage != "" {
				json.NewEncoder(w).Encode(map[string]any{"success": false, "message": f.postMessage})
				return
			}
			key := accountKey(r)
			f.setChecked(key, f.quotaOf(key)+f.award)
			if f.forkStyle {
				json.NewEncoder(w).Encode(map[string]any{
					"success": true,
					"message": "签到成功，获得 $25 额度",
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"message": "签到成功",
				"data":    map[string]any{"quota_awarded": f.award, "checkin_date": f.responseCheckinDate()},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data": map[string]any{
				"stats": map[string]any{
					"checked_in_today": f.isChecked(accountKey(r)),
					"total_quota":      f.quotaOf(accountKey(r)),
					"total_checkins":   1,
					"records":          []any{},
				},
			},
		})
	})

	return mux
}

// accountKey 用凭据区分账号：同站点多账号的测试里，每个账号用不同的 session Cookie 值。
// 没带 session Cookie 时返回空串，表示"默认账号"。
func accountKey(r *http.Request) string {
	if c, err := r.Cookie("session"); err == nil {
		return c.Value
	}
	return ""
}

// isChecked 判断某个账号今天是否已签到。checkedToday 仍是全局开关（既有测试用它模拟
// 「今天已签到」），per-account 状态放在 checked 里。
func (f *fakeSite) isChecked(key string) bool {
	return f.checkedToday || (key != "" && f.checked[key])
}

// quotaOf 返回某个账号当前余额；未单独记录时回落到 quota 字段（既有测试的默认值）。
func (f *fakeSite) quotaOf(key string) int64 {
	if key == "" {
		return f.quota
	}
	if v, ok := f.quotas[key]; ok {
		return v
	}
	return f.quota
}

// setChecked 记录某个账号签到后的状态与余额。
func (f *fakeSite) setChecked(key string, newQuota int64) {
	if key == "" {
		f.checkedToday = true
		f.quota = newQuota
		return
	}
	if f.checked == nil {
		f.checked = map[string]bool{}
		f.quotas = map[string]int64{}
	}
	f.checked[key] = true
	f.quotas[key] = newQuota
}

// recordingNotifier 记录被推送的通知，代替真实的 Bark 请求。
type recordingNotifier struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (n *recordingNotifier) Name() string  { return "recording" }
func (n *recordingNotifier) Enabled() bool { return true }

func (n *recordingNotifier) Send(_ context.Context, msg notify.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.msgs = append(n.msgs, msg)
	return nil
}

func (n *recordingNotifier) messages() []notify.Message {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]notify.Message, len(n.msgs))
	copy(out, n.msgs)
	return out
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newFakeSiteServer(t *testing.T, f *fakeSite) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return srv
}

func testConfig(t *testing.T, baseURL string) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.App.Timezone = "Asia/Shanghai"
	cfg.App.Schedule = "08:00"
	cfg.App.JitterSeconds = 0
	cfg.App.Concurrency = 2
	cfg.App.TimeoutSeconds = 3
	cfg.App.Retries = 2
	cfg.App.RetryBackoffSeconds = 1
	cfg.Notify.Bark.Mode = config.BarkModeSummary
	cfg.Sites = []config.Site{{
		Name:       "站点A",
		BaseURL:    baseURL,
		Enabled:    true,
		Credential: config.Credential{Type: config.CredentialCookie, Cookie: "session=test"},
	}}
	return cfg
}

func newTestRunner(t *testing.T, cfg *config.Config, rec *recordingNotifier) (*Runner, *gorm.DB) {
	t.Helper()
	gdb, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(gdb) })

	notifier := notify.NewMulti(testLogger())
	if rec != nil {
		notifier = notify.NewMulti(testLogger(), rec)
	}
	runner, err := NewRunner(gdb, cfg, notifier, testLogger())
	if err != nil {
		t.Fatalf("构造 Runner 失败: %v", err)
	}
	// 测试里把重试间隔压到 1ms，避免真等 1 秒以上。
	runner.retryBackoff = time.Millisecond
	return runner, gdb
}

func todayIn(t *testing.T, cfg *config.Config) string {
	t.Helper()
	loc, err := cfg.App.Location()
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	return time.Now().In(loc).Format("2006-01-02")
}

func TestRunSuccessFlow(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, award: 10000, quota: 500000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	rec := &recordingNotifier{}
	runner, gdb := newTestRunner(t, cfg, rec)

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if summary.Total() != 1 || summary.Failed() != 0 {
		t.Fatalf("批次统计不对: total=%d failed=%d", summary.Total(), summary.Failed())
	}

	res := summary.Results[0]
	if res.Status != model.StatusSuccess {
		t.Fatalf("状态应为 success，实际 %s（%s）", res.Status, res.Message)
	}
	if res.QuotaAwarded != 10000 {
		t.Errorf("获得额度不对: %d", res.QuotaAwarded)
	}
	if res.QuotaBefore == nil || *res.QuotaBefore != 500000 {
		t.Errorf("签到前余额不对: %v", res.QuotaBefore)
	}
	if res.QuotaAfter == nil || *res.QuotaAfter != 510000 {
		t.Errorf("签到后余额不对: %v", res.QuotaAfter)
	}
	if res.Username != "tester" || res.SiteVersion != "v1.0.0-rc.40" {
		t.Errorf("站点信息未记录: %+v", res)
	}
	if fake.postCalls != 1 {
		t.Errorf("应当只提交一次签到，实际 %d 次", fake.postCalls)
	}

	// 落库检查。
	var record model.CheckinRecord
	if err := gdb.First(&record).Error; err != nil {
		t.Fatalf("未写入签到记录: %v", err)
	}
	if record.Status != string(model.StatusSuccess) || record.QuotaAwarded != 10000 {
		t.Errorf("签到记录内容不对: %+v", record)
	}
	if record.CheckinDate != todayIn(t, cfg) || record.RunID != summary.RunID {
		t.Errorf("签到日期/批次号不对: %+v", record)
	}
	if !strings.Contains(res.Response, "quota_awarded") {
		t.Errorf("结果里应保留站点原始响应，实际 %q", res.Response)
	}
	if record.QuotaPerUnit != 500000 {
		t.Errorf("额度单位未记录: %v", record.QuotaPerUnit)
	}

	var runLog model.RunLog
	if err := gdb.First(&runLog).Error; err != nil {
		t.Fatalf("未写入批次记录: %v", err)
	}
	if runLog.Total != 1 || runLog.Failed != 0 || runLog.Trigger != TriggerManual {
		t.Errorf("批次记录内容不对: %+v", runLog)
	}

	msgs := rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("应当推送 1 条通知，实际 %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Title, "签到完成 1/1") {
		t.Errorf("通知标题不对: %q", msgs[0].Title)
	}
	if !strings.Contains(msgs[0].Body, "站点A") || !strings.Contains(msgs[0].Body, "✅") {
		t.Errorf("通知正文缺少站点信息: %q", msgs[0].Body)
	}
}

func TestRunAlreadyCheckedIn(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, checkedToday: true, quota: 500000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusAlready {
		t.Fatalf("状态应为 already，实际 %s", res.Status)
	}
	if fake.postCalls != 0 {
		t.Errorf("已签到不应再提交，实际提交 %d 次", fake.postCalls)
	}
}

func TestRunCheckinDisabled(t *testing.T) {
	fake := &fakeSite{checkinEnabled: false}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusSkippedDisabled {
		t.Fatalf("状态应为 skipped_disabled，实际 %s", res.Status)
	}
	if fake.selfCalls != 0 {
		t.Error("站点未开启签到时不应请求需要登录的接口")
	}
	if summary.Failed() != 0 {
		t.Error("未开启签到不应算作失败")
	}
}

func TestRunAuthFailureIsNotRetried(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, authFail: true}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, gdb := newTestRunner(t, cfg, &recordingNotifier{})

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusAuthFailed {
		t.Fatalf("状态应为 auth_failed，实际 %s（%s）", res.Status, res.Message)
	}
	if fake.selfCalls != 1 {
		t.Errorf("凭据失效不应重试，实际请求 %d 次", fake.selfCalls)
	}
	if fake.postCalls != 0 {
		t.Error("凭据失效不应提交签到")
	}

	var record model.CheckinRecord
	if err := gdb.First(&record).Error; err != nil {
		t.Fatalf("未写入记录: %v", err)
	}
	if record.Status != string(model.StatusAuthFailed) {
		t.Errorf("记录状态不对: %s", record.Status)
	}
}

func TestRunRetriesServerErrors(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, selfFailures: 2, award: 500, quota: 1000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusSuccess {
		t.Fatalf("两次 500 后应当重试成功，实际 %s（%s）", res.Status, res.Message)
	}
	// 前两次 500 + 成功一次 + 签到后再查一次余额。
	if fake.selfCalls != 4 {
		t.Errorf("重试次数不对: selfCalls=%d", fake.selfCalls)
	}
	if res.Attempts < 3 {
		t.Errorf("尝试次数应当包含重试: %d", res.Attempts)
	}
}

func TestRunTurnstileRequired(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, turnstile: true, postMessage: "Turnstile token 为空"}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusNeedTurnstile {
		t.Fatalf("状态应为 need_turnstile，实际 %s", res.Status)
	}
	if !res.TurnstileCheck {
		t.Error("应当记录站点开启了 Turnstile")
	}
	if fake.postCalls != 1 {
		t.Errorf("不应重试验证码失败，实际提交 %d 次", fake.postCalls)
	}
}

func TestRunNonNewAPISite(t *testing.T) {
	// 注意样本：既没有 checkin_enabled，也没有 version/system_name —— 这才是「不像 new-api」。
	// 若只缺 checkin_enabled 却带 version（fork 常见），现在会按「未声明」继续尝试，见
	// TestRunForkWithoutCheckinFieldInStatus。
	fake := &fakeSite{statusBody: `{"success":true,"data":{"foo":"bar"}}`}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusNotNewAPI {
		t.Fatalf("状态应为 not_newapi，实际 %s", res.Status)
	}
	if fake.selfCalls != 0 {
		t.Error("识别为非 new-api 后不应继续请求")
	}
}

func TestRunIsIdempotentAcrossRuns(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, award: 2000, quota: 1000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	rec := &recordingNotifier{}
	runner, gdb := newTestRunner(t, cfg, rec)

	first, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("第一次 Run 失败: %v", err)
	}
	if first.Results[0].Status != model.StatusSuccess {
		t.Fatalf("第一次应为 success，实际 %s", first.Results[0].Status)
	}

	second, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("第二次 Run 失败: %v", err)
	}
	if second.Results[0].Status != model.StatusAlready {
		t.Fatalf("第二次应为 already，实际 %s", second.Results[0].Status)
	}
	if fake.postCalls != 1 {
		t.Errorf("同一天不应重复提交签到，实际 %d 次", fake.postCalls)
	}

	var count int64
	if err := gdb.Model(&model.CheckinRecord{}).Count(&count).Error; err != nil {
		t.Fatalf("统计签到记录失败: %v", err)
	}
	if count != 1 {
		t.Errorf("同一天同一站点应当只有 1 行记录，实际 %d 行", count)
	}

	// 全绿的通知每天只推一条（第二次被去重键拦住）。
	if msgs := rec.messages(); len(msgs) != 1 {
		t.Errorf("成功汇总每天应只推 1 条，实际 %d 条", len(msgs))
	}
}

func TestNotifyDedupForRepeatedFailures(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, authFail: true}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	cfg.Notify.Bark.Mode = config.BarkModeFailure
	rec := &recordingNotifier{}
	runner, _ := newTestRunner(t, cfg, rec)

	for i := 0; i < 2; i++ {
		if _, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual}); err != nil {
			t.Fatalf("第 %d 次 Run 失败: %v", i+1, err)
		}
	}

	msgs := rec.messages()
	if len(msgs) != 1 {
		t.Fatalf("同一失败当天只应推 1 条，实际 %d 条", len(msgs))
	}
	if !strings.Contains(msgs[0].Title, "签到异常") {
		t.Errorf("失败通知标题不对: %q", msgs[0].Title)
	}
	if msgs[0].Level != failureLevel {
		t.Errorf("失败通知应升级级别，实际 %q", msgs[0].Level)
	}
	if !strings.Contains(msgs[0].Body, "未登录") {
		t.Errorf("通知正文应当带上站点原文: %q", msgs[0].Body)
	}
}

func TestDryRunDoesNotSubmit(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, award: 2000, quota: 1000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	cfg.App.DryRun = true
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if summary.Results[0].Status != model.StatusDryRun {
		t.Fatalf("状态应为 dry_run，实际 %s", summary.Results[0].Status)
	}
	if fake.postCalls != 0 {
		t.Errorf("dry-run 不应提交签到，实际 %d 次", fake.postCalls)
	}
	if summary.Failed() != 0 {
		t.Error("dry-run 不应算作失败")
	}
}

func TestSelectSitesFilter(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	cfg.Sites = append(cfg.Sites, config.Site{
		Name:       "站点B",
		BaseURL:    srv.URL,
		Enabled:    true,
		Credential: config.Credential{Type: config.CredentialCookie, Cookie: "session=b"},
	})
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual, Site: "站点B"})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if summary.Total() != 1 || summary.Results[0].SiteName != "站点B" {
		t.Errorf("站点过滤失败: %+v", summary.Results)
	}

	_, err = runner.Run(context.Background(), RunOptions{Trigger: TriggerManual, Site: "不存在的站点"})
	if !errors.Is(err, ErrSiteNotFound) {
		t.Errorf("未找到站点时应返回 ErrSiteNotFound，实际 %v", err)
	}
}

func TestConcurrentRunsAreRejected(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	if !runner.Begin() {
		t.Fatal("第一次 Begin 应当成功")
	}
	if _, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual}); !errors.Is(err, ErrRunInProgress) {
		t.Errorf("已有批次执行时应返回 ErrRunInProgress，实际 %v", err)
	}
	runner.Finish()
	if runner.Running() {
		t.Error("Finish 后不应仍处于运行中")
	}
}

func TestRenderMessageIncludesHintsAndSuppression(t *testing.T) {
	before := int64(1000)
	after := int64(3000)
	sum := &Summary{
		RunID:   "manual-test",
		Trigger: TriggerManual,
		Results: []SiteResult{
			{SiteName: "站点A", Status: model.StatusSuccess, QuotaAwarded: 2000, QuotaBefore: &before, QuotaAfter: &after, QuotaPerUnit: 500000},
			{SiteName: "站点B", Status: model.StatusAlready, QuotaAfter: &before, QuotaPerUnit: 500000},
			{SiteName: "站点C", Status: model.StatusAuthFailed, Message: "未登录，请先登录"},
			{SiteName: "站点D", Status: model.StatusError, Message: "请求超时（超过 20s）"},
		},
	}

	// 只有一个失败是"新"的（站点C），站点D 已在今天提醒过。
	msg := RenderMessage(sum, []SiteResult{sum.Results[2]}, "active")
	if !strings.Contains(msg.Title, "签到异常 2/4") {
		t.Errorf("标题不对: %q", msg.Title)
	}
	if msg.Level != failureLevel {
		t.Errorf("有失败时应升级级别: %q", msg.Level)
	}
	if !strings.Contains(msg.Body, "❌ 站点C") {
		t.Errorf("正文缺少失败站点: %q", msg.Body)
	}
	if strings.Contains(msg.Body, "❌ 站点D") {
		t.Errorf("已提醒过的失败不应重复出现: %q", msg.Body)
	}
	if !strings.Contains(msg.Body, "另有 1 项失败") {
		t.Errorf("应当提示被抑制的失败数量: %q", msg.Body)
	}
	if !strings.Contains(msg.Body, "重新登录面板") {
		t.Errorf("应当给出凭据失效的处理建议: %q", msg.Body)
	}
	if !strings.Contains(msg.Body, "✅ 站点A") || !strings.Contains(msg.Body, "☑️ 站点B") {
		t.Errorf("正文缺少成功/已签到站点: %q", msg.Body)
	}

	// 全绿且没有新失败时，标题应当是"完成"。
	clean := RenderMessage(&Summary{Results: sum.Results[:2]}, nil, "active")
	if !strings.Contains(clean.Title, "签到完成 2/2") || clean.Level != "active" {
		t.Errorf("全绿通知不对: %q level=%q", clean.Title, clean.Level)
	}
}

func TestFormatMoney(t *testing.T) {
	cases := []struct {
		quota    int64
		perUnit  float64
		expected string
	}{
		{500000, 500000, "$1.00"},
		{10000, 500000, "$0.0200"},
		{5000, 500000, "$0.0100"},
		{1000, 500000, "$0.002000"},
		{10000, 0, "$0.0200"}, // 单位未知时按默认 500000 计算
	}
	for _, tc := range cases {
		if got := formatMoney(tc.quota, tc.perUnit); got != tc.expected {
			t.Errorf("formatMoney(%d, %v) = %q，期望 %q", tc.quota, tc.perUnit, got, tc.expected)
		}
	}
}

func TestStatusFromErrorMapping(t *testing.T) {
	if got := statusFromError(nil); got != model.StatusError {
		t.Errorf("nil 错误应映射为 error，实际 %s", got)
	}
	if got := statusFromError(site.NewError(site.KindBlocked, "GET /api/status", "被拦截", nil)); got != model.StatusBlocked {
		t.Errorf("被 WAF 拦截应映射为 blocked，实际 %s", got)
	}
	if got := statusFromError(site.NewError(site.KindAuthFailed, "GET /api/user/self", "401", nil)); got != model.StatusAuthFailed {
		t.Errorf("凭据失效应映射为 auth_failed，实际 %s", got)
	}
}

// TestRerunUpdatesSameDayRow 验证「同一天重复运行会更新同一行」的落库语义。
//
// 这条语义很关键：凭据失效后修好再重跑、或手动补跑，都不应该在 checkin_records
// 里堆出多行，否则按天统计会重复计数。仅断言"第二次运行状态是 already"是不够的，
// 必须验证那一行确实被覆盖更新，因此这里让第一次失败、第二次成功。
func TestRerunUpdatesSameDayRow(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, authFail: true}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, gdb := newTestRunner(t, cfg, nil)

	first, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("第一次 Run 失败: %v", err)
	}
	if got := first.Results[0].Status; got != model.StatusAuthFailed {
		t.Fatalf("第一次运行状态应为 auth_failed，实际 %s", got)
	}

	// 修好凭据，并给站点一个可观测的额度，然后重跑同一天。
	fake.mu.Lock()
	fake.authFail = false
	fake.quota = 1000
	fake.award = 20000
	fake.mu.Unlock()

	second, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("第二次 Run 失败: %v", err)
	}
	if got := second.Results[0].Status; got != model.StatusSuccess {
		t.Fatalf("第二次运行状态应为 success，实际 %s（%s）", got, second.Results[0].Message)
	}

	var count int64
	if err := gdb.Model(&model.CheckinRecord{}).Count(&count).Error; err != nil {
		t.Fatalf("统计签到记录失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("同一天重复运行应当只保留 1 行记录，实际 %d 行", count)
	}

	var record model.CheckinRecord
	if err := gdb.First(&record).Error; err != nil {
		t.Fatalf("读取签到记录失败: %v", err)
	}
	if record.Status != string(model.StatusSuccess) {
		t.Errorf("那一行应被更新为 success，实际 %s", record.Status)
	}
	if record.QuotaAwarded != 20000 {
		t.Errorf("那一行的额度应被更新为 20000，实际 %d", record.QuotaAwarded)
	}
	if record.RunID != second.RunID {
		t.Errorf("那一行的批次号应更新为最新批次 %s，实际 %s", second.RunID, record.RunID)
	}
	if record.Trigger != TriggerManual {
		t.Errorf("那一行的触发来源应为 manual，实际 %s", record.Trigger)
	}
}

// responseCheckinDate 返回假站点应回答的签到日期。
func (f *fakeSite) responseCheckinDate() string {
	if f.checkinDate != "" {
		return f.checkinDate
	}
	return time.Now().Format("2006-01-02")
}

// TestSuccessUsesSiteCheckinDate 验证落库日期以站点返回的 checkin_date 为准。
//
// 站点按自己的时区判定自然日：当 app.timezone 与站点时区不一致时，只有用站点日期
// 才不会把同一次签到记成两天。
func TestSuccessUsesSiteCheckinDate(t *testing.T) {
	const siteDate = "2026-01-02"
	fake := &fakeSite{checkinEnabled: true, award: 1000, checkinDate: siteDate}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, gdb := newTestRunner(t, cfg, nil)

	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if got := summary.Results[0].Status; got != model.StatusSuccess {
		t.Fatalf("状态应为 success，实际 %s（%s）", got, summary.Results[0].Message)
	}

	var record model.CheckinRecord
	if err := gdb.First(&record).Error; err != nil {
		t.Fatalf("读取签到记录失败: %v", err)
	}
	if record.CheckinDate != siteDate {
		t.Errorf("落库日期 = %q，期望站点返回的 %q", record.CheckinDate, siteDate)
	}
}

// TestRerunAfterSuccessKeepsAwardAndStatus 验证「一行 = 当天最佳结果」。
//
// 早上真签到成功后，晚上再跑一次（例如手动触发）时站点会回答「今日已签到」，此时：
//   - 状态不能被降级成 already —— 当天确实签到成功过；
//   - quota_awarded 不能被抹成 0 —— 否则晚上的重跑会让当天已经拿到的奖励凭空消失。
func TestRerunAfterSuccessKeepsAwardAndStatus(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, award: 30000, quota: 100000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, gdb := newTestRunner(t, cfg, nil)

	first, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("第一次 Run 失败: %v", err)
	}
	if got := first.Results[0].Status; got != model.StatusSuccess {
		t.Fatalf("第一次运行状态应为 success，实际 %s（%s）", got, first.Results[0].Message)
	}
	if got := first.Results[0].QuotaAwarded; got != 30000 {
		t.Fatalf("第一次运行应获得 30000，实际 %d", got)
	}

	second, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("第二次 Run 失败: %v", err)
	}
	if got := second.Results[0].Status; got != model.StatusAlready {
		t.Fatalf("第二次运行状态应为 already，实际 %s", got)
	}

	var count int64
	if err := gdb.Model(&model.CheckinRecord{}).Count(&count).Error; err != nil {
		t.Fatalf("统计签到记录失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("同一天应当只有 1 行记录，实际 %d 行", count)
	}

	var record model.CheckinRecord
	if err := gdb.First(&record).Error; err != nil {
		t.Fatalf("读取签到记录失败: %v", err)
	}
	if record.Status != string(model.StatusSuccess) {
		t.Errorf("当天已成功时状态不应被降级，实际 %s", record.Status)
	}
	if record.QuotaAwarded != 30000 {
		t.Errorf("当天已获得的额度不应被抹掉，实际 %d", record.QuotaAwarded)
	}
}

// TestCanceledBatchIsNotPersisted 验证被取消的批次不留半成品记录。
//
// 这类记录会让 StartupCatchUp 误判"今天已经跑过"，导致重启后当天不再补签。
func TestCanceledBatchIsNotPersisted(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, award: 1000, quota: 1000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, gdb := newTestRunner(t, cfg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟批次执行中收到 SIGTERM

	_, err := runner.Run(ctx, RunOptions{Trigger: TriggerManual})
	if !errors.Is(err, ErrRunCanceled) {
		t.Fatalf("期望返回 ErrRunCanceled，实际 %v", err)
	}

	var records, runs int64
	if err := gdb.Model(&model.CheckinRecord{}).Count(&records).Error; err != nil {
		t.Fatalf("统计签到记录失败: %v", err)
	}
	if err := gdb.Model(&model.RunLog{}).Count(&runs).Error; err != nil {
		t.Fatalf("统计批次记录失败: %v", err)
	}
	if records != 0 || runs != 0 {
		t.Errorf("被取消的批次不应落库：checkin_records=%d run_logs=%d", records, runs)
	}
}

// TestDryRunDoesNotOverwriteConfirmedResult 验证"当天已有确认结果时不写入本次结论"。
//
// 场景：早上签到成功，之后用户按 README 用 --dry-run 验证凭据；dry-run 表示"没真正签到"，
// 不能把当天真实成功的记录抹成 dry_run（否则当天额度统计归零、状态也难以解释）。
func TestDryRunDoesNotOverwriteConfirmedResult(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, award: 2000, quota: 1000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, gdb := newTestRunner(t, cfg, &recordingNotifier{})

	first, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("第一次 Run 失败: %v", err)
	}
	if first.Results[0].Status != model.StatusSuccess {
		t.Fatalf("第一次应为 success，实际 %s", first.Results[0].Status)
	}

	cfg.App.DryRun = true
	second, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("dry-run Run 失败: %v", err)
	}
	if second.Results[0].Status != model.StatusDryRun {
		t.Fatalf("第二次应为 dry_run，实际 %s", second.Results[0].Status)
	}

	var record model.CheckinRecord
	if err := gdb.First(&record).Error; err != nil {
		t.Fatalf("读取签到记录失败: %v", err)
	}
	if record.Status != string(model.StatusSuccess) {
		t.Errorf("dry-run 不该覆盖当天已确认的成功记录，实际状态 %s", record.Status)
	}
	if record.QuotaAwarded != 2000 {
		t.Errorf("当天额度不该被 dry-run 清零，实际 %d", record.QuotaAwarded)
	}
}

// TestSummaryNotificationOnlyOncePerDay 验证"全绿成功汇总"每天只推一次，
// 且局部运行（?site=X / --site）不会推出误导性的"签到完成"。
func TestSummaryNotificationOnlyOncePerDay(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, award: 1000, quota: 100}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	cfg.Sites = append(cfg.Sites, config.Site{
		Name:       "站点B",
		BaseURL:    srv.URL,
		Enabled:    true,
		Credential: config.Credential{Type: config.CredentialCookie, Cookie: "session=b"},
	})
	rec := &recordingNotifier{}
	runner, _ := newTestRunner(t, cfg, rec)

	if _, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual}); err != nil {
		t.Fatalf("全量 Run 失败: %v", err)
	}
	if got := len(rec.messages()); got != 1 {
		t.Fatalf("全量运行后应推 1 条汇总，实际 %d 条", got)
	}

	if _, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual, Site: "站点A"}); err != nil {
		t.Fatalf("局部 Run 失败: %v", err)
	}
	if got := len(rec.messages()); got != 1 {
		t.Errorf("局部运行不该重复推送成功汇总，实际 %d 条", got)
	}
}

// TestRunScheduledWaitsForBusyLock 验证定时批次遇到"已有批次在跑"时会等待，
// 而不是被静默丢掉（那是漏签的主要来源）。
func TestRunScheduledWaitsForBusyLock(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	svc := newTestService(cfg, runner)
	if !runner.Begin() {
		t.Fatal("Begin 应当成功")
	}
	go func() {
		time.Sleep(40 * time.Millisecond)
		runner.Finish()
	}()

	summary, err := svc.RunScheduled(context.Background())
	if err != nil {
		t.Fatalf("RunScheduled 应当在执行权释放后成功，实际错误: %v", err)
	}
	if summary == nil || summary.Total() != 1 {
		t.Fatalf("批次结果不对: %+v", summary)
	}
}

// TestRunScheduledGivesUpAfterBudget 验证等待有上限，超时后返回明确的错误。
func TestRunScheduledGivesUpAfterBudget(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	runner, _ := newTestRunner(t, cfg, &recordingNotifier{})

	svc := newTestService(cfg, runner)
	svc.busyWaitBudget = 30 * time.Millisecond
	if !runner.Begin() {
		t.Fatal("Begin 应当成功")
	}
	defer runner.Finish()

	if _, err := svc.RunScheduled(context.Background()); !errors.Is(err, ErrRunInProgress) {
		t.Errorf("等待超时后应返回 ErrRunInProgress，实际 %v", err)
	}
}

// newTestService 用现成的 runner 组装 Service，并把等待策略调到测试友好的量级。
func newTestService(cfg *config.Config, runner *Runner) *Service {
	return &Service{
		cfg:               cfg,
		runner:            runner,
		loc:               runner.Location(),
		schedule:          runner.Schedule(),
		log:               testLogger(),
		busyRetryInterval: 5 * time.Millisecond,
		busyWaitBudget:    2 * time.Second,
	}
}

// TestAttemptRecordsLatency 回归测试：latency_ms 必须反映真实耗时。
//
// 曾经的写法是在 attempt（匿名返回值、多个 return 分支）里用 defer 给局部变量赋值，
// 返回值在 defer 之前就已拷贝，导致 latency_ms 恒为 0 —— 日志里看像"瞬间成功"，
// 实际却掩盖了慢站点。现在耗时在包装层 attempt 里赋值，任何 return 分支都能被统计到。
func TestAttemptRecordsLatency(t *testing.T) {
	f := &fakeSite{checkinEnabled: true, award: 1000, delay: 30 * time.Millisecond}
	srv := newFakeSiteServer(t, f)
	cfg := testConfig(t, srv.URL)
	runner, _ := newTestRunner(t, cfg, nil)

	res := runner.attempt(context.Background(), cfg.Sites[0])
	if res.Status != model.StatusSuccess {
		t.Fatalf("期望签到成功，实际 %v（%v）", res.Status, res.Message)
	}
	if res.LatencyMS < 20 {
		t.Fatalf("latency_ms 应记录真实耗时，实际 %d（不应再出现恒为 0）", res.LatencyMS)
	}
}

// TestRunMultipleAccountsOnSameSite 验证「同一站点多个账号」的配置方式：
// sites[] 放多条、base_url 相同、name 不同、凭据各自独立，各账号互不干扰。
//
// 之所以成立：签到记录的唯一索引是 (site_name, checkin_date)，站点身份用 name 而不是
// base_url，因此同一 URL 上的两个账号各占一行；配置校验又要求 name 唯一，避免两个账号
// 共用同一个键而互相覆盖。
func TestRunMultipleAccountsOnSameSite(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, award: 10000, quota: 500000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)

	// 第二个账号：同一个 base_url，只改 name 与凭据。
	second := cfg.Sites[0]
	second.Name = "站点A-账号2"
	second.Credential = config.Credential{Type: config.CredentialCookie, Cookie: "session=second-account"}
	cfg.Sites = append(cfg.Sites, second)

	runner, gdb := newTestRunner(t, cfg, nil)
	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if summary.Total() != 2 || summary.Failed() != 0 {
		t.Fatalf("两个账号都应成功: total=%d failed=%d", summary.Total(), summary.Failed())
	}
	if fake.postCalls != 2 {
		t.Errorf("每个账号各提交一次签到，实际提交 %d 次", fake.postCalls)
	}

	var records []model.CheckinRecord
	if err := gdb.Where("checkin_date = ?", todayIn(t, cfg)).Order("site_name").Find(&records).Error; err != nil {
		t.Fatalf("查询签到记录失败: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("两个账号应各占一行，实际 %d 行（name 重复或键设计不当会吞掉一行）", len(records))
	}
	if records[0].SiteName == records[1].SiteName {
		t.Errorf("两行的站点名不应相同: %q / %q", records[0].SiteName, records[1].SiteName)
	}
	for _, r := range records {
		if r.Status != string(model.StatusSuccess) {
			t.Errorf("账号 %s 状态应为 success，实际 %s", r.SiteName, r.Status)
		}
		if r.QuotaAwarded != 10000 {
			t.Errorf("账号 %s 额度应为 10000，实际 %d", r.SiteName, r.QuotaAwarded)
		}
	}
}

// TestRunForkWithCustomCheckinPath 覆盖「fork 改了签到接口路径 + 没有状态查询接口」。
//
// 真实例子：某 fork 的签到是 POST /api/user/sign_in，响应 {"success":true,"message":"签到成功，获得 $25 额度"}
// （没有 data、没有 quota_awarded），并且查不到签到状态。改前这类站点会被 404 判成失败，永远签不上。
func TestRunForkWithCustomCheckinPath(t *testing.T) {
	fake := &fakeSite{
		checkinEnabled: true,
		checkinPath:    "/api/user/sign_in",
		forkStyle:      true,
		award:          250000,
		quota:          1000000,
	}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	cfg.Sites[0].CheckinPath = "/api/user/sign_in"
	cfg.Sites[0].CheckinStatus = config.CheckinStatusOff

	runner, gdb := newTestRunner(t, cfg, nil)
	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusSuccess {
		t.Fatalf("自定义签到路径应签到成功，实际 %s（%s）", res.Status, res.Message)
	}
	// 响应里没有 quota_awarded，额度应当由"签到前后余额差"补齐。
	if res.QuotaAwarded != 250000 {
		t.Errorf("额度应按余额差补齐，实际 %d（消息: %s）", res.QuotaAwarded, res.Message)
	}
	if fake.postCalls != 1 {
		t.Errorf("应只提交一次签到，实际 %d 次", fake.postCalls)
	}
	if fake.statusQueries != 0 {
		t.Errorf("checkin_status: off 时不应发起状态预查，实际 %d 次", fake.statusQueries)
	}
	var rec model.CheckinRecord
	if err := gdb.First(&rec).Error; err != nil {
		t.Fatalf("未写入签到记录: %v", err)
	}
	if rec.Status != string(model.StatusSuccess) || rec.QuotaAwarded != 250000 {
		t.Errorf("落库内容不对: status=%s quota=%d", rec.Status, rec.QuotaAwarded)
	}
	// 站点原始响应要带进结果，日志里才能看到站点到底回了什么。
	if !strings.Contains(res.Response, "签到成功，获得 $25 额度") {
		t.Errorf("结果里应保留站点原始响应，实际 %q", res.Response)
	}
}

// TestRunForkStatusEndpointUnsupported 覆盖 auto 模式的自动降级：
// 站点没有状态查询接口（GET 返回 404）时，只记一条警告并继续提交，不能因此判失败。
func TestRunForkStatusEndpointUnsupported(t *testing.T) {
	fake := &fakeSite{
		checkinEnabled: true,
		checkinPath:    "/api/user/sign_in",
		forkStyle:      true,
		award:          250000,
		quota:          1000000,
	}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	cfg.Sites[0].CheckinPath = "/api/user/sign_in"
	cfg.Sites[0].CheckinStatus = config.CheckinStatusAuto

	runner, _ := newTestRunner(t, cfg, nil)
	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusSuccess {
		t.Fatalf("状态接口不存在时仍应完成签到，实际 %s（%s）", res.Status, res.Message)
	}
	if fake.statusQueries != 1 {
		t.Errorf("auto 模式下应尝试过一次状态预查，实际 %d 次", fake.statusQueries)
	}
	if fake.postCalls != 1 {
		t.Errorf("降级后应正常提交签到，实际 %d 次", fake.postCalls)
	}
}

// TestRunForkWithoutCheckinFieldInStatus 覆盖「/api/status 未声明签到开关」的 fork：
// 不能因此在步骤 1 就判失败，应当继续把签到做完（真机上就有这种站点）。
func TestRunForkWithoutCheckinFieldInStatus(t *testing.T) {
	fake := &fakeSite{
		checkinPath: "/api/user/sign_in",
		forkStyle:   true,
		statusBody:  `{"success":true,"data":{"version":"v0.0.0","system_name":"Any Router"}}`,
		award:       250000,
		quota:       1000000,
	}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	cfg.Sites[0].CheckinPath = "/api/user/sign_in"
	cfg.Sites[0].CheckinStatus = config.CheckinStatusOff

	runner, _ := newTestRunner(t, cfg, nil)
	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusSuccess {
		t.Fatalf("未声明签到开关时仍应完成签到，实际 %s（%s）", res.Status, res.Message)
	}
	if fake.postCalls != 1 {
		t.Errorf("应提交一次签到，实际 %d 次", fake.postCalls)
	}
	if res.QuotaAwarded != 250000 {
		t.Errorf("额度应按余额差补齐，实际 %d", res.QuotaAwarded)
	}
}

// TestRunWithSiteProxy 验证 sites[].proxy 被真正使用，并且出现在结果里（会进日志）。
//
// 技巧：把假站点本身当作代理 —— 请求会以「绝对 URI」形式打到它，而 ServeMux 仍按路径匹配，
// 因此它照样正常应答；能跑通就说明请求确实走了代理这条路径。
func TestRunWithSiteProxy(t *testing.T) {
	fake := &fakeSite{checkinEnabled: true, award: 1000, quota: 100000}
	srv := newFakeSiteServer(t, fake)
	cfg := testConfig(t, srv.URL)
	cfg.Sites[0].Proxy = srv.URL

	runner, _ := newTestRunner(t, cfg, nil)
	summary, err := runner.Run(context.Background(), RunOptions{Trigger: TriggerManual})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	res := summary.Results[0]
	if res.Status != model.StatusSuccess {
		t.Fatalf("配置代理后应仍能签到，实际 %s（%s）", res.Status, res.Message)
	}
	if res.Proxy != srv.URL {
		t.Errorf("结果里应带上生效的代理，实际 %q", res.Proxy)
	}
}
