// Package model 定义签到结果的数据表结构。
package model

import (
	"time"

	"gorm.io/gorm"
)

// Status 是一次签到尝试的最终状态。
type Status string

const (
	// StatusSuccess 签到成功，拿到了额度。
	StatusSuccess Status = "success"
	// StatusAlready 今日已签到，未重复提交。
	StatusAlready Status = "already"
	// StatusSkippedDisabled 目标站未开启签到功能。
	StatusSkippedDisabled Status = "skipped_disabled"
	// StatusAuthFailed 凭据失效（Cookie 过期或令牌无效）。
	StatusAuthFailed Status = "auth_failed"
	// StatusNeedTurnstile 目标站要求通过 Turnstile 验证码。
	StatusNeedTurnstile Status = "need_turnstile"
	// StatusNotNewAPI 打不开 /api/status，或响应不像 new-api。
	StatusNotNewAPI Status = "not_newapi"
	// StatusBlocked 请求被站点前置的 WAF/防护拦截（JS 挑战页/边缘拒绝），脚本无法通过。
	StatusBlocked Status = "blocked"
	// StatusDryRun 试运行，只探测未提交签到。
	StatusDryRun Status = "dry_run"
	// StatusError 其他错误（网络、5xx、响应无法解析等）。
	StatusError Status = "error"
)

// IsOK 表示该状态不应被当成失败（用于退出码与通知语气）。
func (s Status) IsOK() bool {
	switch s {
	case StatusSuccess, StatusAlready, StatusSkippedDisabled, StatusDryRun:
		return true
	default:
		return false
	}
}

// Label 返回中文短标签，用于通知与日志。
func (s Status) Label() string {
	switch s {
	case StatusSuccess:
		return "签到成功"
	case StatusAlready:
		return "今日已签到"
	case StatusSkippedDisabled:
		return "站点未开启签到"
	case StatusAuthFailed:
		return "凭据失效"
	case StatusNeedTurnstile:
		return "需过验证码"
	case StatusNotNewAPI:
		return "非 new-api 站点"
	case StatusBlocked:
		return "被防护拦截"
	case StatusDryRun:
		return "试运行未提交"
	default:
		return "失败"
	}
}

// CheckinRecord 记录某站点某一天的签到结果。
//
// (site_name, checkin_date) 唯一，重复执行同一天是覆盖更新而不是插入新行，
// 因此手动重跑或重试不会污染历史。
type CheckinRecord struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	SiteName     string    `gorm:"size:128;not null;uniqueIndex:idx_site_date" json:"site_name"`
	CheckinDate  string    `gorm:"size:10;not null;uniqueIndex:idx_site_date" json:"checkin_date"`
	Status       string    `gorm:"size:32;not null;index:idx_status" json:"status"`
	Message      string    `gorm:"size:512" json:"message"`
	QuotaAwarded int64     `json:"quota_awarded"`
	QuotaBefore  *int64    `json:"quota_before,omitempty"`
	QuotaAfter   *int64    `json:"quota_after,omitempty"`
	QuotaPerUnit float64   `json:"quota_per_unit"`
	HTTPStatus   int       `json:"http_status"`
	LatencyMS    int64     `json:"latency_ms"`
	Attempts     int       `json:"attempts"`
	RunID        string    `gorm:"size:64;index:idx_run_id" json:"run_id"`
	Trigger      string    `gorm:"size:16" json:"trigger"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TableName 固定表名。
func (CheckinRecord) TableName() string { return "checkin_records" }

// RunLog 记录一次批次执行（定时、启动补跑或手动触发）。
type RunLog struct {
	RunID      string    `gorm:"primaryKey;size:64" json:"run_id"`
	Trigger    string    `gorm:"size:16;index" json:"trigger"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Total      int       `json:"total"`
	Succeeded  int       `json:"succeeded"`
	Failed     int       `json:"failed"`
	Summary    string    `gorm:"size:1024" json:"summary"`
}

// TableName 固定表名。
func (RunLog) TableName() string { return "run_logs" }

// NotifyLog 用于通知去重：同一站点同一天同一原因只推一次。
type NotifyLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	DedupKey  string    `gorm:"size:255;not null;uniqueIndex:idx_dedup_key" json:"dedup_key"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName 固定表名。
func (NotifyLog) TableName() string { return "notify_logs" }

// UpsertCheckin 写入或更新一条签到记录。
func UpsertCheckin(db *gorm.DB, rec *CheckinRecord) error {
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	rec.UpdatedAt = time.Now()

	return db.Clauses(upsertClause(rec)).Create(rec).Error
}

// LatestCheckinPerSite 返回每个站点最近一次签到记录。
func LatestCheckinPerSite(db *gorm.DB) ([]CheckinRecord, error) {
	var records []CheckinRecord
	err := db.Raw(`SELECT * FROM checkin_records r
		WHERE r.id = (SELECT MAX(id) FROM checkin_records x WHERE x.site_name = r.site_name)`).
		Scan(&records).Error
	if err != nil {
		return nil, err
	}
	return records, nil
}

// LatestCheckinForSite 返回指定站点最近一次签到记录，不存在时返回 nil。
func LatestCheckinForSite(db *gorm.DB, siteName string) (*CheckinRecord, error) {
	var rec CheckinRecord
	err := db.Where("site_name = ?", siteName).Order("id DESC").First(&rec).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

// RecentRunLogs 返回最近的批次记录。
func RecentRunLogs(db *gorm.DB, limit int) ([]RunLog, error) {
	if limit <= 0 {
		limit = 20
	}
	var logs []RunLog
	err := db.Order("started_at DESC").Limit(limit).Find(&logs).Error
	return logs, err
}

// HasRunOnDate 判断某天（按给定位置）是否已经执行过任何批次。
func HasRunOnDate(db *gorm.DB, day time.Time, loc *time.Location) (bool, error) {
	start := time.Date(day.In(loc).Year(), day.In(loc).Month(), day.In(loc).Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 1)
	var count int64
	err := db.Model(&RunLog{}).
		Where("started_at >= ? AND started_at < ?", start, end).
		Count(&count).Error
	return count > 0, err
}

// FindCheckin 按 (站点, 日期) 查询签到记录，不存在时返回 nil。
func FindCheckin(db *gorm.DB, siteName, date string) (*CheckinRecord, error) {
	var rec CheckinRecord
	err := db.Where("site_name = ? AND checkin_date = ?", siteName, date).First(&rec).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}
