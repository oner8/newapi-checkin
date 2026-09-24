package site

import "bytes"

// 阿里云 WAF 的 JS 挑战（acw_sc__v2）是"可离线求解"的，因此不需要浏览器，也不需要用户手工复制 Cookie。
//
// 挑战页形如：
//
//	<html><script>var arg1='EEFA790EB387F45DA18DA984FFBBBFA66EAF0D6E';
//	(function(a,c){var G=a0j,d=a();while(!![]){try{...}}})();</script></html>
//
// 浏览器执行这段脚本后会写入 Cookie acw_sc__v2：脚本做的事是"按固定位置表重排 arg1 的 40 个
// 十六进制字符，再与固定密钥逐字节异或"。因为是纯字符串变换，用 Go 复刻即可。
//
// 实测（2026-09-24）：对一个真实站点（server: ESA / x-tengine-error: denied by http_custom）先取到
// 挑战页、用本算法算出 acw_sc__v2 再带上重放，即返回正常 JSON，无需人工介入。
const (
	// acwArg1Marker 是挑战页里 arg1 的固定写法。
	acwArg1Marker = "arg1='"
	// acwArg1Len 是 arg1 的固定长度（也是置换表大小）。
	acwArg1Len = 40
	// acwXorKey 是公开实现里固定的异或密钥（40 个十六进制字符）。
	acwXorKey = "3000176000856006061501533003690027800375"
	// acwCookieName 是求解后要携带的 Cookie 名。
	acwCookieName = "acw_sc__v2"
)

// acwShuffle 是置换用的固定位置表（值从 1 开始计数）。
var acwShuffle = [acwArg1Len]int{
	0xf, 0x23, 0x1d, 0x18, 0x21, 0x10, 0x1, 0x26, 0xa, 0x9,
	0x13, 0x1f, 0x28, 0x1b, 0x16, 0x17, 0x19, 0xd, 0x6, 0xb,
	0x27, 0x12, 0x14, 0x8, 0xe, 0x15, 0x20, 0x1a, 0x2, 0x1e,
	0x7, 0x4, 0x11, 0x5, 0x3, 0x1c, 0x22, 0x25, 0xc, 0x24,
}

const hexDigits = "0123456789abcdef"

// acwScV2 由挑战页里的 arg1 算出 acw_sc__v2 的值；输入不合法时返回空串。
func acwScV2(arg1 string) string {
	if len(arg1) != acwArg1Len {
		return ""
	}

	// 第一步：按固定位置表重排（原脚本里的 unsbox）。
	shuffled := make([]byte, acwArg1Len)
	for i := 0; i < acwArg1Len; i++ {
		for j, pos := range acwShuffle {
			if pos == i+1 {
				shuffled[j] = arg1[i]
				break
			}
		}
	}

	// 第二步：与固定密钥逐字节异或，输出 40 个十六进制字符。
	out := make([]byte, 0, acwArg1Len)
	for i := 0; i+1 < acwArg1Len; i += 2 {
		a, okA := hexByte(shuffled[i], shuffled[i+1])
		b, okB := hexByte(acwXorKey[i], acwXorKey[i+1])
		if !okA || !okB {
			return ""
		}
		v := a ^ b
		out = append(out, hexDigits[v>>4], hexDigits[v&0xf])
	}
	return string(out)
}

// extractACWArg1 从响应体里取出挑战参数 arg1；不是这类挑战页时返回 false。
func extractACWArg1(raw []byte) (string, bool) {
	idx := bytes.Index(raw, []byte(acwArg1Marker))
	if idx < 0 {
		return "", false
	}
	rest := raw[idx+len(acwArg1Marker):]
	end := bytes.IndexByte(rest, '\'')
	if end != acwArg1Len {
		return "", false
	}
	arg1 := string(rest[:end])
	for i := 0; i < len(arg1); i++ {
		if _, ok := hexValue(arg1[i]); !ok {
			return "", false
		}
	}
	return arg1, true
}

// hexByte 把两个十六进制字符转成一个字节。
func hexByte(hi, lo byte) (byte, bool) {
	h, okH := hexValue(hi)
	l, okL := hexValue(lo)
	if !okH || !okL {
		return 0, false
	}
	return h<<4 | l, true
}

// hexValue 把单个十六进制字符转成数值。
func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}
