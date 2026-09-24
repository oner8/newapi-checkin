package checkin

import (
	"context"
	"testing"
	"time"

	"github.com/oner8/newapi-checkin/internal/notify"
)

// TestBatchContextIsDecoupledFromSignals 验证后台批次使用的上下文与进程信号解耦、且有上界。
//
// 解耦的意义：容器收到 SIGTERM 时，正在跑的批次不该被"取消"成失败结果写进数据库
// （那会把当天其实已经成功的结果覆盖掉，还会让启动补跑误以为今天已经跑过）。
func TestBatchContextIsDecoupledFromSignals(t *testing.T) {
	cfg := testConfig(t, "https://example.invalid")
	_, gdb := newTestRunner(t, cfg, nil)

	svc, err := NewService(gdb, cfg, notify.NewMulti(testLogger()), testLogger())
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}

	parent, cancelParent := context.WithCancel(context.Background())
	batch, cancelBatch := svc.BatchContext(parent)
	defer cancelBatch()

	// 模拟收到 SIGTERM。
	cancelParent()
	if err := batch.Err(); err != nil {
		t.Fatalf("批处理上下文不应随父上下文一起取消，实际 %v", err)
	}

	deadline, ok := batch.Deadline()
	if !ok {
		t.Fatal("批处理上下文应当有截止时间，避免卡住的批次永久占用执行权")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > maxBatchDuration {
		t.Errorf("截止时间不合理：剩余 %v，上限 %v", remaining, maxBatchDuration)
	}

	// 但批次自身仍必须可被取消（超时保护）。
	cancelBatch()
	if batch.Err() == nil {
		t.Error("显式取消批次后，批处理上下文应当已经结束")
	}
}
