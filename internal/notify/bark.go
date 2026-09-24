package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"newapi-checkin/internal/config"
)

// maxBarkBody 限制推送正文长度，避免超出 Bark 的服务端限制。
const maxBarkBody = 1200

// barkResponse 是 Bark 的返回体。
type barkResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Bark 是 Bark（iOS）推送渠道。
type Bark struct {
	cfg     config.Bark
	httpCli *http.Client
	log     *slog.Logger
}

// NewBark 构造 Bark 渠道。未配置 key 时 Enabled 返回 false。
func NewBark(cfg config.Bark, log *slog.Logger) *Bark {
	if log == nil {
		log = slog.Default()
	}
	return &Bark{
		cfg:     cfg,
		httpCli: &http.Client{Timeout: 15 * time.Second},
		log:     log,
	}
}

// Name 返回渠道名。
func (b *Bark) Name() string { return "bark" }

// Enabled 判断 Bark 是否可用。
func (b *Bark) Enabled() bool {
	return b.cfg.Enabled &&
		strings.TrimSpace(b.cfg.Key) != "" &&
		strings.TrimSpace(b.cfg.Server) != ""
}

// Send 推送一条消息。
//
// 用 POST + JSON 而不是 GET 拼路径：正文含中文和换行时 GET 很容易触发 URL 长度限制。
func (b *Bark) Send(ctx context.Context, msg Message) error {
	if !b.Enabled() {
		return nil
	}

	level := strings.TrimSpace(msg.Level)
	if level == "" {
		level = b.cfg.Level
	}

	payload := map[string]any{
		"title":     msg.Title,
		"body":      truncate(msg.Body, maxBarkBody),
		"level":     level,
		"group":     firstNonEmpty(msg.Group, b.cfg.Group),
		"icon":      firstNonEmpty(msg.Icon, b.cfg.Icon),
		"sound":     firstNonEmpty(msg.Sound, b.cfg.Sound),
		"isArchive": 1,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("bark 请求体编码失败: %w", err)
	}

	endpoint := strings.TrimRight(b.cfg.Server, "/") + "/" + strings.Trim(b.cfg.Key, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("bark 请求构造失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := b.httpCli.Do(req)
	if err != nil {
		return fmt.Errorf("bark 请求失败: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if readErr != nil {
		return fmt.Errorf("bark 响应读取失败: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bark 返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if len(bytes.TrimSpace(raw)) == 0 {
		// 200 + 空响应体：部分自建 Bark / 反向代理会这样返回。当成成功，
		// 否则会误判为失败并释放通知去重键，导致同一条提醒被反复推送。
		return nil
	}
	var parsed barkResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// 200 但返回了非空且不是 JSON 的内容：多半被换成了 HTML 错误页。
		// 这种情况不能算推送成功，否则去重键会被永久占用、当天再也推不出去。
		return fmt.Errorf("bark 返回 200 但响应体不是 JSON: %s", truncate(strings.TrimSpace(string(raw)), 120))
	}
	if parsed.Code != 0 && parsed.Code != 200 {
		return fmt.Errorf("bark 返回 code=%d message=%s", parsed.Code, parsed.Message)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}
