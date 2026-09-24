// Package server 提供健康检查与手动触发的 HTTP 接口。
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"newapi-checkin/internal/checkin"
	"newapi-checkin/internal/config"
	"newapi-checkin/internal/model"
	"newapi-checkin/internal/notify"
	"newapi-checkin/internal/scheduler"
	"newapi-checkin/internal/site"
)

// Service 是 HTTP 层依赖的服务能力（由 checkin.Service 实现）。
type Service interface {
	Trigger(ctx context.Context, siteName string) (string, error)
	NextRun() time.Time
	Running() bool
	LastRun() *checkin.Summary
	Schedule() scheduler.Schedule
}

// Options 构造 HTTP 服务所需参数。
type Options struct {
	Addr       string
	AdminToken string
	Config     *config.Config
	DB         *gorm.DB
	Service    Service
	Notifier   *notify.Multi
	Version    string
	StartedAt  time.Time
	Log        *slog.Logger
}

// Server 包装 gin 引擎与 http.Server。
type Server struct {
	opts  Options
	http  *http.Server
	start time.Time
}

// New 构造服务。
func New(opts Options) *Server {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.StartedAt.IsZero() {
		opts.StartedAt = time.Now()
	}
	gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	engine.Use(gin.Recovery())
	// 不信任任何代理头，保证 ClientIP() 反映真实来源，供 admin_token 为空时的本机判断使用。
	_ = engine.SetTrustedProxies(nil)

	s := &Server{opts: opts, start: opts.StartedAt}
	engine.GET("/healthz", s.healthz)
	engine.GET("/", s.index)

	api := engine.Group("/api")
	api.GET("/sites", s.listSites)
	api.GET("/runs", s.listRuns)
	api.POST("/run", s.adminGuard(), s.triggerRun)

	s.http = &http.Server{
		Addr:              opts.Addr,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Handler 返回底层 http.Handler（测试用）。
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Addr 返回监听地址。
func (s *Server) Addr() string { return s.http.Addr }

// ListenAndServe 启动服务并阻塞，直到出错或被 Shutdown。
func (s *Server) ListenAndServe() error {
	s.opts.Log.Info("HTTP 服务已启动", "addr", s.addrForLog())
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 优雅关闭。
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) addrForLog() string {
	addr := s.http.Addr
	if addr == "" {
		return ":http"
	}
	return addr
}

// healthz 供 Docker healthcheck 使用，任何情况下都应快速返回。
func (s *Server) healthz(c *gin.Context) {
	next := s.opts.Service.NextRun()
	payload := gin.H{
		"status":         "ok",
		"version":        s.opts.Version,
		"time":           time.Now().Format(time.RFC3339),
		"uptime_seconds": int64(time.Since(s.start).Seconds()),
		"running":        s.opts.Service.Running(),
		"sites":          len(s.opts.Config.EnabledSites()),
		"schedule":       s.opts.Service.Schedule().String(),
		"timezone":       s.opts.Config.App.Timezone,
		"dry_run":        s.opts.Config.App.DryRun,
	}
	if !next.IsZero() {
		payload["next_run"] = next.Format(time.RFC3339)
	}
	if s.opts.Notifier != nil {
		payload["notify_channels"] = s.opts.Notifier.Names()
	}
	if last := s.opts.Service.LastRun(); last != nil {
		payload["last_run"] = gin.H{
			"run_id":        last.RunID,
			"trigger":       last.Trigger,
			"started_at":    last.StartedAt.Format(time.RFC3339),
			"succeeded":     last.Succeeded(),
			"failed":        last.Failed(),
			"quota_awarded": last.QuotaAwarded(),
		}
	}
	c.JSON(http.StatusOK, payload)
}

// index 提供一份简短的接口索引，方便浏览器直接打开确认服务在跑。
func (s *Server) index(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"service": "newapi-checkin",
		"version": s.opts.Version,
		"endpoints": []string{
			"GET /healthz",
			"GET /api/sites",
			"GET /api/runs?limit=20",
			"POST /api/run （需 X-Admin-Token，可加 ?site=站点名）",
		},
	})
}

// listSites 返回每个站点的配置概要与最近一次签到结果。
func (s *Server) listSites(c *gin.Context) {
	latest := map[string]model.CheckinRecord{}
	if s.opts.DB != nil {
		records, err := model.LatestCheckinPerSite(s.opts.DB)
		if err != nil {
			s.opts.Log.Warn("读取最近签到记录失败", "error", err)
		}
		for _, r := range records {
			latest[r.SiteName] = r
		}
	}

	type lastView struct {
		CheckinDate  string `json:"checkin_date"`
		Status       string `json:"status"`
		Message      string `json:"message"`
		QuotaAwarded int64  `json:"quota_awarded"`
		Attempts     int    `json:"attempts"`
		UpdatedAt    string `json:"updated_at"`
	}
	type siteView struct {
		Name       string          `json:"name"`
		BaseURL    string          `json:"base_url"`
		Enabled    bool            `json:"enabled"`
		Credential site.PublicInfo `json:"credential"`
		Last       *lastView       `json:"last,omitempty"`
	}

	views := make([]siteView, 0, len(s.opts.Config.Sites))
	for _, siteCfg := range s.opts.Config.Sites {
		view := siteView{
			Name:       siteCfg.Name,
			BaseURL:    siteCfg.BaseURL,
			Enabled:    siteCfg.Enabled,
			Credential: site.NewCredential(siteCfg.Credential).PublicInfo(),
		}
		if rec, ok := latest[siteCfg.Name]; ok {
			view.Last = &lastView{
				CheckinDate:  rec.CheckinDate,
				Status:       rec.Status,
				Message:      rec.Message,
				QuotaAwarded: rec.QuotaAwarded,
				Attempts:     rec.Attempts,
				UpdatedAt:    rec.UpdatedAt.Format(time.RFC3339),
			}
		}
		views = append(views, view)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"sites":      views,
			"running":    s.opts.Service.Running(),
			"next_run":   nextRunString(s.opts.Service),
			"dry_run":    s.opts.Config.App.DryRun,
			"updated_at": time.Now().Format(time.RFC3339),
		},
	})
}

// listRuns 返回最近的批次记录。
func (s *Server) listRuns(c *gin.Context) {
	limit := 20
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 200 {
			limit = parsed
		}
	}
	if s.opts.DB == nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"runs": []any{}}})
		return
	}
	runs, err := model.RecentRunLogs(s.opts.DB, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"runs": runs}})
}

// triggerRun 异步触发一次签到，立即返回批次号。
func (s *Server) triggerRun(c *gin.Context) {
	siteName := c.Query("site")
	runID, err := s.opts.Service.Trigger(c.Request.Context(), siteName)
	switch {
	case errors.Is(err, checkin.ErrRunInProgress):
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": err.Error()})
		return
	case errors.Is(err, checkin.ErrSiteNotFound):
		c.JSON(http.StatusNotFound, gin.H{"success": false, "message": err.Error()})
		return
	case err != nil:
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"message": "已开始签到，结果稍后写入数据库并推送通知",
		"data":    gin.H{"run_id": runID, "site": siteName},
	})
}

// adminGuard 保护写接口：配置了 admin_token 就校验它，否则只允许本机访问。
func (s *Server) adminGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.opts.AdminToken == "" {
			if ip := net.ParseIP(c.ClientIP()); ip != nil && ip.IsLoopback() {
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"success": false,
				"message": "未配置 server.admin_token，且请求不是来自本机，已拒绝",
			})
			return
		}
		got := c.GetHeader("X-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.opts.AdminToken)) == 1 {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"message": "X-Admin-Token 不正确",
		})
	}
}

func nextRunString(svc Service) string {
	next := svc.NextRun()
	if next.IsZero() {
		return ""
	}
	return next.Format(time.RFC3339)
}
