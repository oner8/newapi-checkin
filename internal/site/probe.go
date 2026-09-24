package site

import (
	"context"
	"fmt"
	"strings"

	"newapi-checkin/internal/config"
)

// 凭据组合的标签。
const (
	ComboCookie           = "cookie"
	ComboAccessToken      = "access_token"
	ComboTokenWithUserID  = "access_token+New-Api-User"
	ComboCookieWithUserID = "cookie+New-Api-User"
)

// ComboResult 是一种凭据组合的探测结果。
type ComboResult struct {
	Label    string
	OK       bool
	Username string
	UserID   int64
	Quota    int64
	Err      string
}

// ProbeResult 是单个站点的诊断结果。
type ProbeResult struct {
	SiteName string
	BaseURL  string
	// CheckinPath / CheckinStatus 回显该站点生效的签到接口设置，方便确认 fork 的配置有没有被读到。
	CheckinPath   string
	CheckinStatus string
	// Proxy 回显该站点生效的代理（凭据已打码）；为空表示未单独配置、沿用环境变量。
	Proxy       string
	Reachable   bool
	Status      *Status
	StatusErr   string
	Combos      []ComboResult
	Recommended string
}

// Probe 依次尝试多种凭据组合，找出哪一种能通过 /api/user/self。
//
// 存在的意义：不同版本/fork 的 new-api 鉴权方式不同（Cookie、访问令牌、访问令牌+用户 ID 头），
// 无法离线确定目标站属于哪一种；与其看日志猜，不如让程序把每种组合都试一遍并直接报告结论。
func Probe(ctx context.Context, s config.Site, app config.App) ProbeResult {
	result := ProbeResult{
		SiteName:      s.Name,
		BaseURL:       s.BaseURL,
		CheckinPath:   s.CheckinPath,
		CheckinStatus: s.CheckinStatus,
	}

	client, err := NewClientFromSite(s, app)
	if err != nil {
		result.StatusErr = err.Error()
		return result
	}
	result.Proxy = client.ProxyDescription()

	status, statusErr := client.Status(ctx)
	if statusErr == nil {
		result.Reachable = true
		result.Status = status
	} else {
		result.StatusErr = statusErr.Error()
		if status != nil {
			result.Status = status
			result.Reachable = true
		}
	}

	cred := NewCredential(s.Credential)
	for _, candidate := range probeCandidates(cred) {
		combo := ComboResult{Label: candidate.label}
		self, err := client.WithCredential(candidate.cred).Self(ctx)
		if err != nil {
			combo.Err = err.Error()
		} else {
			combo.OK = true
			combo.Username = self.Username
			combo.UserID = self.ID
			combo.Quota = self.Quota
		}
		result.Combos = append(result.Combos, combo)
		if combo.OK && result.Recommended == "" {
			result.Recommended = candidate.label
		}
	}
	return result
}

type probeCandidate struct {
	label string
	cred  Credential
}

// probeCandidates 生成要尝试的凭据组合：只尝试手头确实有材料的组合。
func probeCandidates(cred Credential) []probeCandidate {
	var out []probeCandidate
	hasCookie := strings.TrimSpace(cred.Cookie) != ""
	hasToken := strings.TrimSpace(cred.Token) != ""
	userID := strings.TrimSpace(cred.NewAPIUser)

	if hasCookie {
		out = append(out, probeCandidate{
			label: ComboCookie,
			cred:  Credential{Type: config.CredentialCookie, Cookie: cred.Cookie},
		})
		if userID != "" {
			out = append(out, probeCandidate{
				label: ComboCookieWithUserID,
				cred:  Credential{Type: config.CredentialCookie, Cookie: cred.Cookie, NewAPIUser: userID},
			})
		}
	}
	if hasToken {
		out = append(out, probeCandidate{
			label: ComboAccessToken,
			cred:  Credential{Type: config.CredentialAccessToken, Token: cred.Token},
		})
		out = append(out, probeCandidate{
			label: ComboTokenWithUserID,
			// 用户 ID 未知时用 "1" 探测：老版本只校验"头是否存在且非空"，
			// 用 1 足以区分"缺这个头"与"头的内容不对"两种情况。
			cred: Credential{Type: config.CredentialAccessToken, Token: cred.Token, NewAPIUser: firstNonEmpty(userID, "1")},
		})
	}
	return out
}

// Describe 返回一行可读的探测结论，供 CLI 打印。
func (r ProbeResult) Describe() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "站点: %s\n  base_url: %s\n", r.SiteName, r.BaseURL)
	if r.CheckinPath != "" {
		mode := r.CheckinStatus
		if mode == "" {
			mode = config.CheckinStatusAuto
		}
		note := ""
		if r.CheckinPath != config.DefaultCheckinPath {
			note = "（非默认路径，fork 改过签到接口）"
		}
		fmt.Fprintf(&sb, "  签到接口: POST %s%s   状态预查: %s\n", r.CheckinPath, note, mode)
	}
	if r.Status != nil {
		version := r.Status.Version
		if version == "" {
			version = "未知"
		}
		systemName := r.Status.SystemName
		if systemName == "" {
			systemName = "未提供"
		}
		fmt.Fprintf(&sb, "  new-api 版本: %s（站点名: %s）\n", version, systemName)
		fmt.Fprintf(&sb, "  签到功能: %s   Turnstile 验证码: %s\n",
			checkinText(r.Status), enabledText(r.Status.TurnstileCheck))
		if r.Status.TurnstileCheck {
			sb.WriteString("  注意: 站点开启了 Turnstile，纯脚本无法自动通过验证码，务必用长期有效的 Cookie/令牌\n")
		}
	}
	if r.Proxy != "" {
		fmt.Fprintf(&sb, "  代理: %s\n", r.Proxy)
	}
	if r.StatusErr != "" {
		fmt.Fprintf(&sb, "  /api/status 探测失败: %s\n", r.StatusErr)
	}
	sb.WriteString("  凭据探测:\n")
	if len(r.Combos) == 0 {
		sb.WriteString("    （配置里没有可试的凭据）\n")
	}
	for _, c := range r.Combos {
		if c.OK {
			fmt.Fprintf(&sb, "    ✓ %-28s 用户=%s(id=%d) 余额=%d\n", c.Label, c.Username, c.UserID, c.Quota)
		} else {
			fmt.Fprintf(&sb, "    ✗ %-28s %s\n", c.Label, c.Err)
		}
	}
	if r.Recommended != "" {
		fmt.Fprintf(&sb, "  建议: credential.type=%s\n", recommendedType(r.Recommended))
	} else {
		sb.WriteString("  建议: 没有可用凭据，请重新复制 Cookie 或改用访问令牌\n")
	}
	return sb.String()
}

func recommendedType(label string) string {
	if strings.HasPrefix(label, ComboAccessToken) {
		return string(config.CredentialAccessToken)
	}
	return string(config.CredentialCookie)
}

// checkinText 描述签到开关：站点没声明该字段时要如实说明，而不是直接报「未开启」。
func checkinText(s *Status) string {
	if s.CheckinSupportUnknown {
		return "未声明（按开启处理）"
	}
	return enabledText(s.CheckinEnabled)
}

func enabledText(v bool) string {
	if v {
		return "已开启"
	}
	return "未开启"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
