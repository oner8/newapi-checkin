package site

import "testing"

// TestACWScV2 用独立实现（Python 复刻，脚本另存）算出的向量交叉验证"置换 + 异或"算法。
//
// 更强的证据在集成测试里：真机实测时用本算法算出的 acw_sc__v2 被阿里云 WAF 接受，
// 请求从挑战页变成了正常 JSON。
func TestACWScV2(t *testing.T) {
	cases := []struct {
		arg1 string
		want string
	}{
		{"0123456789ABCDEF0123456789ABCDEF01234567", "d2c7186598ab1a508a4f6064e4fa746323ab17c6"},
		{"EEFA790EB387F45DA18DA984FFBBBFA66EAF0D6E", "6ab47a8d3b0f8b9ef98d608d7a6c860a807be30a"},
		{"ffffffffffffffffffffffffffffffffffffffff", "cfffe89fff7a9ff9f9eafeaccffc96ffd87ffc8a"},
	}
	for _, tc := range cases {
		if got := acwScV2(tc.arg1); got != tc.want {
			t.Errorf("acwScV2(%s) = %q，期望 %q", tc.arg1, got, tc.want)
		}
	}

	if got := acwScV2("too-short"); got != "" {
		t.Errorf("长度不对应返回空串，实际 %q", got)
	}
	if got := acwScV2("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"); got != "" {
		t.Errorf("非十六进制应返回空串，实际 %q", got)
	}
}

func TestExtractACWArg1(t *testing.T) {
	const arg1 = "EEFA790EB387F45DA18DA984FFBBBFA66EAF0D6E"
	page := []byte(`<html><script>var arg1='` + arg1 + `';(function(a,c){var G=a0j,d=a();})();</script></html>`)

	got, ok := extractACWArg1(page)
	if !ok || got != arg1 {
		t.Fatalf("应从挑战页提取 arg1，实际 ok=%v value=%q", ok, got)
	}

	if _, ok := extractACWArg1([]byte(`<html><body>Just a moment...</body></html>`)); ok {
		t.Error("普通页面不应被当成 acw 挑战页")
	}
	if _, ok := extractACWArg1([]byte(`var arg1='XYZ'`)); ok {
		t.Error("长度或字符集不合法的 arg1 不应提取成功")
	}
}
