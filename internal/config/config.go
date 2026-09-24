// Package config 负责加载 checkin 服务的 YAML 配置。
//
// 设计要点：
//   - 先填充默认值再 Unmarshal，因此配置文件只需要写想覆盖的字段；
//   - 支持 ${VAR} 与 ${VAR:-默认值} 环境变量展开，且只在结构体字符串字段上展开，
//     避免把密钥内容当成 YAML 语法解析（密钥里常带冒号、# 等字符）；
//   - ${VAR} 未定义时直接报错并指出是哪个字段，${VAR:-} 表示允许为空。
package config

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// CredentialType 是站点凭据类型。
type CredentialType string

const (
	// CredentialCookie 使用浏览器 Cookie（session=...），兼容绝大多数版本与 fork。
	CredentialCookie CredentialType = "cookie"
	// CredentialAccessToken 使用面板个人访问令牌（PAT），通过 Authorization: Bearer 传递。
	CredentialAccessToken CredentialType = "access_token"
)

// 签到接口路径与"签到状态预查"开关。
const (
	// DefaultCheckinPath 是 new-api 标准签到路径：查状态用 GET、提交用 POST。
	DefaultCheckinPath = "/api/user/checkin"
	// CheckinStatusAuto 表示用 checkin_path + ?month=YYYY-MM 预查当月状态，已签到就不重复提交。
	CheckinStatusAuto = "auto"
	// CheckinStatusOff 表示站点没有签到状态接口（例如只提供 POST /api/user/sign_in 的 fork），
	// 跳过预查直接提交。auto 模式下遇到"没有该接口"也会自动降级，见 runner.precheckStatus。
	CheckinStatusOff = "off"
)

// WAF 挑战的处理方式。
const (
	// WAFChallengeSolve 遇到"可离线求解"的前置防护挑战（目前是阿里云 WAF 的 acw_sc__v2）时，
	// 自动算出通行 Cookie 并重放一次请求。默认。
	WAFChallengeSolve = "solve"
	// WAFChallengeOff 不做求解，直接把这类响应判为 blocked（需要自己提供通行 Cookie）。
	WAFChallengeOff = "off"
)

// BarkMode 决定什么时候推送。
const (
	BarkModeAlways  = "always"
	BarkModeFailure = "failure"
	BarkModeSummary = "summary"
)

// Config 是配置文件根结构。
type Config struct {
	App      App      `yaml:"app"`
	Database Database `yaml:"database"`
	Server   Server   `yaml:"server"`
	Notify   Notify   `yaml:"notify"`
	Sites    []Site   `yaml:"sites"`
}

// App 是调度与执行相关配置。
type App struct {
	Timezone            string `yaml:"timezone"`
	Schedule            string `yaml:"schedule"`
	JitterSeconds       int    `yaml:"jitter_seconds"`
	RunOnStart          bool   `yaml:"run_on_start"`
	Concurrency         int    `yaml:"concurrency"`
	TimeoutSeconds      int    `yaml:"timeout_seconds"`
	Retries             int    `yaml:"retries"`
	RetryBackoffSeconds int    `yaml:"retry_backoff_seconds"`
	DryRun              bool   `yaml:"dry_run"`
	// WAFChallenge 是前置防护挑战的处理方式：solve（默认）| off。
	// solve 只处理"可离线求解"的挑战（阿里云 WAF 的 acw_sc__v2），其他厂商的挑战仍会报 blocked。
	WAFChallenge string `yaml:"waf_challenge"`
}

// Database 是结果落库配置。
type Database struct {
	Path string `yaml:"path"`
}

// Server 是内置 HTTP 服务配置。
type Server struct {
	Enabled    bool   `yaml:"enabled"`
	Addr       string `yaml:"addr"`
	AdminToken string `yaml:"admin_token"`
}

// Notify 是通知配置。
type Notify struct {
	Bark Bark `yaml:"bark"`
}

// Bark 是 Bark（iOS）推送配置。
type Bark struct {
	Enabled bool   `yaml:"enabled"`
	Server  string `yaml:"server"`
	Key     string `yaml:"key"`
	Group   string `yaml:"group"`
	Level   string `yaml:"level"`
	Icon    string `yaml:"icon"`
	Sound   string `yaml:"sound"`
	Mode    string `yaml:"mode"`
}

// Site 是单个目标站点配置。
type Site struct {
	Name       string            `yaml:"name"`
	BaseURL    string            `yaml:"base_url"`
	Enabled    bool              `yaml:"enabled"`
	Credential Credential        `yaml:"credential"`
	Headers    map[string]string `yaml:"headers"`
	// CheckinPath 是签到接口路径，默认 /api/user/checkin。部分 fork 改了路径
	// （例如某些 fork 用 /api/user/sign_in），查状态与提交都用它。
	CheckinPath string `yaml:"checkin_path"`
	// CheckinStatus 控制是否预查当月签到状态：auto（默认）| off。
	// 站点没有签到状态接口时写 off，可以省掉每天一次注定失败的查询请求。
	CheckinStatus string `yaml:"checkin_status"`
	// Proxy 是该站点专用的代理，留空则沿用环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）。
	// 支持 http:// 、https:// 、socks5://（可带凭据，例如 socks5://user:pass@127.0.0.1:1080）。
	Proxy string `yaml:"proxy"`
}

// Credential 是站点凭据。
type Credential struct {
	Type       CredentialType `yaml:"type"`
	Cookie     string         `yaml:"cookie"`
	Token      string         `yaml:"token"`
	NewAPIUser string         `yaml:"new_api_user"`
}

// Location 返回配置的时区。
func (a App) Location() (*time.Location, error) {
	return time.LoadLocation(a.Timezone)
}

// Timeout 返回单次 HTTP 请求超时。
func (a App) Timeout() time.Duration {
	return time.Duration(a.TimeoutSeconds) * time.Second
}

// RetryBackoff 返回首次重试前的等待时间。
func (a App) RetryBackoff() time.Duration {
	return time.Duration(a.RetryBackoffSeconds) * time.Second
}

// Jitter 返回每站每日的最大随机推迟时间。
func (a App) Jitter() time.Duration {
	return time.Duration(a.JitterSeconds) * time.Second
}

// EnabledSites 返回启用且按配置顺序排列的站点。
func (c *Config) EnabledSites() []Site {
	out := make([]Site, 0, len(c.Sites))
	for _, s := range c.Sites {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

// SiteByName 按名称查找站点。
func (c *Config) SiteByName(name string) (Site, bool) {
	for _, s := range c.Sites {
		if s.Name == name {
			return s, true
		}
	}
	return Site{}, false
}

// Default 返回默认配置。
func Default() *Config {
	return &Config{
		App: App{
			Timezone:            "Asia/Shanghai",
			Schedule:            "08:00",
			JitterSeconds:       600,
			RunOnStart:          true,
			Concurrency:         3,
			TimeoutSeconds:      20,
			Retries:             2,
			RetryBackoffSeconds: 60,
			WAFChallenge:        WAFChallengeSolve,
		},
		Database: Database{Path: "/data/newapi-checkin.db"},
		Server:   Server{Enabled: true, Addr: ":8080"},
		Notify: Notify{Bark: Bark{
			Enabled: true,
			Server:  "https://api.day.app",
			Group:   "签到",
			Level:   "active",
			Mode:    BarkModeSummary,
		}},
	}
}

// Load 读取并校验配置文件。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	if err := cfg.expandEnv(); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// expandEnv 展开所有字符串字段里的 ${VAR} / ${VAR:-默认值}。
func (c *Config) expandEnv() error {
	type target struct {
		field string
		value *string
	}
	targets := []target{
		{"app.timezone", &c.App.Timezone},
		{"app.schedule", &c.App.Schedule},
		{"database.path", &c.Database.Path},
		{"server.addr", &c.Server.Addr},
		{"server.admin_token", &c.Server.AdminToken},
		{"notify.bark.server", &c.Notify.Bark.Server},
		{"notify.bark.key", &c.Notify.Bark.Key},
		{"notify.bark.group", &c.Notify.Bark.Group},
		{"notify.bark.level", &c.Notify.Bark.Level},
		{"notify.bark.icon", &c.Notify.Bark.Icon},
		{"notify.bark.sound", &c.Notify.Bark.Sound},
		{"notify.bark.mode", &c.Notify.Bark.Mode},
	}
	for i := range c.Sites {
		s := &c.Sites[i]
		prefix := fmt.Sprintf("sites[%d](%s)", i, s.Name)
		if s.Name == "" {
			prefix = fmt.Sprintf("sites[%d]", i)
		}
		targets = append(targets,
			target{prefix + ".name", &s.Name},
			target{prefix + ".base_url", &s.BaseURL},
			target{prefix + ".credential.type", (*string)(&s.Credential.Type)},
			target{prefix + ".credential.cookie", &s.Credential.Cookie},
			target{prefix + ".credential.token", &s.Credential.Token},
			target{prefix + ".credential.new_api_user", &s.Credential.NewAPIUser},
			target{prefix + ".proxy", &s.Proxy},
		)
	}

	for _, t := range targets {
		expanded, err := expandString(*t.value)
		if err != nil {
			return fmt.Errorf("字段 %s: %w", t.field, err)
		}
		*t.value = expanded
	}

	for i := range c.Sites {
		s := &c.Sites[i]
		for k, v := range s.Headers {
			expanded, err := expandString(v)
			if err != nil {
				return fmt.Errorf("字段 sites[%d](%s).headers[%s]: %w", i, s.Name, k, err)
			}
			s.Headers[k] = expanded
		}
	}
	return nil
}

// expandString 展开单个字符串中的 ${VAR} 与 ${VAR:-默认值}。
//
// 变量未定义且没有默认值时返回错误；若设置了 <VAR>_FILE 环境变量则读取该文件内容
// （去除首尾空白），方便配合 Docker secret 使用。
func expandString(in string) (string, error) {
	if !strings.Contains(in, "${") {
		return in, nil
	}
	var (
		out    strings.Builder
		errOut error
	)
	for i := 0; i < len(in); {
		if in[i] != '$' || i+1 >= len(in) || in[i+1] != '{' {
			out.WriteByte(in[i])
			i++
			continue
		}
		end := strings.IndexByte(in[i+2:], '}')
		if end < 0 {
			// 没找到右花括号。若 ${ 后面看起来像个变量名，几乎肯定是漏写了 "}"，
			// 这种笔误必须当场报错：否则它会以字面量发出去，最后表现为"凭据失效"，
			// 排查成本极高。只有不像变量名时才当作普通文本透传。
			if looksLikeEnvName(in[i+2:]) {
				errOut = fmt.Errorf("变量引用缺少右花括号: %q", in[i:])
				break
			}
			out.WriteByte(in[i])
			i++
			continue
		}
		body := in[i+2 : i+2+end]
		i += 2 + end + 1
		if strings.Contains(body, "${") {
			errOut = fmt.Errorf("不支持嵌套的变量引用: ${%s}", body)
			break
		}

		name, def, hasDef := cut(body, ":-")
		name = strings.TrimSpace(name)
		if name == "" || !isEnvName(name) {
			errOut = fmt.Errorf("非法的环境变量名: %q", body)
			break
		}
		val, ok := os.LookupEnv(name)
		if !ok {
			if filePath, fileOK := os.LookupEnv(name + "_FILE"); fileOK {
				content, err := os.ReadFile(filePath)
				if err != nil {
					errOut = fmt.Errorf("读取环境变量 %s_FILE 指向的文件 %s 失败: %w", name, filePath, err)
					break
				}
				val, ok = strings.TrimSpace(string(content)), true
			}
		}
		if !ok {
			if !hasDef {
				errOut = fmt.Errorf("环境变量 %s 未定义（如允许为空请写 ${%s:-}）", name, name)
				break
			}
			val = def
		}
		out.WriteString(val)
	}
	if errOut != nil {
		return "", errOut
	}
	return out.String(), nil
}

func cut(s, sep string) (before, after string, found bool) {
	if idx := strings.Index(s, sep); idx >= 0 {
		return s[:idx], s[idx+len(sep):], true
	}
	return s, "", false
}

func isEnvName(s string) bool {
	for i, r := range s {
		switch {
		case r == '_' || unicode.IsDigit(r):
			// ok
		case unicode.IsUpper(r) || unicode.IsLower(r):
			// ok
		default:
			return false
		}
		if i == 0 && unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

func (c *Config) applyDefaults() {
	d := Default()
	if strings.TrimSpace(c.App.Timezone) == "" {
		c.App.Timezone = d.App.Timezone
	}
	if strings.TrimSpace(c.App.Schedule) == "" {
		c.App.Schedule = d.App.Schedule
	}
	if c.App.Concurrency <= 0 {
		c.App.Concurrency = d.App.Concurrency
	}
	if c.App.TimeoutSeconds <= 0 {
		c.App.TimeoutSeconds = d.App.TimeoutSeconds
	}
	if c.App.RetryBackoffSeconds <= 0 {
		c.App.RetryBackoffSeconds = d.App.RetryBackoffSeconds
	}
	c.App.WAFChallenge = strings.ToLower(strings.TrimSpace(c.App.WAFChallenge))
	if c.App.WAFChallenge == "" {
		c.App.WAFChallenge = d.App.WAFChallenge
	}
	if strings.TrimSpace(c.Database.Path) == "" {
		c.Database.Path = d.Database.Path
	}
	if strings.TrimSpace(c.Server.Addr) == "" {
		c.Server.Addr = d.Server.Addr
	}
	if strings.TrimSpace(c.Notify.Bark.Server) == "" {
		c.Notify.Bark.Server = d.Notify.Bark.Server
	}
	c.Notify.Bark.Server = strings.TrimRight(c.Notify.Bark.Server, "/")
	if strings.TrimSpace(c.Notify.Bark.Level) == "" {
		c.Notify.Bark.Level = d.Notify.Bark.Level
	}
	if strings.TrimSpace(c.Notify.Bark.Mode) == "" {
		c.Notify.Bark.Mode = d.Notify.Bark.Mode
	}
	for i := range c.Sites {
		s := &c.Sites[i]
		if strings.TrimSpace(string(s.Credential.Type)) == "" {
			if strings.TrimSpace(s.Credential.Token) != "" && strings.TrimSpace(s.Credential.Cookie) == "" {
				s.Credential.Type = CredentialAccessToken
			} else {
				s.Credential.Type = CredentialCookie
			}
		}
		s.Credential.Type = CredentialType(strings.ToLower(strings.TrimSpace(string(s.Credential.Type))))
		if strings.TrimSpace(s.BaseURL) != "" {
			s.BaseURL = strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
		}
		if strings.TrimSpace(s.CheckinPath) == "" {
			s.CheckinPath = DefaultCheckinPath
		}
		s.CheckinPath = "/" + strings.TrimLeft(strings.TrimSpace(s.CheckinPath), "/")
		s.CheckinStatus = strings.ToLower(strings.TrimSpace(s.CheckinStatus))
		if s.CheckinStatus == "" {
			s.CheckinStatus = CheckinStatusAuto
		}
	}
}

// Validate 校验配置。返回的错误信息里包含具体字段名，便于直接定位。
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	switch c.App.WAFChallenge {
	case WAFChallengeSolve, WAFChallengeOff:
	default:
		add("app.waf_challenge 只能是 solve 或 off，当前为 %q", c.App.WAFChallenge)
	}

	if _, err := c.App.Location(); err != nil {
		add("app.timezone 非法: %v", err)
	}
	if _, _, err := ParseSchedule(c.App.Schedule); err != nil {
		add("app.schedule 非法: %v", err)
	}
	if c.App.JitterSeconds < 0 {
		add("app.jitter_seconds 不能为负")
	}
	if c.App.Concurrency < 1 {
		add("app.concurrency 至少为 1")
	}
	if c.App.TimeoutSeconds < 1 {
		add("app.timeout_seconds 至少为 1")
	}
	if c.App.Retries < 0 {
		add("app.retries 不能为负")
	}
	if strings.TrimSpace(c.Database.Path) == "" {
		add("database.path 不能为空")
	}
	if c.Notify.Bark.Enabled {
		switch c.Notify.Bark.Mode {
		case BarkModeAlways, BarkModeFailure, BarkModeSummary:
		default:
			add("notify.bark.mode 只能是 %s / %s / %s", BarkModeAlways, BarkModeFailure, BarkModeSummary)
		}
		if strings.TrimSpace(c.Notify.Bark.Key) != "" && !strings.HasPrefix(c.Notify.Bark.Server, "http") {
			add("notify.bark.server 必须以 http(s):// 开头")
		}
	}

	if len(c.Sites) == 0 {
		add("sites 不能为空，至少配置一个站点")
	}
	seen := map[string]int{}
	for i, s := range c.Sites {
		label := fmt.Sprintf("sites[%d]", i)
		if strings.TrimSpace(s.Name) == "" {
			add("%s.name 不能为空", label)
			label = fmt.Sprintf("sites[%d]", i)
		} else {
			label = fmt.Sprintf("sites[%d](%s)", i, s.Name)
			if first, dup := seen[s.Name]; dup {
				add("%s.name 与 sites[%d] 重复", label, first)
			} else {
				seen[s.Name] = i
			}
		}
		if strings.TrimSpace(s.BaseURL) == "" {
			add("%s.base_url 不能为空", label)
		} else if !strings.HasPrefix(s.BaseURL, "http://") && !strings.HasPrefix(s.BaseURL, "https://") {
			add("%s.base_url 必须以 http:// 或 https:// 开头", label)
		}
		// 归一化会自动补前导 /，所以这里只需拦住"误填完整 URL"这种常见错误。
		if strings.Contains(s.CheckinPath, "://") {
			add("%s.checkin_path 只写路径（例如 /api/user/sign_in），不要写完整 URL，当前为 %q", label, s.CheckinPath)
		}
		switch s.CheckinStatus {
		case CheckinStatusAuto, CheckinStatusOff:
		default:
			add("%s.checkin_status 只能是 auto 或 off，当前为 %q", label, s.CheckinStatus)
		}
		if strings.TrimSpace(s.Proxy) != "" {
			u, err := url.Parse(strings.TrimSpace(s.Proxy))
			switch {
			case err != nil || u.Host == "":
				add("%s.proxy 无法解析，需形如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080，当前为 %q", label, s.Proxy)
			default:
				switch strings.ToLower(u.Scheme) {
				case "http", "https", "socks5", "socks5h":
				default:
					add("%s.proxy 只支持 http / https / socks5，当前协议为 %q", label, u.Scheme)
				}
			}
		}
		if !s.Enabled {
			continue
		}
		switch s.Credential.Type {
		case CredentialCookie:
			if strings.TrimSpace(s.Credential.Cookie) == "" {
				add("%s.credential.cookie 为空（type=cookie 时必须提供，可写 ${ENV_NAME}）", label)
			}
		case CredentialAccessToken:
			if strings.TrimSpace(s.Credential.Token) == "" {
				add("%s.credential.token 为空（type=access_token 时必须提供，可写 ${ENV_NAME}）", label)
			}
		default:
			add("%s.credential.type 只能是 cookie 或 access_token，当前为 %q", label, s.Credential.Type)
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("配置校验失败:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// ParseSchedule 解析 "HH:MM" 形式的每日触发时间。
func ParseSchedule(s string) (hour, minute int, err error) {
	trimmed := strings.TrimSpace(s)
	parts := strings.Split(trimmed, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("期望 HH:MM 格式，实际为 %q", s)
	}
	hour, err = parseClockPart(parts[0], 24)
	if err != nil {
		return 0, 0, fmt.Errorf("小时部分非法: %w", err)
	}
	minute, err = parseClockPart(parts[1], 60)
	if err != nil {
		return 0, 0, fmt.Errorf("分钟部分非法: %w", err)
	}
	return hour, minute, nil
}

func parseClockPart(s string, max int) (int, error) {
	if s == "" || len(s) > 2 {
		return 0, fmt.Errorf("%q", s)
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q", s)
		}
		n = n*10 + int(r-'0')
	}
	if n >= max {
		return 0, fmt.Errorf("%d 超出范围 [0,%d)", n, max)
	}
	return n, nil
}

// looksLikeEnvName 判断字符串开头是否像环境变量名（首字符为字母/下划线，后续为字母数字下划线）。
func looksLikeEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			if i == 0 && unicode.IsDigit(r) {
				return false
			}
			continue
		}
		return i > 0
	}
	return true
}
