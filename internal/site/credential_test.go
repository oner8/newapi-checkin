package site

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"newapi-checkin/internal/config"
)

func TestNormalizeCookie(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"整段 Cookie 头", "session=abc; other=1", "session=abc; other=1"},
		{"分隔符缺空格", "session=abc;other=1", "session=abc; other=1"},
		{"裸 session 值 base64 填充", "MTc4OQ==", "session=MTc4OQ=="},
		{"裸 session 值 URL 编码填充", "MTc4OQ%3D%3D", "session=MTc4OQ%3D%3D"},
		{"裸 JWT 风格值", "eyJhbGciOi.abc.def", "session=eyJhbGciOi.abc.def"},
		{"带 Cookie 前缀", "Cookie: session=abc", "session=abc"},
		{"带换行", "session=abc;\n  other=1", "session=abc; other=1"},
		{"带制表符", "session=abc;\tother=1", "session=abc; other=1"},
		{"多余的分号", "session=abc;;", "session=abc"},
		{"空值", "", ""},
		{"只有空白", "   \n ", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeCookie(tc.in); got != tc.want {
				t.Errorf("NormalizeCookie(%q) = %q, 期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeCookieDoesNotDecode 确保含 %3D%3D 的值原样透传（不做 URL 解码）。
func TestNormalizeCookieDoesNotDecode(t *testing.T) {
	raw := "session=MTc4OQ%3D%3D"
	if got := NormalizeCookie(raw); got != raw {
		t.Errorf("不应解码 Cookie 值: %q", got)
	}
}

func TestCredentialApply(t *testing.T) {
	t.Run("cookie 模式", func(t *testing.T) {
		cred := Credential{Type: config.CredentialCookie, Cookie: "MTc4OQ=="}
		h := http.Header{}
		cred.Apply(h)
		if got := h.Get("Cookie"); got != "session=MTc4OQ==" {
			t.Errorf("Cookie 头不对: %q", got)
		}
		if got := h.Get("Authorization"); got != "" {
			t.Errorf("cookie 模式不应带 Authorization: %q", got)
		}
	})

	t.Run("access_token 模式", func(t *testing.T) {
		cred := Credential{Type: config.CredentialAccessToken, Token: "sk-abc"}
		h := http.Header{}
		cred.Apply(h)
		if got := h.Get("Authorization"); got != "Bearer sk-abc" {
			t.Errorf("Authorization 头不对: %q", got)
		}
		if got := h.Get("Cookie"); got != "" {
			t.Errorf("access_token 模式不应带 Cookie: %q", got)
		}
	})

	t.Run("附加 New-Api-User", func(t *testing.T) {
		cred := Credential{Type: config.CredentialAccessToken, Token: "sk-abc", NewAPIUser: "42"}
		h := http.Header{}
		cred.Apply(h)
		if got := h.Get("New-Api-User"); got != "42" {
			t.Errorf("New-Api-User 头不对: %q", got)
		}
	})
}

func TestCredentialDescribeHidesSecret(t *testing.T) {
	secret := "MTc4OQ0aVeryLongSessionValueWithoutSpaces"
	cred := Credential{Type: config.CredentialCookie, Cookie: secret}
	desc := cred.Describe()

	if strings.Contains(desc, secret) {
		t.Fatalf("描述里泄露了完整凭据: %q", desc)
	}
	if !strings.Contains(desc, "cookie") || !strings.Contains(desc, "…") {
		t.Errorf("描述应当包含类型与掩码: %q", desc)
	}
	if names := cred.CookieNames(); len(names) != 1 || names[0] != "session" {
		t.Errorf("CookieNames 应当只返回键名: %v", names)
	}

	tokenCred := Credential{Type: config.CredentialAccessToken, Token: secret}
	tokenDesc := tokenCred.Describe()
	if strings.Contains(tokenDesc, secret) {
		t.Fatalf("令牌描述里泄露了完整值: %q", tokenDesc)
	}
	if len(tokenCred.CookieNames()) != 0 {
		t.Error("访问令牌模式不应返回 Cookie 键名")
	}
}

func TestMaskSecretShortValue(t *testing.T) {
	if got := maskSecret("short"); got != "*****" {
		t.Errorf("短值应当全部打码: %q", got)
	}
	if got := maskSecret(""); got != "空" {
		t.Errorf("空值提示不对: %q", got)
	}
}

// TestPublicInfoLeaksNothing 验证对外接口使用的凭据信息不含任何密文片段。
//
// /api/sites 是未鉴权接口，因此它拿到的必须是 PublicInfo（只有类型与键名），
// 而不是带掩码片段的 Describe()。
func TestPublicInfoLeaksNothing(t *testing.T) {
	raw := "MTc4OQ0aVeryLongSessionValueWithoutSpaces"

	cookie := Credential{Type: config.CredentialCookie, Cookie: raw}
	info := cookie.PublicInfo()
	if info.Type != "cookie" {
		t.Errorf("type = %q，期望 cookie", info.Type)
	}
	if !info.HasSecret {
		t.Error("配置了 cookie 时 has_secret 应为 true")
	}
	if len(info.CookieNames) != 1 || info.CookieNames[0] != "session" {
		t.Errorf("cookie_names = %v，期望 [session]", info.CookieNames)
	}
	rendered := fmt.Sprintf("%+v", info)
	if strings.Contains(rendered, raw) {
		t.Fatalf("PublicInfo 里出现了完整凭据: %s", rendered)
	}
	if strings.Contains(rendered, "…") || strings.Contains(rendered, "MTc4") {
		t.Errorf("PublicInfo 不应包含任何掩码片段: %s", rendered)
	}

	token := Credential{Type: config.CredentialAccessToken, Token: raw, NewAPIUser: "42"}
	tokenInfo := token.PublicInfo()
	if tokenInfo.Type != "access_token" || !tokenInfo.HasSecret || !tokenInfo.NewAPIUser {
		t.Errorf("令牌站点 PublicInfo 不对: %+v", tokenInfo)
	}
	if len(tokenInfo.CookieNames) != 0 {
		t.Errorf("访问令牌模式不应返回 cookie_names: %v", tokenInfo.CookieNames)
	}
	if strings.Contains(fmt.Sprintf("%+v", tokenInfo), raw) {
		t.Fatalf("令牌 PublicInfo 里出现了完整令牌: %+v", tokenInfo)
	}
}

// TestMaskSecretScalesWithLength 验证暴露的字符数与密钥长度成正比。
//
// 旧实现固定露出首尾各 4 个字符：一个 9 字符的密钥会被暴露 8 个字符。
func TestMaskSecretScalesWithLength(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "空"},
		{"abcdef", "******"},
		{"abcdefg", "a…g"},
		{"abcdefghijklmnop", "abcd…mnop"},
		{"MTc4OQ0aVeryLongSessionValueWithoutSpaces", "MTc4…aces"},
	}
	for _, tc := range cases {
		if got := maskSecret(tc.in); got != tc.want {
			t.Errorf("maskSecret(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeCookieBareValueWithCookieNameInside 验证裸 session 值里恰好含
// "csrf"/"new_api" 这类子串时仍按裸值处理，而不是被误判成整段 Cookie 头。
//
// 这是很隐蔽的一类失败：误判后请求里根本没有 session，站点的表现却只是"未登录"，
// 用户只会看到"凭据失效"，反复重取 Cookie 也修不好。
func TestNormalizeCookieBareValueWithCookieNameInside(t *testing.T) {
	// 这三个值都**字面**含有 Cookie 键名子串，且都不是 "name=value" 形式。
	// 用真实子串而不是它的 base64 编码：编码之后子串就不存在了，断言会恒过（假绿）。
	bareValues := []string{
		"aGVsbG8-csrf-token",    // 含字面 "csrf"
		"prefix-new_api_user",   // 含字面 "new_api"
		"MTc4-new-api-lookup==", // 含字面 "new-api"，且带 base64 填充
	}
	for _, bare := range bareValues {
		want := "session=" + bare
		if got := NormalizeCookie(bare); got != want {
			t.Errorf("裸值被误判: NormalizeCookie(%q) = %q，期望 %q", bare, got, want)
		}
	}

	// 真正的 Cookie 头仍然要按头处理（键名整体匹配）。
	for _, header := range []string{"csrf_token=abc", "session=abc", "cf_clearance=abc"} {
		if got := NormalizeCookie(header); got != header {
			t.Errorf("Cookie 头被误判为裸值: NormalizeCookie(%q) = %q", header, got)
		}
	}
}
