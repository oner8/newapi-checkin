package model

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// upsertClause 按 (site_name, checkin_date) 冲突时更新已有行。
//
// 这样同一天的手动重跑、失败重试、启动补跑都只会更新同一条记录，
// 不会在 checkin_records 里堆出多行。
func upsertClause(rec *CheckinRecord) clause.OnConflict {
	_ = rec
	return clause.OnConflict{
		Columns: []clause.Column{{Name: "site_name"}, {Name: "checkin_date"}},
		DoUpdates: clause.Assignments(map[string]any{
			// 一行代表"当天的最佳结果"，两条规则：
			//   - status 粘性：当天只要签到成功过，这一行的状态就固定为 success。
			//     后续的失败（站点抖动、凭据临时失效）或"今日已签到"都不再改写它，
			//     否则晚上的手动重跑会把早上的成果改写成"没做事"或"失败了"。
			//   - quota_awarded 取历史最大值：站点在"今日已签到"时不会返回额度（为 0），
			//     若直接覆盖就会把当天真实拿到的额度抹掉。
			// 注意 MAX(a,b) 是 SQLite 的标量最大值函数，本项目只面向 SQLite。
			"status": gorm.Expr(
				"CASE WHEN checkin_records.status = 'success' " +
					"THEN checkin_records.status ELSE excluded.status END"),
			"quota_awarded":  gorm.Expr("MAX(checkin_records.quota_awarded, excluded.quota_awarded)"),
			"message":        clause.Column{Table: "excluded", Name: "message"},
			"quota_before":   clause.Column{Table: "excluded", Name: "quota_before"},
			"quota_after":    clause.Column{Table: "excluded", Name: "quota_after"},
			"quota_per_unit": clause.Column{Table: "excluded", Name: "quota_per_unit"},
			"http_status":    clause.Column{Table: "excluded", Name: "http_status"},
			"latency_ms":     clause.Column{Table: "excluded", Name: "latency_ms"},
			"attempts":       clause.Column{Table: "excluded", Name: "attempts"},
			"run_id":         clause.Column{Table: "excluded", Name: "run_id"},
			"trigger":        clause.Column{Table: "excluded", Name: "trigger"},
			"updated_at":     clause.Column{Table: "excluded", Name: "updated_at"},
		}),
	}
}

// ClaimNotifyDedup 尝试占有某个去重键。
//
// 返回 true 表示本次是首次占有（可以推送），false 表示之前已经推过（应当跳过）。
// 依赖 notify_logs.dedup_key 的唯一索引，因此并发/重试都安全。
func ClaimNotifyDedup(db *gorm.DB, key string) (bool, error) {
	if db == nil || key == "" {
		return true, nil
	}
	rec := NotifyLog{DedupKey: key, CreatedAt: time.Now()}
	res := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&rec)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ReleaseNotifyDedup 释放去重键（例如推送本身失败时允许下次重试推送）。
func ReleaseNotifyDedup(db *gorm.DB, key string) error {
	if db == nil || key == "" {
		return nil
	}
	return db.Where("dedup_key = ?", key).Delete(&NotifyLog{}).Error
}
