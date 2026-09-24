package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig 把 YAML 内容写进临时文件并返回路径。
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	return path
}

const minimalSite = `
sites:
  - name: demo
    base_url: https://api.example.com/
    enabled: true
    credential:
      type: cookie
      cookie: session=abc
`

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalSite))
	if err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}

	cases := []struct {
		name string
		got  any
		want any
	}{
		{"app.timezone", cfg.App.Timezone, "Asia/Shanghai"},
		{"app.schedule", cfg.App.Schedule, "08:00"},
		{"app.jitter_seconds", cfg.App.JitterSeconds, 600},
		{"app.run_on_start", cfg.App.RunOnStart, true},
		{"app.concurrency", cfg.App.Concurrency, 3},
		{"app.timeout_seconds", cfg.App.TimeoutSeconds, 20},
		{"app.retries", cfg.App.Retries, 2},
		{"app.retry_backoff_seconds", cfg.App.RetryBackoffSeconds, 60},
		{"app.dry_run", cfg.App.DryRun, false},
		{"database.path", cfg.Database.Path, "/data/newapi-checkin.db"},
		{"server.enabled", cfg.Server.Enabled, true},
		{"server.addr", cfg.Server.Addr, ":8080"},
		{"notify.bark.enabled", cfg.Notify.Bark.Enabled, true},
		{"notify.bark.server", cfg.Notify.Bark.Server, "https://api.day.app"},
		{"notify.bark.group", cfg.Notify.Bark.Group, "签到"},
		{"notify.bark.level", cfg.Notify.Bark.Level, "active"},
		{"notify.bark.mode", cfg.Notify.Bark.Mode, "summary"},
		{"site.base_url 去掉尾部斜杠", cfg.Sites[0].BaseURL, "https://api.example.com"},
		{"site.checkin_path 默认值", cfg.Sites[0].CheckinPath, DefaultCheckinPath},
		{"site.checkin_status 默认值", cfg.Sites[0].CheckinStatus, CheckinStatusAuto},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %v，期望 %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadExpandsEnv(t *testing.T) {
	t.Setenv("TEST_COOKIE_VALUE", "session=MTc4OQ%3D%3D; other=1")

	cfg, err := Load(writeConfig(t, `
sites:
  - name: demo
    base_url: https://api.example.com
    enabled: true
    credential:
      type: cookie
      cookie: ${TEST_COOKIE_VALUE}
`))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	got := cfg.Sites[0].Credential.Cookie
	// 关键：值必须原样透传，%3D 不能被解码成 '='。
	if got != "session=MTc4OQ%3D%3D; other=1" {
		t.Fatalf("cookie = %q，期望原样保留 %%3D%%3D", got)
	}
	if strings.Contains(got, "==") {
		t.Errorf("cookie 里的 %%3D 被解码了: %q", got)
	}
}

func TestLoadExpandsEnvWithDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
notify:
  bark:
    enabled: true
    key: ${TEST_BARK_KEY_DEFINITELY_MISSING:-}
sites:
  - name: demo
    base_url: https://api.example.com
    enabled: true
    credential:
      type: cookie
      cookie: ${TEST_COOKIE_DEFINITELY_MISSING:-session=fallback-value}
`))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Notify.Bark.Key != "" {
		t.Errorf("bark key = %q，期望空字符串（${VAR:-} 应允许为空）", cfg.Notify.Bark.Key)
	}
	if cfg.Sites[0].Credential.Cookie != "session=fallback-value" {
		t.Errorf("cookie = %q，期望使用默认值", cfg.Sites[0].Credential.Cookie)
	}
}

func TestLoadUndefinedEnvFailsWithFieldName(t *testing.T) {
	_, err := Load(writeConfig(t, `
sites:
  - name: demo
    base_url: https://api.example.com
    enabled: true
    credential:
      type: cookie
      cookie: ${TEST_COOKIE_ABSOLUTELY_MISSING}
`))
	if err == nil {
		t.Fatal("环境变量未定义时应当报错，实际没有报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "TEST_COOKIE_ABSOLUTELY_MISSING") {
		t.Errorf("错误信息应包含变量名，实际为: %s", msg)
	}
	if !strings.Contains(msg, "credential.cookie") {
		t.Errorf("错误信息应指出字段路径，实际为: %s", msg)
	}
}

func TestLoadEnvFromFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "cookie.txt")
	if err := os.WriteFile(secretPath, []byte("session=from-file-value\n"), 0o600); err != nil {
		t.Fatalf("写入 secret 文件失败: %v", err)
	}
	t.Setenv("TEST_COOKIE_FILE_VAR_FILE", secretPath)

	cfg, err := Load(writeConfig(t, `
sites:
  - name: demo
    base_url: https://api.example.com
    enabled: true
    credential:
      type: cookie
      cookie: ${TEST_COOKIE_FILE_VAR}
`))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Sites[0].Credential.Cookie != "session=from-file-value" {
		t.Errorf("cookie = %q，期望读取 <VAR>_FILE 并去掉结尾换行", cfg.Sites[0].Credential.Cookie)
	}
}

func TestParseSchedule(t *testing.T) {
	valid := []struct {
		in     string
		hour   int
		minute int
	}{
		{"08:00", 8, 0},
		{"00:00", 0, 0},
		{"23:59", 23, 59},
		{"8:05", 8, 5},
		{"  09:30  ", 9, 30},
	}
	for _, c := range valid {
		hour, minute, err := ParseSchedule(c.in)
		if err != nil {
			t.Errorf("ParseSchedule(%q) 报错: %v", c.in, err)
			continue
		}
		if hour != c.hour || minute != c.minute {
			t.Errorf("ParseSchedule(%q) = %d:%d，期望 %d:%d", c.in, hour, minute, c.hour, c.minute)
		}
	}

	invalid := []string{"", "8", "24:00", "08:60", "08:0:0", "ab:cd", "-1:00", "08:-1"}
	for _, c := range invalid {
		if _, _, err := ParseSchedule(c); err == nil {
			t.Errorf("ParseSchedule(%q) 应当报错，实际通过", c)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		contains string
	}{
		{
			name: "站点名为空",
			body: `
sites:
  - name: ""
    base_url: https://a.example.com
    enabled: true
    credential: {type: cookie, cookie: session=x}
`,
			contains: "name 不能为空",
		},
		{
			name: "站点名重复",
			body: `
sites:
  - name: dup
    base_url: https://a.example.com
    enabled: true
    credential: {type: cookie, cookie: session=x}
  - name: dup
    base_url: https://b.example.com
    enabled: true
    credential: {type: cookie, cookie: session=y}
`,
			contains: "重复",
		},
		{
			name: "proxy 缺少协议",
			body: `
sites:
  - name: demo
    base_url: https://a.example.com
    enabled: true
    proxy: 127.0.0.1:7890
    credential: {type: cookie, cookie: session=x}
`,
			contains: "proxy",
		},
		{
			name: "proxy 协议不支持",
			body: `
sites:
  - name: demo
    base_url: https://a.example.com
    enabled: true
    proxy: ftp://127.0.0.1:21
    credential: {type: cookie, cookie: session=x}
`,
			contains: "proxy",
		},
		{
			name: "checkin_status 取值非法",
			body: `
sites:
  - name: demo
    base_url: https://a.example.com
    enabled: true
    checkin_status: sometimes
    credential: {type: cookie, cookie: session=x}
`,
			contains: "checkin_status",
		},
		{
			name: "checkin_path 写成完整 URL",
			body: `
sites:
  - name: demo
    base_url: https://a.example.com
    enabled: true
    checkin_path: https://a.example.com/api/user/sign_in
    credential: {type: cookie, cookie: session=x}
`,
			contains: "checkin_path",
		},
		{
			name: "cookie 模式的凭据为空",
			body: `
sites:
  - name: demo
    base_url: https://a.example.com
    enabled: true
    credential: {type: cookie, cookie: ""}
`,
			contains: "credential.cookie 为空",
		},
		{
			name: "access_token 模式的令牌为空",
			body: `
sites:
  - name: demo
    base_url: https://a.example.com
    enabled: true
    credential: {type: access_token, token: ""}
`,
			contains: "credential.token 为空",
		},
		{
			name: "凭据类型非法",
			body: `
sites:
  - name: demo
    base_url: https://a.example.com
    enabled: true
    credential: {type: oauth, cookie: session=x}
`,
			contains: "credential.type 只能是",
		},
		{
			name: "base_url 缺协议",
			body: `
sites:
  - name: demo
    base_url: api.example.com
    enabled: true
    credential: {type: cookie, cookie: session=x}
`,
			contains: "base_url 必须以 http",
		},
		{
			name:     "没有站点",
			body:     `sites: []`,
			contains: "sites 不能为空",
		},
		{
			name: "时区非法",
			body: `
app:
  timezone: Mars/Olympus
` + minimalSite,
			contains: "timezone 非法",
		},
		{
			name: "schedule 非法",
			body: `
app:
  schedule: "25:00"
` + minimalSite,
			contains: "schedule 非法",
		},
		{
			name: "bark mode 非法",
			body: `
notify:
  bark:
    enabled: true
    mode: sometimes
` + minimalSite,
			contains: "bark.mode 只能是",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, c.body))
			if err == nil {
				t.Fatalf("期望校验失败，实际通过")
			}
			if !strings.Contains(err.Error(), c.contains) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), c.contains)
			}
		})
	}
}

func TestValidateAllowsDisabledSiteWithoutCredential(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
sites:
  - name: paused
    base_url: https://a.example.com
    enabled: false
`))
	if err != nil {
		t.Fatalf("被禁用的站点不填凭据不应报错，实际: %v", err)
	}
	if got := len(cfg.EnabledSites()); got != 0 {
		t.Errorf("EnabledSites() 长度 = %d，期望 0", got)
	}
}

func TestCredentialTypeInferredFromNonEmptyField(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
sites:
  - name: tokened
    base_url: https://a.example.com
    enabled: true
    credential:
      token: pat-abcdef
`))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Sites[0].Credential.Type != CredentialAccessToken {
		t.Errorf("credential.type = %q，期望根据非空 token 推断为 access_token", cfg.Sites[0].Credential.Type)
	}
}

func TestSiteHelpers(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalSite))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if _, ok := cfg.SiteByName("demo"); !ok {
		t.Error("SiteByName(demo) 应当找到站点")
	}
	if _, ok := cfg.SiteByName("nope"); ok {
		t.Error("SiteByName(nope) 不应当找到站点")
	}
	if got := len(cfg.EnabledSites()); got != 1 {
		t.Errorf("EnabledSites() 长度 = %d，期望 1", got)
	}
}

func TestAppDurations(t *testing.T) {
	app := App{TimeoutSeconds: 20, RetryBackoffSeconds: 60, JitterSeconds: 600}
	if app.Timeout() != 20*time.Second {
		t.Errorf("Timeout() = %v，期望 20s", app.Timeout())
	}
	if app.RetryBackoff() != time.Minute {
		t.Errorf("RetryBackoff() = %v，期望 1m", app.RetryBackoff())
	}
	if app.Jitter() != 10*time.Minute {
		t.Errorf("Jitter() = %v，期望 10m", app.Jitter())
	}
}

func TestLoadReportsMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("配置文件不存在时应当报错")
	}
}

func TestExpandStringHandlesPlainValues(t *testing.T) {
	got, err := expandString("session=abc; other=1")
	if err != nil {
		t.Fatalf("不含变量的字符串不应报错: %v", err)
	}
	if got != "session=abc; other=1" {
		t.Errorf("expandString 改动了不含变量的字符串: %q", got)
	}
}

func TestExpandStringRejectsBadSyntax(t *testing.T) {
	if _, err := expandString("${UNCLOSED"); err == nil {
		t.Error("缺少右花括号时应报错")
	}
	if _, err := expandString("${1INVALID}"); err == nil {
		t.Error("非法变量名应报错")
	}
}

// TestExpandStringToleratesLiteralDollarBrace 验证"不像变量名"的 ${ 会被原样透传。
//
// 严格报错只针对看起来像变量名却漏了 "}" 的笔误；像 ${%... 这种明显不是变量引用的
// 内容（可能出现在 Cookie 或自定义请求头里）不应让服务启动失败。
func TestExpandStringToleratesLiteralDollarBrace(t *testing.T) {
	const raw = "session=${%-not-a-var"
	got, err := expandString(raw)
	if err != nil {
		t.Fatalf("不像变量名的 ${ 不应报错: %v", err)
	}
	if got != raw {
		t.Errorf("expandString(%q) = %q，期望原样透传", raw, got)
	}
}

// TestSiteCheckinPathNormalized 覆盖 fork 改了签到路径时的写法：
// 允许省略前导斜杠、允许大小写混写的 checkin_status。
func TestSiteCheckinPathNormalized(t *testing.T) {
	body := strings.Replace(minimalSite,
		"    credential:\n",
		"    checkin_path: api/user/sign_in\n    checkin_status: OFF\n    credential:\n", 1)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if got := cfg.Sites[0].CheckinPath; got != "/api/user/sign_in" {
		t.Errorf("checkin_path 应补上前导斜杠，实际 %q", got)
	}
	if got := cfg.Sites[0].CheckinStatus; got != CheckinStatusOff {
		t.Errorf("checkin_status 应统一小写为 off，实际 %q", got)
	}
}

// TestSiteProxyEnvExpansion 覆盖 sites[].proxy 支持 ${ENV} 注入（代理常带凭据，不该写进配置文件）。
func TestSiteProxyEnvExpansion(t *testing.T) {
	t.Setenv("TEST_PROXY_URL", "socks5://user:pw@127.0.0.1:1080")
	body := strings.Replace(minimalSite, "    credential:\n", "    proxy: ${TEST_PROXY_URL}\n    credential:\n", 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if got := cfg.Sites[0].Proxy; got != "socks5://user:pw@127.0.0.1:1080" {
		t.Errorf("proxy 应从环境变量注入，实际 %q", got)
	}
}
