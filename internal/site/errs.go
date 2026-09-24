package site

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind 是错误分类。批次重试与通知语气都依赖它。
type Kind int

const (
	// KindNetwork 网络层错误（超时、连接失败、DNS），可重试。
	KindNetwork Kind = iota
	// KindServer 服务端错误（5xx / 429），可重试。
	KindServer
	// KindDecode 响应不是预期 JSON（例如被 WAF 拦成了 HTML），可重试一次。
	KindDecode
	// KindAuthFailed 凭据失效，重试无用。
	KindAuthFailed
	// KindNotNewAPI 目标站不像 new-api（或未开放 /api/status），重试无用。
	KindNotNewAPI
	// KindCheckinDisabled 目标站未开启签到功能。
	KindCheckinDisabled
	// KindAlreadyCheckedIn 今日已签到，属于正常结果。
	KindAlreadyCheckedIn
	// KindTurnstile 目标站要求通过 Turnstile 验证码，脚本无法自动完成。
	KindTurnstile
	// KindBusiness 其他业务层失败（success=false 且没有更具体的分类）。
	KindBusiness
	// KindBlocked 请求被站点前置的 WAF/防护拦截（JS 挑战页或边缘拒绝），重试无用：
	// 需要浏览器通行 Cookie（acw_sc__v2 / cf_clearance 等）并匹配 User-Agent。
	KindBlocked
)

// String 便于日志阅读。
func (k Kind) String() string {
	switch k {
	case KindNetwork:
		return "network"
	case KindServer:
		return "server"
	case KindDecode:
		return "decode"
	case KindAuthFailed:
		return "auth_failed"
	case KindNotNewAPI:
		return "not_newapi"
	case KindCheckinDisabled:
		return "checkin_disabled"
	case KindAlreadyCheckedIn:
		return "already_checked_in"
	case KindTurnstile:
		return "turnstile"
	case KindBusiness:
		return "business"
	case KindBlocked:
		return "blocked"
	default:
		return "unknown"
	}
}

// Error 携带分类信息的目标站错误。
type Error struct {
	Kind       Kind
	Op         string
	HTTPStatus int
	Message    string
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	var sb strings.Builder
	if e.Op != "" {
		sb.WriteString(e.Op)
		sb.WriteString(": ")
	}
	if e.Message != "" {
		sb.WriteString(e.Message)
	} else if e.Err != nil {
		sb.WriteString(e.Err.Error())
	} else {
		sb.WriteString(e.Kind.String())
	}
	if e.HTTPStatus > 0 {
		fmt.Fprintf(&sb, " (HTTP %d)", e.HTTPStatus)
	}
	if e.Err != nil && e.Message != "" {
		fmt.Fprintf(&sb, ": %v", e.Err)
	}
	return sb.String()
}

// Unwrap 支持 errors.Is / errors.As 穿透。
func (e *Error) Unwrap() error { return e.Err }

// Retryable 表示这个错误值得退避重试。
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindNetwork, KindServer, KindDecode:
		return true
	default:
		return false
	}
}

// NewError 构造一个分类错误。
func NewError(kind Kind, op, message string, err error) *Error {
	return &Error{Kind: kind, Op: op, Message: message, Err: err}
}

// AsError 把任意 error 提取为 *Error；不是分类错误时返回 nil。
func AsError(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return nil
}

// KindOf 返回错误的分类，未知错误一律视为可重试的网络类错误。
func KindOf(err error) Kind {
	if err == nil {
		return KindBusiness
	}
	if target := AsError(err); target != nil {
		return target.Kind
	}
	return KindNetwork
}

// IsKind 判断错误是否属于某一分类。
func IsKind(err error, kind Kind) bool {
	return err != nil && KindOf(err) == kind
}

// authHints 用于在没有结构化错误码时按文案兜底判断"凭据失效"。
//
// 只收指向明确的词：像 "session"、"auth" 这类过于宽泛的子串会误伤正常业务提示
// （例如文案里出现 session_id），因此不纳入匹配。
var authHints = []string{
	"未登录", "无权进行此操作", "登录已过期", "请先登录", "令牌无效", "无效的令牌",
	"unauthorized", "invalid token", "token 无效", "用户信息无效", "not logged in",
	"用户不存在", "认证失败", "authentication failed",
}

var turnstileHints = []string{"turnstile", "验证码", "人机验证", "challenge"}

var alreadyHints = []string{"今日已签到", "已经签到", "已签到"}

var disabledHints = []string{"签到功能未启用", "未开启签到", "签到未启用"}

// transientHints 是"临时性"提示：站点自己让我们稍后重试，说明这是可恢复的服务端抖动。
// 这类失败既值得退避重试，也不该被当成凭据问题 —— 否则会给用户"重新复制 Cookie"的
// 误导建议（例如站点返回「系统繁忙，请稍后重试」）。
var transientHints = []string{
	"稍后重试", "稍后再试", "请稍后", "系统繁忙", "繁忙", "超时", "timeout",
	"try again", "busy", "too many requests", "请求过于频繁", "频率限制",
}

func containsAnyFold(s string, hints []string) bool {
	lower := strings.ToLower(s)
	for _, h := range hints {
		if strings.Contains(lower, strings.ToLower(h)) {
			return true
		}
	}
	return false
}

// classifyMessage 依据站点返回的 message 做兜底分类。
func classifyMessage(message string) Kind {
	switch {
	case containsAnyFold(message, turnstileHints):
		return KindTurnstile
	case containsAnyFold(message, alreadyHints):
		return KindAlreadyCheckedIn
	case containsAnyFold(message, disabledHints):
		return KindCheckinDisabled
	case containsAnyFold(message, transientHints):
		return KindServer
	case containsAnyFold(message, authHints):
		return KindAuthFailed
	default:
		return KindBusiness
	}
}

// classifyHTTPStatus 依据 HTTP 状态码做分类，返回 (分类, 是否已确定)。
func classifyHTTPStatus(status int) (Kind, bool) {
	switch {
	case status == 401 || status == 403:
		return KindAuthFailed, true
	case status == 408 || status == 425:
		// 这两个 4xx 表示"服务端暂时没法处理"，属于可重试，而不是业务终态失败。
		return KindServer, true
	case status == 429:
		return KindServer, true
	case status >= 500:
		return KindServer, true
	case status >= 400:
		return KindBusiness, true
	default:
		return KindBusiness, false
	}
}

// Retryable 表示该错误值得退避重试（跨包调用入口，行为与 (*Error).Retryable 一致）。
func Retryable(err error) bool {
	if target := AsError(err); target != nil {
		return target.Retryable()
	}
	// 不是分类错误的普通 error（例如底层网络库错误）按可重试处理。
	return err != nil
}
