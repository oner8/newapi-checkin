package checkin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gorm.io/gorm"

	"newapi-checkin/internal/config"
	"newapi-checkin/internal/model"
	"newapi-checkin/internal/notify"
	"newapi-checkin/internal/scheduler"
)

// Service 是对外门面：定时循环、HTTP 接口、启动补跑都通过它触发批次。
type Service struct {
	cfg      *config.Config
	runner   *Runner
	loc      *time.Location
	schedule scheduler.Schedule
	log      *slog.Logger

	mu      sync.Mutex
	nextRun time.Time

	// 定时批次冲突时的等待策略（测试里可调小）。
	busyRetryInterval time.Duration
	busyWaitBudget    time.Duration
}

// NewService 构造服务。
func NewService(db *gorm.DB, cfg *config.Config, notifier *notify.Multi, log *slog.Logger) (*Service, error) {
	runner, err := NewRunner(db, cfg, notifier, log)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg:               cfg,
		runner:            runner,
		loc:               runner.Location(),
		schedule:          runner.Schedule(),
		log:               log,
		busyRetryInterval: defaultBusyRetryInterval,
		busyWaitBudget:    defaultBusyWaitBudget,
	}, nil
}

// Config 返回配置。
func (s *Service) Config() *config.Config { return s.cfg }

// Location 返回配置时区。
func (s *Service) Location() *time.Location { return s.loc }

// Schedule 返回调度配置。
func (s *Service) Schedule() scheduler.Schedule { return s.schedule }

// Running 返回是否有批次在执行。
func (s *Service) Running() bool { return s.runner.Running() }

// LastRun 返回最近一次批次汇总。
func (s *Service) LastRun() *Summary { return s.runner.LastRun() }

// NextRun 返回下一次定时触发时间；未启动调度时为零值。
func (s *Service) NextRun() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextRun
}

// SetNextRun 记录下一次触发时间（调度循环每次重算后调用）。
func (s *Service) SetNextRun(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextRun = t
}

// RunOnce 同步执行一批签到。
func (s *Service) RunOnce(ctx context.Context, trigger, siteName string) (*Summary, error) {
	return s.runner.Run(ctx, RunOptions{Trigger: trigger, Site: siteName})
}

// Trigger 异步触发一批签到，立即返回批次号。
//
// 这里先在校验阶段占用执行权，保证并发触发能被立刻拒绝（HTTP 可以返回 409），
// 而不是先返回批次号再静默失败。
func (s *Service) Trigger(ctx context.Context, siteName string) (string, error) {
	if _, err := s.runner.selectSites(siteName); err != nil {
		return "", err
	}
	if !s.runner.Begin() {
		return "", ErrRunInProgress
	}

	runID := newRunID(TriggerManual, time.Now())
	go func() {
		defer s.runner.Finish()
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("手动批次 goroutine panic，已隔离", "run_id", runID, "panic", fmt.Sprint(rec))
			}
		}()
		// 与发起请求的生命周期解耦：客户端断开不应中断已开始的签到。
		bg := context.WithoutCancel(ctx)
		if _, err := s.runner.RunReserved(bg, RunOptions{
			Trigger: TriggerManual,
			Site:    siteName,
			RunID:   runID,
		}); err != nil {
			s.log.Error("手动触发的签到批次失败", "run_id", runID, "error", err)
		}
	}()
	return runID, nil
}

// StartupCatchUp 在容器启动时判断当天是否漏签，需要时补跑一次。
//
// 判断依据是 run_logs 里当天有没有任何批次记录：只要今天跑过（定时或补跑），
// 反复重启就不会重复执行；如果重启时今天还没跑过，就立刻补一次，避免漏签。
func (s *Service) StartupCatchUp(ctx context.Context) (bool, error) {
	if !s.cfg.App.RunOnStart {
		s.log.Debug("run_on_start=false，跳过启动补跑")
		return false, nil
	}
	if s.runner.db == nil {
		return false, nil
	}
	now := time.Now()
	hasRun, err := model.HasRunOnDate(s.runner.db, now, s.loc)
	if err != nil {
		return false, fmt.Errorf("检查当天执行记录失败: %w", err)
	}
	if hasRun {
		s.log.Info("今天已经执行过签到批次，跳过启动补跑",
			"date", now.In(s.loc).Format("2006-01-02"))
		return false, nil
	}

	s.log.Info("启动补跑：今天还没有执行过签到",
		"date", now.In(s.loc).Format("2006-01-02"),
		"schedule", s.schedule.String())
	if _, err := s.RunWithBusyWait(ctx, TriggerStartup, ""); err != nil {
		return true, err
	}
	return true, nil
}

// maxBatchDuration 是一批签到的时间上限。
//
// 批次上下文必须与进程信号解耦：否则收到 SIGTERM 时，在飞的请求会被取消、结果被判成
// 失败，进而把当天"其实已经成功"的记录覆盖掉。但也必须有界，避免卡死的批次永久占用
// 执行权。默认配置下的最坏情况（抖动 10 分钟 + 每站 3 次请求超时与指数退避的并发波次）
// 远小于 30 分钟。
const maxBatchDuration = 30 * time.Minute

// BatchContext 返回后台批次（定时、启动补跑）使用的上下文。
//
// 用它而不是直接用信号 ctx：SIGTERM 时进程会退出，此时尚未落库的批次什么都不写，
// 于是当天不会被误判成"已经跑过"，容器重启后由 run_on_start 重新补跑。
// 手动触发（HTTP / --run-once）不走这里：前者已经与请求生命周期解耦，后者应当可被 Ctrl-C 中断。
func (s *Service) BatchContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), maxBatchDuration)
}

// 定时批次遇到"已有批次在跑"时的等待策略。
//
// 等待预算必须明显小于单批时间上限（maxBatchDuration）：否则等待本身会把批次上下文
// 的预算吃光，等到执行权时 ctx 已经过期，所有请求立刻失败，当天照样漏签。
const (
	defaultBusyRetryInterval = 30 * time.Second
	defaultBusyWaitBudget    = 5 * time.Minute
)

// RunScheduled 执行定时批次，并在已有批次占用执行权时等待它结束再执行。
//
// 直接 Run 会在冲突时返回 ErrRunInProgress 而被调度循环丢掉，当天就不会再签到——这是漏签的
// 主要来源：定时点前几分钟刚好有人在面板点了"立即签到"，或上一批还卡在退避重试里。
func (s *Service) RunScheduled(parent context.Context) (*Summary, error) {
	return s.RunWithBusyWait(parent, TriggerSchedule, "")
}

// RunWithBusyWait 执行一批签到，并在已有批次占用执行权时等待后重试；定时与启动补跑共用。
//
// 直接 Run 会在冲突时返回 ErrRunInProgress 而被调用方丢掉，当天就不会再签到——这是漏签的
// 主要来源：定时点前几分钟刚好有人在面板点了"立即签到"，或上一批还卡在退避重试里。
// 启动补跑同理：HTTP 服务比补跑先起来，用户在那个瞬间点一次"立即签到"就能把补跑挤掉。
func (s *Service) RunWithBusyWait(parent context.Context, trigger, site string) (*Summary, error) {
	deadline := time.Now().Add(s.busyWaitBudget)
	for {
		// 每次尝试都现派生一份新的批次上下文：等待不该消耗批次的执行预算。
		attemptCtx, cancel := s.BatchContext(parent)
		summary, err := s.runner.Run(attemptCtx, RunOptions{Trigger: trigger, Site: site})
		cancel()
		if !errors.Is(err, ErrRunInProgress) {
			return summary, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w（已等待 %s 仍未拿到执行权，跳过本次）", ErrRunInProgress, s.busyWaitBudget)
		}
		s.log.Warn("已有批次在执行，稍后重试", "trigger", trigger, "retry_in", s.busyRetryInterval.String())
		if !sleepCtx(parent, s.busyRetryInterval) {
			return nil, fmt.Errorf("%w（等待期间进程要求退出）: %w", ErrRunInProgress, parent.Err())
		}
	}
}

// WaitIdle 等待正在执行的批次收尾（用于优雅关闭）。
//
// 批次结果要写库、要推送通知，进程直接退出会把它拦腰截断：轻则丢结果，重则留下半条记录。
func (s *Service) WaitIdle(ctx context.Context) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for s.runner.Running() {
		select {
		case <-ctx.Done():
			s.log.Warn("等待批次收尾超时，可能有结果未落库")
			return
		case <-ticker.C:
		}
	}
}
