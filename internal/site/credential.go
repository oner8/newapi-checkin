package site

import (
	"net/http"
	"strings"

	"github.com/oner8/newapi-checkin/internal/config"
)

// Credential 是实际发送给目标站的凭据。
type Credential struct {
	Type       config.CredentialType
	Cookie     string
	Token      string
	NewAPIUser string
}

// NewCredential 从配置构造凭据。
func NewCredential(c config.Credential) Credential {
	return Credential{
		Type:       c.Type,
		Cookie:     strings.TrimSpace(c.Cookie),
		Token:      strings.TrimSpace(c.Token),
		NewAPIUser: strings.TrimSpace(c.NewAPIUser),
	}
}

// Apply 把凭据写进请求头。
func (c Credential) Apply(h http.Header) {
	if c.Type == config.CredentialAccessToken {
		if c.Token != "" {
			h.Set("Authorization", "Bearer "+c.Token)
		}
	} else if c.Cookie != "" {
		h.Set("Cookie", NormalizeCookie(c.Cookie))
	}
	if c.NewAPIUser != "" {
		// 部分老版本 / fork 在使用访问令牌时要求带上用户 ID。
		h.Set("New-Api-User", c.NewAPIUser)
	}
}

// cookieNames 是常见 Cookie 键名，用于判断用户粘贴的是整段 Cookie 头还是裸的 session 值。
//
// 关键：只按"键名整体相等"匹配，绝不做子串匹配。new-api 的 session 值是 base64，
// 字母表天然可能包含 "csrf"、"new_api" 这类子串；用子串匹配会把裸值误判成 Cookie 头，
// 于是请求里根本没有 session，站点只会回"未登录"，用户再怎么重取 Cookie 都修不好。
var cookieNames = map[string]bool{
	"session":      true,
	"token":        true,
	"cf_clearance": true,
	"csrf_token":   true,
	"auth_token":   true,
	"access_token": true,
	"new_api_user": true,
	"new-api-user": true,
}

// NormalizeCookie 把用户输入归一化成可直接放进 Cookie 头的值。
//
// 支持三种输入：
//   - 整段 Cookie 头： "session=abc; other=1"      → 原样（规整分隔符）
//   - 带 "Cookie: " 前缀的粘贴内容                  → 去掉前缀
//   - 只有 session 的值："MTc4OQ%3D%3D"            → 补上 "session=" 前缀
//
// 值本身绝不做 URL 解码。
func NormalizeCookie(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if len(s) >= 7 && strings.EqualFold(s[:7], "cookie:") {
		s = strings.TrimSpace(s[7:])
	}
	// 把换行折叠成 "; " 而不是直接删掉：DevTools 复制的 Cookie 头可能分成多行，
	// 直接删除会把两个键粘成一个，用 "; " 才能保持语义（值里不允许出现换行）。
	s = strings.NewReplacer("\r\n", "; ", "\n", "; ", "\r", "; ", "\t", "").Replace(s)
	s = strings.TrimSpace(s)

	if strings.Contains(s, ";") || looksLikeCookieHeader(s) {
		return tidyCookieHeader(s)
	}
	return "session=" + s
}

// tidyCookieHeader 把 "a=1;b=2" 规整成 "a=1; b=2"。
func tidyCookieHeader(s string) string {
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "; ")
}

// CookieNames 返回 Cookie 头里出现的键名，用于日志与诊断（不含值）。
func (c Credential) CookieNames() []string {
	if c.Cookie == "" {
		return nil
	}
	var names []string
	if c.Type == config.CredentialAccessToken {
		return nil
	}
	for _, pair := range strings.Split(NormalizeCookie(c.Cookie), ";") {
		if idx := strings.Index(pair, "="); idx > 0 {
			names = append(names, strings.TrimSpace(pair[:idx]))
		}
	}
	return names
}

// Fingerprint 返回可安全打印的凭据指纹（只保留首尾各 4 个字符）。
func (c Credential) Fingerprint() string {
	var secret string
	switch c.Type {
	case config.CredentialAccessToken:
		secret = c.Token
	default:
		secret = sessionValue(c.Cookie)
	}
	return maskSecret(secret)
}

// Describe 返回形如 "cookie(session=abcd…wxyz)" 的描述，永不泄露完整值。
func (c Credential) Describe() string {
	if c.Type == config.CredentialAccessToken {
		return "access_token(" + c.Fingerprint() + ")"
	}
	names := c.CookieNames()
	label := "cookie"
	if len(names) > 0 {
		label += "[" + strings.Join(names, ",") + "]"
	}
	return label + "(" + c.Fingerprint() + ")"
}

// sessionValue 取出 session 键对应的值（找不到时返回整段值）。
func sessionValue(raw string) string {
	if raw == "" {
		return ""
	}
	for _, pair := range strings.Split(NormalizeCookie(raw), ";") {
		idx := strings.Index(pair, "=")
		if idx <= 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(pair[:idx]), "session") {
			return strings.TrimSpace(pair[idx+1:])
		}
	}
	trimmed := strings.TrimSpace(NormalizeCookie(raw))
	if idx := strings.Index(trimmed, "="); idx > 0 {
		return trimmed[idx+1:]
	}
	return trimmed
}

// PublicInfo 是可安全对外暴露的凭据信息。
//
// 与 Describe() 的区别：这里连掩码片段都不包含，因此可以放在未鉴权的 /api/sites 里。
type PublicInfo struct {
	Type        string   `json:"type"`
	CookieNames []string `json:"cookie_names,omitempty"`
	HasSecret   bool     `json:"has_secret"`
	NewAPIUser  bool     `json:"new_api_user,omitempty"`
}

// PublicInfo 返回不含任何密文片段的凭据信息。
func (c Credential) PublicInfo() PublicInfo {
	info := PublicInfo{
		Type:       string(c.Type),
		NewAPIUser: c.NewAPIUser != "",
	}
	if c.Type == config.CredentialAccessToken {
		info.HasSecret = c.Token != ""
		return info
	}
	info.HasSecret = c.Cookie != ""
	info.CookieNames = c.CookieNames()
	return info
}

// maskSecret 只保留首尾少量字符，且对短值全部打码。
//
// 暴露长度与密钥长度成比例：4 个字符的密钥不应该露出 8 个字符，因此露出位数取
// len/4（上限 4），长度不足 6 时完全不显示。
func maskSecret(secret string) string {
	if secret == "" {
		return "空"
	}
	r := []rune(secret)
	if len(r) <= 6 {
		return strings.Repeat("*", len(r))
	}
	n := len(r) / 4
	if n > 4 {
		n = 4
	}
	return string(r[:n]) + "…" + string(r[len(r)-n:])
}

// looksLikeCookieHeader 判断字符串是否已经是 "name=value" 形式的 Cookie 头片段。
func looksLikeCookieHeader(s string) bool {
	idx := strings.Index(s, "=")
	if idx <= 0 {
		return false
	}
	return cookieNames[strings.ToLower(strings.TrimSpace(s[:idx]))]
}
