package scheduler

import (
	"testing"
	"time"
)

// testLoc 是固定东八区，避免依赖宿主机的本地时区。
var testLoc = time.FixedZone("CST", 8*3600)

// jitterKeys 是一组固定的站点名，用于构造可区分抖动的测试数据。
var jitterKeys = []string{
	"site-a", "site-b", "site-c", "site-d", "site-e",
	"site-f", "site-g", "site-h", "site-i", "site-j",
	"站点-甲", "站点-乙", "站点-丙", "站点-丁", "站点-戊",
	"alpha", "beta", "gamma", "delta", "epsilon",
}

func TestScheduleString(t *testing.T) {
	cases := []struct {
		name string
		s    Schedule
		want string
	}{
		{"补零", New(8, 5, 0, testLoc), "08:05"},
		{"午夜", New(0, 0, 0, testLoc), "00:00"},
		{"两位数", New(23, 59, 0, testLoc), "23:59"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.String(); got != tc.want {
				t.Fatalf("String() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestNewNilLocationUsesUTC(t *testing.T) {
	s := New(8, 0, 0, nil)
	if s.Loc != time.UTC {
		t.Fatalf("New(..., nil).Loc = %v，期望 UTC", s.Loc)
	}
}

func TestNext(t *testing.T) {
	s := New(8, 30, 0, testLoc)
	cases := []struct {
		name string
		from time.Time
		want time.Time
	}{
		{
			"当天触发时刻之前返回当天",
			time.Date(2024, 3, 10, 7, 0, 0, 0, testLoc),
			time.Date(2024, 3, 10, 8, 30, 0, 0, testLoc),
		},
		{
			"正好等于触发时刻返回次日",
			time.Date(2024, 3, 10, 8, 30, 0, 0, testLoc),
			time.Date(2024, 3, 11, 8, 30, 0, 0, testLoc),
		},
		{
			"触发时刻之后返回次日",
			time.Date(2024, 3, 10, 9, 0, 0, 0, testLoc),
			time.Date(2024, 3, 11, 8, 30, 0, 0, testLoc),
		},
		{
			"跨月末",
			time.Date(2024, 3, 31, 23, 0, 0, 0, testLoc),
			time.Date(2024, 4, 1, 8, 30, 0, 0, testLoc),
		},
		{
			"差一秒到触发时刻",
			time.Date(2024, 3, 10, 8, 29, 59, 0, testLoc),
			time.Date(2024, 3, 10, 8, 30, 0, 0, testLoc),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := s.Next(tc.from)
			if !got.After(tc.from) {
				t.Fatalf("Next(%s) = %s，必须严格大于 from",
					tc.from.Format(time.RFC3339), got.Format(time.RFC3339))
			}
			y, m, d := got.Date()
			if y != tc.want.Year() || m != tc.want.Month() || d != tc.want.Day() {
				t.Fatalf("Next(%s) 日期 = %04d-%02d-%02d，期望 %04d-%02d-%02d",
					tc.from.Format(time.RFC3339), y, int(m), d,
					tc.want.Year(), int(tc.want.Month()), tc.want.Day())
			}
			if got.Hour() != tc.want.Hour() || got.Minute() != tc.want.Minute() {
				t.Fatalf("Next(%s) 时间 = %02d:%02d，期望 %02d:%02d",
					tc.from.Format(time.RFC3339), got.Hour(), got.Minute(),
					tc.want.Hour(), tc.want.Minute())
			}
			if !got.Equal(tc.want) {
				t.Fatalf("Next(%s) = %s，期望 %s",
					tc.from.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

func TestJitterFor(t *testing.T) {
	day := time.Date(2024, 3, 10, 8, 30, 0, 0, testLoc)

	t.Run("Jitter 为 0 时返回 0", func(t *testing.T) {
		s := New(8, 30, 0, testLoc)
		for _, key := range jitterKeys {
			if got := s.JitterFor(key, day); got != 0 {
				t.Fatalf("JitterFor(%q) = %s，Jitter=0 时期望 0", key, got)
			}
		}
	})

	t.Run("空 key 返回 0", func(t *testing.T) {
		s := New(8, 30, time.Hour, testLoc)
		if got := s.JitterFor("", day); got != 0 {
			t.Fatalf("JitterFor(\"\") = %s，期望 0", got)
		}
	})

	t.Run("同 key 同日稳定", func(t *testing.T) {
		s := New(8, 30, time.Hour, testLoc)
		for _, key := range jitterKeys {
			first := s.JitterFor(key, day)
			second := s.JitterFor(key, day)
			if first != second {
				t.Fatalf("JitterFor(%q) 两次调用不一致: %s vs %s", key, first, second)
			}
		}
	})

	t.Run("结果落在 [0, Jitter) 区间", func(t *testing.T) {
		jitter := time.Hour
		s := New(8, 30, jitter, testLoc)
		for _, key := range jitterKeys {
			got := s.JitterFor(key, day)
			if got < 0 {
				t.Fatalf("JitterFor(%q) = %s，不应为负", key, got)
			}
			if got >= jitter {
				t.Fatalf("JitterFor(%q) = %s，应严格小于 Jitter=%s", key, got, jitter)
			}
		}
	})

	t.Run("不同 key 至少产生两个不同值", func(t *testing.T) {
		s := New(8, 30, time.Hour, testLoc)
		distinct := map[time.Duration]struct{}{}
		for _, key := range jitterKeys {
			distinct[s.JitterFor(key, day)] = struct{}{}
		}
		if len(distinct) < 2 {
			t.Fatalf("不同 key 的抖动应至少有两种取值，实际只有 %d 种", len(distinct))
		}
	})

	t.Run("不同日期至少产生两个不同值", func(t *testing.T) {
		s := New(8, 30, time.Hour, testLoc)
		distinct := map[time.Duration]struct{}{}
		for i := 0; i < 5; i++ {
			d := day.AddDate(0, 0, i)
			distinct[s.JitterFor("site-a", d)] = struct{}{}
		}
		if len(distinct) < 2 {
			t.Fatalf("同一 key 不同日期的抖动应至少有两种取值，实际只有 %d 种", len(distinct))
		}
	})
}

func TestTriggerAt(t *testing.T) {
	now := time.Date(2024, 3, 10, 7, 0, 0, 0, testLoc)

	t.Run("Jitter=0 时等于 Next", func(t *testing.T) {
		s := New(8, 30, 0, testLoc)
		want := s.Next(now)
		for _, key := range jitterKeys {
			if got := s.TriggerAt(key, now); !got.Equal(want) {
				t.Fatalf("TriggerAt(%q) = %s，Jitter=0 时期望等于 Next=%s",
					key, got.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		}
	})

	t.Run("抖动不早于 Next 且严格早于次日", func(t *testing.T) {
		// 抖动取 48 小时，大于一天，用来覆盖“夹取到次日前一秒”的分支。
		s := New(8, 30, 48*time.Hour, testLoc)
		base := s.Next(now)
		next := base.AddDate(0, 0, 1)
		clamped := 0
		for _, key := range jitterKeys {
			at := s.TriggerAt(key, now)
			if at.Before(base) {
				t.Fatalf("TriggerAt(%q) = %s 早于 Next=%s",
					key, at.Format(time.RFC3339), base.Format(time.RFC3339))
			}
			if !at.Before(next) {
				t.Fatalf("TriggerAt(%q) = %s 不严格早于次日 %s（抖动跨天）",
					key, at.Format(time.RFC3339), next.Format(time.RFC3339))
			}
			if s.JitterFor(key, base) >= 24*time.Hour {
				clamped++
				if !at.Equal(next.Add(-time.Second)) {
					t.Fatalf("跨天夹取的 TriggerAt(%q) = %s，期望 %s",
						key, at.Format(time.RFC3339), next.Add(-time.Second).Format(time.RFC3339))
				}
			}
		}
		if clamped == 0 {
			t.Fatalf("测试数据未能覆盖跨天夹取分支，请调整 jitterKeys/jitter")
		}
	})
}
