// Package notify 负责把批次结果推送给用户（当前实现 Bark）。
package notify

import (
	"context"
	"errors"
	"log/slog"
)

// Message 是一条待推送的通知。
type Message struct {
	Title string
	Body  string
	Group string
	Level string
	Icon  string
	Sound string
}

// Notifier 是通知渠道。
type Notifier interface {
	// Name 渠道名，用于日志。
	Name() string
	// Enabled 未配置或未启用时返回 false，调用方可以跳过。
	Enabled() bool
	// Send 推送一条消息。
	Send(ctx context.Context, msg Message) error
}

// Multi 把消息扇出到多个渠道。
//
// 去重不在这里做：批次的失败去重依赖数据库，放在 runner 里更内聚。
type Multi struct {
	notifiers []Notifier
	log       *slog.Logger
}

// NewMulti 构造扇出器。
func NewMulti(log *slog.Logger, notifiers ...Notifier) *Multi {
	if log == nil {
		log = slog.Default()
	}
	return &Multi{notifiers: notifiers, log: log}
}

// Enabled 表示至少有一个可用渠道。
func (m *Multi) Enabled() bool {
	for _, n := range m.notifiers {
		if n.Enabled() {
			return true
		}
	}
	return false
}

// Names 返回已启用渠道名。
func (m *Multi) Names() []string {
	// 用非 nil 的空切片：/healthz 里应当显示 [] 而不是 null。
	names := []string{}
	for _, n := range m.notifiers {
		if n.Enabled() {
			names = append(names, n.Name())
		}
	}
	return names
}

// Notify 推送到所有已启用渠道，返回聚合错误。
func (m *Multi) Notify(ctx context.Context, msg Message) error {
	var (
		errs     []error
		attempts int
	)
	for _, n := range m.notifiers {
		if !n.Enabled() {
			continue
		}
		attempts++
		if err := n.Send(ctx, msg); err != nil {
			m.log.Warn("通知推送失败", "channel", n.Name(), "error", err)
			errs = append(errs, err)
			continue
		}
		m.log.Info("通知已推送", "channel", n.Name(), "title", msg.Title)
	}
	if attempts == 0 {
		m.log.Debug("没有可用的通知渠道，仅输出日志")
		return nil
	}
	return errors.Join(errs...)
}
