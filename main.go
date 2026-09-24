// 命令 checkin 定时对多个 new-api 站点执行签到，结果落库并用 Bark 推送。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/oner8/newapi-checkin/internal/checkin"
	"github.com/oner8/newapi-checkin/internal/config"
	"github.com/oner8/newapi-checkin/internal/db"
	"github.com/oner8/newapi-checkin/internal/notify"
	"github.com/oner8/newapi-checkin/internal/scheduler"
	"github.com/oner8/newapi-checkin/internal/server"
	"github.com/oner8/newapi-checkin/internal/site"
)

// version 由构建时注入：-ldflags "-X main.version=x.y.z"。
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "错误: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("newapi-checkin", flag.ContinueOnError)
	var (
		configPath   = fs.String("config", "config.yaml", "配置文件路径")
		runOnce      = fs.Bool("run-once", false, "只执行一次签到后退出，适合 cron / GitHub Actions")
		probeOnly    = fs.Bool("probe", false, "只做站点与凭据诊断，不执行签到")
		siteName     = fs.String("site", "", "只处理指定名称的站点")
		dryRun       = fs.Bool("dry-run", false, "只探测不提交签到（覆盖配置里的 dry_run）")
		noServer     = fs.Bool("no-server", false, "不启动内置 HTTP 服务")
		levelFlag    = fs.String("log-level", "info", "日志级别: debug|info|warn|error")
		printVersion = fs.Bool("version", false, "打印版本后退出")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: newapi-checkin [选项]\n\n选项:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	logger := newLogger(*levelFlag)
	if *printVersion {
		fmt.Printf("newapi-checkin %s\n", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *dryRun {
		cfg.App.DryRun = true
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *probeOnly {
		return runProbe(ctx, cfg)
	}

	gdb, err := db.Open(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(gdb); err != nil {
			logger.Warn("关闭数据库失败", "error", err)
		}
	}()

	notifier := notify.NewMulti(logger, notify.NewBark(cfg.Notify.Bark, logger))
	if !notifier.Enabled() {
		logger.Warn("没有可用的通知渠道（检查 notify.bark.enabled 与 BARK_KEY），结果只写日志和数据库")
	}

	svc, err := checkin.NewService(gdb, cfg, notifier, logger)
	if err != nil {
		return err
	}

	printBanner(logger, cfg, svc, notifier, *dryRun)

	if *runOnce {
		return runOnceMode(ctx, svc, *siteName)
	}

	// HTTP 服务。
	var srv *server.Server
	if cfg.Server.Enabled && !*noServer {
		srv = server.New(server.Options{
			Addr:       cfg.Server.Addr,
			AdminToken: cfg.Server.AdminToken,
			Config:     cfg,
			DB:         gdb,
			Service:    svc,
			Notifier:   notifier,
			Version:    version,
			Log:        logger,
		})
		go func() {
			if err := srv.ListenAndServe(); err != nil {
				logger.Error("HTTP 服务退出", "error", err)
				stop()
			}
		}()
	} else {
		logger.Info("已按配置关闭内置 HTTP 服务")
	}

	// 启动补跑：容器重启后若当天还没跑过，立刻补一次。
	go func() {
		// 批处理上下文与进程信号解耦：SIGTERM 不该把正在进行的批次打断成假失败。
		batchCtx, cancel := svc.BatchContext(ctx)
		defer cancel()
		if _, err := svc.StartupCatchUp(batchCtx); err != nil {
			logger.Error("启动补跑失败", "error", err)
		}
	}()

	// 每日定时。
	loop := &scheduler.Loop{
		Schedule: svc.Schedule(),
		RunFunc: func(ctx context.Context) {
			// RunScheduled 内部会为每次尝试现派生一份批次上下文，
			// 这里必须传调度器自己的 ctx：否则等待会提前吃掉批次的执行预算。
			if _, err := svc.RunScheduled(ctx); err != nil {
				logger.Error("定时签到批次失败", "error", err)
			}
		},
		OnWait: svc.SetNextRun,
		Log:    logger,
	}
	go loop.Run(ctx)

	<-ctx.Done()
	logger.Info("收到退出信号，正在关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if srv != nil {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("HTTP 服务关闭异常", "error", err)
		}
	}
	// 等在跑的批次收尾：它的结果还要落库与推送，直接退出会把它截断。
	idleCtx, idleCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer idleCancel()
	svc.WaitIdle(idleCtx)
	return nil
}

// runOnceMode 跑一次并打印结果，失败时返回错误（进程退出码 1，便于 cron 报警）。
func runOnceMode(ctx context.Context, svc *checkin.Service, siteName string) error {
	summary, err := svc.RunOnce(ctx, checkin.TriggerManual, siteName)
	if err != nil {
		return err
	}
	printSummary(os.Stdout, summary)
	if summary.Failed() > 0 {
		return fmt.Errorf("有 %d 个站点签到失败，详见上方输出", summary.Failed())
	}
	return nil
}

// runProbe 打印每个站点的诊断结果；有站点没有可用凭据时返回错误。
func runProbe(ctx context.Context, cfg *config.Config) error {
	sites := cfg.EnabledSites()
	if len(sites) == 0 {
		return errors.New("没有启用的站点，检查 sites[].enabled")
	}

	usable := 0
	for i, s := range sites {
		if i > 0 {
			fmt.Println(strings.Repeat("-", 60))
		}
		result := site.Probe(ctx, s, cfg.App)
		fmt.Print(result.Describe())
		if result.Recommended != "" {
			usable++
		}
	}
	fmt.Printf("\n共 %d 个站点，%d 个有可用凭据。\n", len(sites), usable)
	if usable < len(sites) {
		return fmt.Errorf("%d 个站点没有可用凭据", len(sites)-usable)
	}
	return nil
}

// printSummary 打印批次的站点列表。
func printSummary(w *os.File, summary *checkin.Summary) {
	fmt.Fprintf(w, "\n批次 %s（%s）: 共 %d 个站点，成功 %d，失败 %d，耗时 %s\n",
		summary.RunID, summary.Trigger, summary.Total(), summary.Succeeded(), summary.Failed(),
		summary.Duration().Round(time.Second))
	fmt.Fprintf(w, "%-20s %-16s %-12s %s\n", "站点", "状态", "获得额度", "说明")
	for _, r := range summary.Results {
		fmt.Fprintf(w, "%-20s %-16s %-12d %s\n",
			r.SiteName, string(r.Status), r.QuotaAwarded, r.Message)
	}
}

// printBanner 打印启动信息，方便一眼确认配置是否如预期。
func printBanner(logger *slog.Logger, cfg *config.Config, svc *checkin.Service, notifier *notify.Multi, dryRun bool) {
	sites := cfg.EnabledSites()
	names := make([]string, 0, len(sites))
	for _, s := range sites {
		names = append(names, s.Name)
	}
	logger.Info("newapi-checkin 启动",
		"version", version,
		"schedule", svc.Schedule().String(),
		"timezone", cfg.App.Timezone,
		"sites", strings.Join(names, ", "),
		"database", cfg.Database.Path,
		"dry_run", cfg.App.DryRun,
		"jitter_seconds", cfg.App.JitterSeconds,
		"notify", strings.Join(notifier.Names(), ","),
		"next_run", svc.Schedule().Next(time.Now()).Format(time.RFC3339),
	)
	if dryRun {
		logger.Warn("dry-run 已开启：只探测站点状态，不会真正提交签到")
	}
}

// newLogger 构造文本日志器。
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
