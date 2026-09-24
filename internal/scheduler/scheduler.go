// Package scheduler 提供每日定时与确定性抖动。
//
// 刻意不引入 cron 库：需求只是"每天在某个 HH:MM 跑一次"，用 time.Timer 计算下一次
// 触发点即可，少一个依赖、逻辑也更容易测。
package scheduler

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"
)

// Schedule 描述每日触发时间与每站抖动窗口。
type Schedule struct {
	Hour   int
	Minute int
	// Jitter 是每站每日的最大随机推迟时间，0 表示不抖动。
	Jitter time.Duration
	Loc    *time.Location
}

// New 构造 Schedule，loc 为 nil 时用 UTC。
func New(hour, minute int, jitter time.Duration, loc *time.Location) Schedule {
	if loc == nil {
		loc = time.UTC
	}
	return Schedule{Hour: hour, Minute: minute, Jitter: jitter, Loc: loc}
}

// String 返回 HH:MM 形式。
func (s Schedule) String() string {
	return fmt.Sprintf("%02d:%02d", s.Hour, s.Minute)
}

// Next 返回 from 之后（严格大于 from）的下一个触发时间点，按 Schedule 的时区计算。
func (s Schedule) Next(from time.Time) time.Time {
	local := from.In(s.Loc)
	candidate := time.Date(local.Year(), local.Month(), local.Day(), s.Hour, s.Minute, 0, 0, s.Loc)
	if !candidate.After(local) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}

// JitterFor 返回某个站点在某天的确定性推迟时间。
//
// 使用 站点名+日期 做哈希，所以同一天同一站点反复计算得到相同结果：
// 调度重算、进程重启都不会让站点"再抖一次"，避免签到时间飘忽不定。
func (s Schedule) JitterFor(key string, day time.Time) time.Duration {
	if s.Jitter <= 0 || key == "" {
		return 0
	}
	seconds := int64(s.Jitter / time.Second)
	if seconds <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(day.In(s.Loc).Format("2006-01-02")))
	return time.Duration(h.Sum64()%uint64(seconds)) * time.Second
}

// TriggerAt 返回某站点下一次应当执行的时间（触发点 + 该站抖动）。
//
// 抖动会被夹在下一个触发点之前，防止推迟跨天导致漏签。
func (s Schedule) TriggerAt(key string, now time.Time) time.Time {
	base := s.Next(now)
	at := base.Add(s.JitterFor(key, base))
	if next := base.AddDate(0, 0, 1); !at.Before(next) {
		at = next.Add(-time.Second)
	}
	return at
}

// Loop 是按 Schedule 反复触发的循环。
type Loop struct {
	Schedule Schedule
	// Key 用于抖动哈希；批次级别的调度一般传空字符串（不做批次抖动）。
	Key string
	// Run 是到点后执行的回调，应当尊重 ctx 取消。
	RunFunc func(ctx context.Context)
	// OnWait 每次算出下一次触发点时回调，便于日志与 /healthz 展示。
	OnWait func(next time.Time)
	Log    *slog.Logger
	// Now 允许注入时间（测试用），默认 time.Now。
	Now func() time.Time
}

// Run 阻塞运行直到 ctx 被取消。
func (l *Loop) Run(ctx context.Context) {
	now := l.Now
	if now == nil {
		now = time.Now
	}
	log := l.Log
	if log == nil {
		log = slog.Default()
	}

	for {
		if ctx.Err() != nil {
			return
		}
		next := l.Schedule.TriggerAt(l.Key, now())
		// 用注入的时钟计算等待时长：若写成 time.Until(next)（墙钟），注入固定时钟时
		// wait 会恒为负，Loop 就会在无 sleep 的紧循环里反复触发批次。
		wait := next.Sub(now())
		if l.OnWait != nil {
			l.OnWait(next)
		}
		if wait <= 0 {
			log.Info("到达触发时间，立即执行", "at", next.Format(time.RFC3339))
			l.RunFunc(ctx)
			continue
		}
		log.Info("等待下一次签到", "next", next.Format(time.RFC3339), "in", wait.Round(time.Second).String())

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			log.Info("到达触发时间", "at", next.Format(time.RFC3339))
			l.RunFunc(ctx)
		}
	}
}
