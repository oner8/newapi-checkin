// Package checkin 编排整个签到流程：并发跑站点、重试、落库、通知。
package checkin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"newapi-checkin/internal/config"
	"newapi-checkin/internal/model"
	"newapi-checkin/internal/notify"
	"newapi-checkin/internal/scheduler"
	"newapi-checkin/internal/site"
)

// 批次触发来源。
const (
	TriggerSchedule = "schedule"
	TriggerStartup  = "startup"
	TriggerManual   = "manual"
)

// defaultQuotaPerUnit 是 new-api 的默认额度换算单位（500000 额度 = $1）。
const defaultQuotaPerUnit = 500000

// maxRetryDelay 是单次重试等待的上限，防止目标站用超大 Retry-After 把批次挂住。
const maxRetryDelay = 10 * time.Minute

// ErrRunInProgress 表示已有一批签到正在执行。
var ErrRunInProgress = errors.New("已有一批签到正在执行，请稍后再试")

// ErrSiteNotFound 表示指定的站点不存在或未启用。
var ErrSiteNotFound = errors.New("未找到指定的站点")

// ErrRunCanceled 表示批次在执行过程中被取消（例如收到 SIGTERM 或批次超时）。
//
// 这种情况下不落库、不推送：留半成品记录会把当天误判成"已经跑过"，
// 反而让启动补跑不再补签，而且会把当天原本成功的结果覆盖成失败。
var ErrRunCanceled = errors.New("批次被取消")

// SiteResult 是单个站点的签到结果。
type SiteResult struct {
	SiteName       string       `json:"site_name"`
	Status         model.Status `json:"status"`
	Message        string       `json:"message"`
	Username       string       `json:"username,omitempty"`
	Credential     string       `json:"credential,omitempty"`
	SiteVersion    string       `json:"site_version,omitempty"`
	TurnstileCheck bool         `json:"turnstile_check,omitempty"`
	QuotaAwarded   int64        `json:"quota_awarded"`
	QuotaBefore    *int64       `json:"quota_before,omitempty"`
	QuotaAfter     *int64       `json:"quota_after,omitempty"`
	QuotaPerUnit   float64      `json:"quota_per_unit"`
	// CheckinDate 是站点返回的签到日期（站点本地时区）；为空时回退到配置时区的当天。
	CheckinDate string `json:"checkin_date,omitempty"`
	HTTPStatus  int    `json:"http_status"`
	LatencyMS   int64  `json:"latency_ms"`
	Attempts    int    `json:"attempts"`
	// Response 是站点对关键请求的原始响应片段（提交签到，或判定"今日已签到"的那次状态查询），
	// 便于在日志里直接看到站点到底回了什么。只用于日志与展示，不落库、也不进通知。
	Response string `json:"response,omitempty"`
	// Proxy 是该站点生效的代理（凭据已打码），便于排查"某个站走了哪条出口"。
	Proxy string `json:"proxy,omitempty"`
}

// Summary 是一批签到的汇总。
type Summary struct {
	RunID      string       `json:"run_id"`
	Trigger    string       `json:"trigger"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
	Results    []SiteResult `json:"results"`
	// SiteFilter 记录本批次是否只跑了单个站点（--site / ?site=）：
	// 判断"能否推送全绿汇总"需要它。
	SiteFilter string `json:"site_filter,omitempty"`
	// Notified 记录本次是否真的推送了通知。
	Notified bool `json:"notified"`
}

// Total 返回站点数。
func (s *Summary) Total() int { return len(s.Results) }

// Succeeded 返回不算失败的站点数（成功 / 已签到 / 未开启签到 / 试运行）。
func (s *Summary) Succeeded() int {
	n := 0
	for _, r := range s.Results {
		if r.Status.IsOK() {
			n++
		}
	}
	return n
}

// Failed 返回失败站点数。
func (s *Summary) Failed() int { return s.Total() - s.Succeeded() }

// Failures 返回失败结果（按配置顺序）。
func (s *Summary) Failures() []SiteResult {
	var out []SiteResult
	for _, r := range s.Results {
		if !r.Status.IsOK() {
			out = append(out, r)
		}
	}
	return out
}

// QuotaAwarded 返回本批获得的额度总和。
func (s *Summary) QuotaAwarded() int64 {
	var total int64
	for _, r := range s.Results {
		total += r.QuotaAwarded
	}
	return total
}

// Duration 返回批次耗时。
func (s *Summary) Duration() time.Duration {
	if s.FinishedAt.IsZero() {
		return 0
	}
	return s.FinishedAt.Sub(s.StartedAt)
}

// Line 返回单行摘要，用于日志与 run_logs.summary。
func (s *Summary) Line() string {
	return fmt.Sprintf("trigger=%s total=%d ok=%d failed=%d quota=%d duration=%s",
		s.Trigger, s.Total(), s.Succeeded(), s.Failed(), s.QuotaAwarded(), s.Duration().Round(time.Second))
}

// RunOptions 描述一次批次执行。
type RunOptions struct {
	// Trigger 为 schedule / startup / manual。
	Trigger string
	// Site 非空时只跑该站点。
	Site string
	// RunID 非空时沿用调用方生成的批次号（HTTP 触发需要提前返回它）。
	RunID string
}

// Runner 执行签到批次；同一时刻只允许一批。
type Runner struct {
	db       *gorm.DB
	cfg      *config.Config
	schedule scheduler.Schedule
	loc      *time.Location
	notifier *notify.Multi
	log      *slog.Logger
	// retryBackoff 是可注入的重试等待间隔（测试里会调小，避免测试真的等 60 秒）。
	retryBackoff time.Duration

	mu      sync.Mutex
	running bool
	lastRun *Summary
	now     func() time.Time
}

// NewRunner 构造 Runner。
func NewRunner(db *gorm.DB, cfg *config.Config, notifier *notify.Multi, log *slog.Logger) (*Runner, error) {
	loc, err := cfg.App.Location()
	if err != nil {
		return nil, fmt.Errorf("时区 %q 无效: %w", cfg.App.Timezone, err)
	}
	hour, minute, err := config.ParseSchedule(cfg.App.Schedule)
	if err != nil {
		return nil, fmt.Errorf("schedule %q 无效: %w", cfg.App.Schedule, err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		db:           db,
		cfg:          cfg,
		schedule:     scheduler.New(hour, minute, cfg.App.Jitter(), loc),
		loc:          loc,
		notifier:     notifier,
		log:          log,
		now:          time.Now,
		retryBackoff: cfg.App.RetryBackoff(),
	}, nil
}

// Schedule 返回调度配置（供 /healthz 展示下次执行时间）。
func (r *Runner) Schedule() scheduler.Schedule { return r.schedule }

// Location 返回配置时区。
func (r *Runner) Location() *time.Location { return r.loc }

// Running 返回是否有批次在执行。
func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// LastRun 返回最近一次批次汇总。
func (r *Runner) LastRun() *Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastRun
}

// Begin 尝试占用执行权；失败表示已有批次在跑。
func (r *Runner) Begin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

// Finish 释放执行权。
func (r *Runner) Finish() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
}

// Run 占用执行权并跑完一批（同步）。
func (r *Runner) Run(ctx context.Context, opts RunOptions) (*Summary, error) {
	if !r.Begin() {
		return nil, ErrRunInProgress
	}
	defer r.Finish()
	return r.RunReserved(ctx, opts)
}

// RunReserved 在已通过 Begin 占位的前提下执行批次。
func (r *Runner) RunReserved(ctx context.Context, opts RunOptions) (summary *Summary, err error) {
	// 批次级 panic 隔离：常驻进程不能因为一次异常响应就整体退出。
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("批次执行 panic，已隔离", "run_id", opts.RunID, "panic", fmt.Sprint(rec))
			summary, err = nil, fmt.Errorf("批次执行内部错误: %v", rec)
		}
	}()
	trigger := opts.Trigger
	if trigger == "" {
		trigger = TriggerManual
	}
	runID := opts.RunID
	if runID == "" {
		runID = newRunID(trigger, r.now())
	}

	sites, err := r.selectSites(opts.Site)
	if err != nil {
		return nil, err
	}

	summary = &Summary{RunID: runID, Trigger: trigger, StartedAt: r.now(), SiteFilter: opts.Site}
	if len(sites) == 0 {
		summary.FinishedAt = r.now()
		r.log.Warn("没有启用的站点，跳过本次签到")
		return summary, nil
	}

	r.log.Info("开始签到批次",
		"run_id", runID, "trigger", trigger, "sites", len(sites),
		"concurrency", r.cfg.App.Concurrency, "dry_run", r.cfg.App.DryRun)

	results := make([]SiteResult, len(sites))
	sem := make(chan struct{}, max(1, r.cfg.App.Concurrency))
	var wg sync.WaitGroup
	batchStart := summary.StartedAt

	for i, s := range sites {
		wg.Add(1)
		go func(i int, s config.Site) {
			defer wg.Done()
			// 定时批次才抖动：手动重跑/启动补跑应当立刻执行。
			if trigger == TriggerSchedule {
				r.waitJitter(ctx, s.Name, batchStart)
			}
			sem <- struct{}{}
			defer func() { <-sem }()

			results[i] = r.safeAttempt(ctx, s)
			fields := []any{
				"site", s.Name,
				"status", string(results[i].Status),
				"quota_awarded", results[i].QuotaAwarded,
				"attempts", results[i].Attempts,
				"latency_ms", results[i].LatencyMS,
				"message", results[i].Message,
			}
			// 站点原始响应片段：排查 fork / 异常时最有用的一条信息。
			if results[i].Response != "" {
				fields = append(fields, "response", results[i].Response)
			}
			if results[i].Proxy != "" {
				fields = append(fields, "proxy", results[i].Proxy)
			}
			r.log.Info("站点签到结果", fields...)
		}(i, s)
	}
	wg.Wait()

	summary.Results = results
	summary.FinishedAt = r.now()
	r.log.Info("签到批次结束", "run_id", runID, "summary", summary.Line())

	if ctxErr := ctx.Err(); ctxErr != nil {
		// 批次被取消（超过批次时间上限，或进程正在退出）：只保留已经有结论的站点结果。
		r.persistConfirmedOnly(summary)
		r.log.Warn("批次被取消，仅保留已确认的结果",
			"run_id", runID, "reason", ctxErr)
		return summary, fmt.Errorf("%w: %w", ErrRunCanceled, ctxErr)
	}

	r.persist(summary, true)
	r.notifySummary(ctx, summary)
	r.mu.Lock()
	r.lastRun = summary
	r.mu.Unlock()
	return summary, nil
}

// selectSites 返回要处理的站点；opts.Site 非空时只返回该站点。
func (r *Runner) selectSites(name string) ([]config.Site, error) {
	enabled := r.cfg.EnabledSites()
	if strings.TrimSpace(name) == "" {
		return enabled, nil
	}
	for _, s := range enabled {
		if s.Name == name {
			return []config.Site{s}, nil
		}
	}
	// 站点存在但被禁用时给出更准确的信息。
	if _, ok := r.cfg.SiteByName(name); ok {
		return nil, fmt.Errorf("%w: 站点 %s 已禁用（enabled: false）", ErrSiteNotFound, name)
	}
	return nil, fmt.Errorf("%w: %s", ErrSiteNotFound, name)
}

// waitJitter 在定时批次里把站点错开，避免所有站点同一秒并发。
func (r *Runner) waitJitter(ctx context.Context, siteName string, batchStart time.Time) {
	delay := r.schedule.JitterFor(siteName, batchStart)
	if delay <= 0 {
		return
	}
	wait := time.Until(batchStart.Add(delay))
	if wait <= 0 {
		return
	}
	r.log.Debug("按抖动推迟该站点", "site", siteName, "delay", delay.Round(time.Second).String())
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// attempt 完成单个站点的完整流程，并在返回前打上端到端耗时。
//
// 注意：耗时必须在包装层赋值。attemptSite 有多个 return 分支，若把
// res.LatencyMS 放进 attemptSite 内部的 defer，赋值会发生在返回值拷贝之后，
// 于是 latency_ms 永远是 0（这个 bug 曾被线上日志"latency_ms=0"掩盖）。
func (r *Runner) attempt(ctx context.Context, s config.Site) SiteResult {
	started := time.Now()
	res := r.attemptSite(ctx, s)
	res.LatencyMS = time.Since(started).Milliseconds()
	return res
}

// attemptSite 完成单个站点的完整流程（不含耗时统计）。
func (r *Runner) attemptSite(ctx context.Context, s config.Site) SiteResult {
	res := SiteResult{SiteName: s.Name, Status: model.StatusError, QuotaPerUnit: defaultQuotaPerUnit}

	client, err := site.NewClientFromSite(s, r.cfg.App)
	if err != nil {
		res.Message = err.Error()
		return res
	}
	res.Credential = client.Credential().Describe()
	res.Proxy = client.ProxyDescription()

	// 前置防护的 JS 挑战被自动通过时留一条日志：否则用户会以为站点没防护，
	// 或者误以为是通过了别的手段（例如粘贴的 Cookie 还有效）。
	defer func() {
		if client.WAFSolved() {
			r.log.Info("已自动通过站点前置防护的 JS 挑战（acw_sc__v2）", "site", s.Name)
		}
	}()

	// 步骤 1：公开接口探测站点能力。
	var status *site.Status
	if err := r.withRetry(ctx, &res, func(ctx context.Context) error {
		var callErr error
		status, callErr = client.Status(ctx)
		return callErr
	}); err != nil {
		return finishWithError(res, err)
	}
	res.SiteVersion = status.Version
	res.TurnstileCheck = status.TurnstileCheck
	if status.QuotaPerUnit > 0 {
		res.QuotaPerUnit = status.QuotaPerUnit
	}
	if status.CheckinSupportUnknown {
		r.log.Warn("站点 /api/status 未声明签到开关，按开启处理并继续尝试",
			"site", s.Name, "version", status.Version)
	}
	if !status.CheckinEnabled {
		res.Status = model.StatusSkippedDisabled
		res.Message = "站点未开启签到功能"
		return res
	}

	// 步骤 2：校验凭据并记录签到前余额。
	var self *site.Self
	if err := r.withRetry(ctx, &res, func(ctx context.Context) error {
		var callErr error
		self, callErr = client.Self(ctx)
		return callErr
	}); err != nil {
		return finishWithError(res, err)
	}
	res.Username = self.Username
	quotaBefore := self.Quota
	res.QuotaBefore = &quotaBefore

	if r.cfg.App.DryRun {
		res.Status = model.StatusDryRun
		res.Message = "试运行（dry_run=true），未提交签到"
		return res
	}

	// 步骤 3：查当月签到状态，已签到就不重复提交。
	//
	// 这一步是"尽力而为"的：部分 fork 根本没有签到状态接口（例如签到路径被改成
	// /api/user/sign_in 的站点），不能因为查状态失败就放弃一次本来可以成功的签到。
	stats, preErr := r.precheckStatus(ctx, client, s, &res)
	if preErr != nil {
		return finishWithError(res, preErr)
	}
	if stats != nil && stats.CheckedInToday {
		res.Status = model.StatusAlready
		res.Message = "今日已签到"
		res.Response = stats.Raw
		after := quotaBefore
		res.QuotaAfter = &after
		return res
	}

	// 步骤 4：提交签到。
	var result *site.CheckinResult
	if err := r.withRetry(ctx, &res, func(ctx context.Context) error {
		var callErr error
		result, callErr = client.DoCheckin(ctx)
		return callErr
	}); err != nil {
		return finishWithError(res, err)
	}

	res.Status = model.StatusSuccess
	res.QuotaAwarded = result.QuotaAwarded
	res.Response = result.Raw
	res.Message = strings.TrimSpace(result.Message)
	if res.Message == "" {
		res.Message = "签到成功"
	}
	// 以站点返回的签到日期为准：站点按自己的时区判自然日，配置时区与站点不一致时，
	// 用站点日期才不会把同一次签到记成两天。
	res.CheckinDate = normalizeCheckinDate(result.CheckinDate)

	// 步骤 5：再取一次余额，便于对账与展示。
	var after *site.Self
	if err := r.withRetry(ctx, &res, func(ctx context.Context) error {
		var callErr error
		after, callErr = client.Self(ctx)
		return callErr
	}); err == nil && after != nil {
		quotaAfter := after.Quota
		res.QuotaAfter = &quotaAfter
		if res.QuotaAwarded == 0 && quotaAfter > quotaBefore {
			res.QuotaAwarded = quotaAfter - quotaBefore
		}
	} else if err != nil {
		// 拿不到余额不影响签到结论，记一条日志即可。
		r.log.Debug("签到后读取余额失败", "site", s.Name, "error", err)
	}
	if res.QuotaAfter == nil {
		after := quotaBefore + res.QuotaAwarded
		res.QuotaAfter = &after
	}
	return res
}

// precheckStatus 查当月签到状态（尽力而为）。返回 (nil, nil) 表示"跳过预查，直接提交"。
//
// 三种情形：
//   - 配置里写了 checkin_status: off —— 直接跳过（该 fork 没有状态接口）；
//   - 查询成功 —— 返回真实统计；
//   - 站点对状态查询回应"没这个接口 / 结构不对"（HTTP 404/405 会落到 KindBusiness，
//     解析不出 stats 会落到 KindDecode）—— 记一条警告后跳过，继续去提交签到。
//
// 网络错误、5xx、凭据失效仍然照常上报：那说明站点或凭据确实有问题，不该假装没事。
func (r *Runner) precheckStatus(ctx context.Context, client *site.Client, s config.Site, res *SiteResult) (*site.CheckinStats, error) {
	if s.CheckinStatus == config.CheckinStatusOff {
		r.log.Debug("站点声明没有签到状态接口，跳过预查直接提交", "site", s.Name)
		return nil, nil
	}

	month := r.now().In(r.loc).Format("2006-01")
	var stats *site.CheckinStats
	err := r.withRetry(ctx, res, func(ctx context.Context) error {
		var callErr error
		stats, callErr = client.CheckinStatus(ctx, month)
		return callErr
	})
	if err == nil {
		return stats, nil
	}
	// 站点把"今日已签到"当成错误返回（HTTP 200 + success:false）。
	if site.IsKind(err, site.KindAlreadyCheckedIn) {
		return &site.CheckinStats{CheckedInToday: true}, nil
	}
	if site.IsKind(err, site.KindDecode) || site.IsKind(err, site.KindBusiness) {
		r.log.Warn("站点没有可用的签到状态查询接口，跳过预查直接提交（确认的话可写 checkin_status: off 省掉这次请求）",
			"site", s.Name, "error", err)
		return nil, nil
	}
	return nil, err
}

// finishWithError 把站点错误映射成状态并写进结果。
func finishWithError(res SiteResult, err error) SiteResult {
	res.Status = statusFromError(err)
	res.Message = err.Error()
	return res
}

// statusFromError 把站点错误分类映射为批次状态。
func statusFromError(err error) model.Status {
	switch site.KindOf(err) {
	case site.KindAuthFailed:
		return model.StatusAuthFailed
	case site.KindTurnstile:
		return model.StatusNeedTurnstile
	case site.KindAlreadyCheckedIn:
		return model.StatusAlready
	case site.KindCheckinDisabled:
		return model.StatusSkippedDisabled
	case site.KindNotNewAPI:
		return model.StatusNotNewAPI
	case site.KindBlocked:
		return model.StatusBlocked
	default:
		return model.StatusError
	}
}

// withRetry 对可重试错误做指数退避重试，并把尝试次数与 HTTP 状态写回结果。
func (r *Runner) withRetry(ctx context.Context, res *SiteResult, fn func(context.Context) error) error {
	attempts := max(1, r.cfg.App.Retries+1)
	backoff := r.retryBackoff
	var lastErr error

	for i := 0; i < attempts; i++ {
		res.Attempts++
		err := fn(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if e := site.AsError(err); e != nil && e.HTTPStatus > 0 {
			res.HTTPStatus = e.HTTPStatus
		}
		if !site.Retryable(err) {
			return err
		}
		if i == attempts-1 {
			break
		}
		delay := backoff * (1 << i)
		if e := site.AsError(err); e != nil && e.RetryAfter > delay {
			delay = e.RetryAfter
		}
		// 夹住上限：站点返回 Retry-After: 86400 时不能真睡一天，
		// 否则该站点会长期占住批次锁，把当天的定时批次挤掉。
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
		r.log.Warn("请求失败，准备重试",
			"site", res.SiteName, "attempt", i+1, "of", attempts,
			"retry_in", delay.Round(time.Second).String(), "error", err)
		if !sleepCtx(ctx, delay) {
			return lastErr
		}
	}
	return lastErr
}

// sleepCtx 可被取消的睡眠；返回 false 表示 ctx 已结束。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// persist 把批次结果写入数据库。
//
// writeRunLog 为 false 时不写 run_logs：批次被取消时用它，好让启动补跑仍能识别出
// "完整批次没跑成"，去补其它站点（run_logs 有当天记录就意味着今天已经跑过）。
func (r *Runner) persist(summary *Summary, writeRunLog bool) {
	if r.db == nil {
		return
	}
	fallbackDay := summary.StartedAt.In(r.loc).Format("2006-01-02")
	for _, res := range summary.Results {
		rec := &model.CheckinRecord{
			SiteName:     res.SiteName,
			CheckinDate:  firstNonEmptyDay(res.CheckinDate, fallbackDay),
			Status:       string(res.Status),
			Message:      truncateRunes(res.Message, 500),
			QuotaAwarded: res.QuotaAwarded,
			QuotaBefore:  res.QuotaBefore,
			QuotaAfter:   res.QuotaAfter,
			QuotaPerUnit: res.QuotaPerUnit,
			HTTPStatus:   res.HTTPStatus,
			LatencyMS:    res.LatencyMS,
			Attempts:     res.Attempts,
			RunID:        summary.RunID,
			Trigger:      summary.Trigger,
		}
		if keep, keptStatus := r.shouldKeepConfirmed(rec); keep {
			r.log.Debug("当天已有确认过的签到结果，本次不覆盖",
				"site", res.SiteName, "date", rec.CheckinDate,
				"kept_status", keptStatus, "new_status", string(res.Status))
			continue
		}
		if err := model.UpsertCheckin(r.db, rec); err != nil {
			r.log.Error("写入签到记录失败", "site", res.SiteName, "error", err)
		}
	}

	if writeRunLog {
		run := &model.RunLog{
			RunID:      summary.RunID,
			Trigger:    summary.Trigger,
			StartedAt:  summary.StartedAt,
			FinishedAt: summary.FinishedAt,
			Total:      summary.Total(),
			Succeeded:  summary.Succeeded(),
			Failed:     summary.Failed(),
			Summary:    truncateRunes(summary.Line(), 1000),
		}
		if err := r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(run).Error; err != nil {
			r.log.Error("写入批次记录失败", "run_id", summary.RunID, "error", err)
		}
	}
}

// isConfirmedStatus 判断状态是否已经是"当天签到有结论"。
func isConfirmedStatus(status model.Status) bool {
	return status == model.StatusSuccess || status == model.StatusAlready
}

// shouldKeepConfirmed 判断是否应当保留库里当天已有的结论、放弃写入本次结果。
//
// 只拦一种情况：库里当天已是 success/already，而本次结果没有结论。典型场景：
//   - 按 README 用 --dry-run 验证凭据，不该把当天真实签到抹成 dry_run；
//   - 收到 SIGTERM 时在跑的批次里所有请求都因 ctx 取消而失败，
//     不该把早上已经成功写入的记录改写成 error。
func (r *Runner) shouldKeepConfirmed(rec *model.CheckinRecord) (bool, string) {
	if r.db == nil || isConfirmedStatus(model.Status(rec.Status)) {
		return false, ""
	}
	existing, err := model.FindCheckin(r.db, rec.SiteName, rec.CheckinDate)
	if err != nil {
		r.log.Warn("查询当天已有签到记录失败，按覆盖处理", "site", rec.SiteName, "error", err)
		return false, ""
	}
	if existing == nil || !isConfirmedStatus(model.Status(existing.Status)) {
		return false, ""
	}
	return true, existing.Status
}

// safeAttempt 包一层 recover：单个站点出现 panic 只应让该站点失败，
// 而不是把整个常驻进程带走（子 goroutine 的 panic 无法被父层的 recover 捕获）。
//
// 返回值必须具名：panic 时 return 语句根本不会完成赋值，只有具名返回值才能把
// "已隔离的错误结果"带回给调用方，否则会落一条站点名为空的幽灵记录。
func (r *Runner) safeAttempt(ctx context.Context, s config.Site) (result SiteResult) {
	result = SiteResult{SiteName: s.Name, QuotaPerUnit: defaultQuotaPerUnit}
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("站点处理 panic，已隔离", "site", s.Name, "panic", fmt.Sprint(rec))
			result.Status = model.StatusError
			result.Message = fmt.Sprintf("内部错误（panic）: %v", rec)
		}
	}()
	return r.attempt(ctx, s)
}

// notifySummary 组装并推送通知，同时处理失败去重。
func (r *Runner) notifySummary(ctx context.Context, summary *Summary) {
	if r.notifier == nil || !r.notifier.Enabled() {
		r.log.Debug("未配置通知渠道，跳过推送")
		return
	}
	mode := r.cfg.Notify.Bark.Mode
	day := summary.StartedAt.In(r.loc).Format("2006-01-02")

	// 失败去重：同一站点、同一天、同一状态只推一次，避免重试/手动重跑刷屏。
	// always 模式的语义是"每次都推"，因此跳过去重、带上全部失败项。
	var (
		fresh      []SiteResult
		claimedKey []string
	)
	if mode == config.BarkModeAlways {
		fresh = summary.Failures()
	} else {
		for _, f := range summary.Failures() {
			key := fmt.Sprintf("fail|%s|%s|%s", day, f.SiteName, f.Status)
			ok, err := model.ClaimNotifyDedup(r.db, key)
			if err != nil {
				r.log.Warn("通知去重检查失败，按未推送处理", "site", f.SiteName, "error", err)
				ok = true
			}
			if ok {
				fresh = append(fresh, f)
				claimedKey = append(claimedKey, key)
			}
		}
	}

	msg := RenderMessage(summary, fresh, r.cfg.Notify.Bark.Level)

	shouldSend := false
	switch mode {
	case config.BarkModeAlways:
		shouldSend = true
	case config.BarkModeFailure:
		shouldSend = len(fresh) > 0
	default: // summary：有失败就推；覆盖全部站点且全绿时，每天推一条成功汇总
		if len(fresh) > 0 {
			shouldSend = true
		} else if summary.Total() > 0 && summary.Failed() == 0 && summary.Total() == r.expectedSites(summary) {
			// 去重键只按天、不带站点数：否则「全量批次」与「?site=X 的批次」各自算一次，
			// 同一天会推两条"签到完成"，而且其中一条会掩盖当天其它站点的失败。
			key := fmt.Sprintf("ok|%s", day)
			if ok, err := model.ClaimNotifyDedup(r.db, key); err == nil && ok {
				claimedKey = append(claimedKey, key)
				shouldSend = true
			}
		}
	}

	if !shouldSend {
		r.log.Debug("按通知策略跳过推送", "mode", mode, "fresh_failures", len(fresh))
		return
	}

	if err := r.notifier.Notify(ctx, msg); err != nil {
		// 推送失败则释放去重键，让下一次运行还能再试。
		for _, key := range claimedKey {
			if releaseErr := model.ReleaseNotifyDedup(r.db, key); releaseErr != nil {
				r.log.Warn("释放通知去重键失败", "key", key, "error", releaseErr)
			}
		}
		r.log.Error("通知推送失败", "error", err)
		return
	}
	summary.Notified = true
}

// newRunID 生成批次号。
func newRunID(trigger string, at time.Time) string {
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s-%s", trigger, at.Format("20060102T150405"))
	}
	return fmt.Sprintf("%s-%s-%s", trigger, at.Format("20060102T150405"), hex.EncodeToString(buf[:]))
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

// normalizeCheckinDate 校验站点返回的签到日期，格式不对时返回空串（由调用方回退）。
func normalizeCheckinDate(value string) string {
	v := strings.TrimSpace(value)
	if len(v) != 10 {
		return ""
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return ""
	}
	return v
}

// firstNonEmptyDay 返回第一个非空的日期。
func firstNonEmptyDay(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// expectedSites 返回本次运行"应当覆盖"的站点数，用于判断能否推送全绿汇总。
//
// 用 --site / ?site= 只跑一个站点时，用户不期望收到"全站全绿"的汇总；
// 但也正因为只跑了 1 个站点，这里把它当作覆盖完整，否则这类用户永远收不到成功汇总。
func (r *Runner) expectedSites(summary *Summary) int {
	if strings.TrimSpace(summary.SiteFilter) != "" {
		return 1
	}
	return len(r.cfg.EnabledSites())
}

// persistConfirmedOnly 只把"当天已有结论"的结果交给 persist（批次被取消时使用）。
//
// 远端很可能真的已经签到、额度已经发过，丢了就对不上账；同时不写 run_logs，
// 让当天的启动补跑仍能识别出"完整批次没跑成"，去补其它站点。
func (r *Runner) persistConfirmedOnly(summary *Summary) {
	var kept []SiteResult
	for _, res := range summary.Results {
		if isConfirmedStatus(res.Status) {
			kept = append(kept, res)
		}
	}
	if len(kept) == 0 {
		return
	}
	sub := *summary
	sub.Results = kept
	r.persist(&sub, false)
}
