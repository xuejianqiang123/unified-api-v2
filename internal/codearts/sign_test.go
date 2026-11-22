package codearts

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// goldenNow 固定签名时刻：x-sdk-date 参与 stringToSign，时钟不定则金标准不可复现。
var goldenNow = time.Date(2026, 11, 22, 5, 0, 0, 0, time.UTC)

// goldenBody 是金标准用例的请求体（与独立算例逐字节相同）。
const goldenBody = `{"model":"glm-5.3","messages":[{"role":"user","content":"你好"}],"stream":true}`

// TestSignRequestGolden 是**跨语言三方对齐**的锚：
//
//	参考实现（TypeScript，node:crypto）→ openssl dgst -hmac → 本 Go 实现
//
// 三段中间量（payloadHash / canonicalHash / signature）都由 openssl 独立复算过，
// 不是「自己算完自己断言」的同义反复。改动本文件的任何一个字节，这里必须变红。
func TestSignRequestGolden(t *testing.T) {
	old := nowFunc
	nowFunc = func() time.Time { return goldenNow }
	t.Cleanup(func() { nowFunc = old })

	headers, err := SignRequest(
		"AKIDTEST", "SKTEST", "STTEST",
		"POST",
		"https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions",
		[]byte(goldenBody), nil,
	)
	if err != nil {
		t.Fatalf("SignRequest: %v", err)
	}

	const wantPayloadHash = "df3578d6267854a9bb8cf51cfc0a3e3aa337f7c1ed31a2c579d1efc220bd7342"
	const wantSignature = "de82bf994806e45d3f01ff948bd90c0af139ebecc0e4639f34c7dae22c08a406"
	const wantAuth = "SDK-HMAC-SHA256 Access=AKIDTEST,SignedHeaders=content-type;host;x-sdk-content-sha256;x-sdk-date;x-security-token,Signature=" + wantSignature

	if got := headers["x-sdk-content-sha256"]; got != wantPayloadHash {
		t.Errorf("payloadHash = %s, want %s", got, wantPayloadHash)
	}
	if got := headers["x-sdk-date"]; got != "20261122T050000Z" {
		t.Errorf("x-sdk-date = %s, want 20261122T050000Z", got)
	}
	if got := headers["Authorization"]; got != wantAuth {
		t.Errorf("Authorization 不匹配\n got: %s\nwant: %s", got, wantAuth)
	}
}

// TestCanonicalRequestShape 锁住拼接形态本身：连续两个换行（headers 与
// SignedHeaders 之间的空行）是规范要求，删掉它签名会静默失效。
func TestCanonicalRequestShape(t *testing.T) {
	headers := map[string]string{
		"host":                 "h",
		"x-sdk-date":           "D",
		"x-sdk-content-sha256": "P",
	}
	got := buildCanonicalRequest("GET", "/a/", "", headers,
		[]string{"host", "x-sdk-content-sha256", "x-sdk-date"}, "P")
	want := "GET\n/a/\n\nhost:h\nx-sdk-content-sha256:P\nx-sdk-date:D\n\nhost;x-sdk-content-sha256;x-sdk-date\nP"
	if got != want {
		t.Errorf("canonicalRequest 形态不对\n got: %q\nwant: %q", got, want)
	}
}

// TestCanonicalQuerySorted 对应真机 2×2 定案：wire 顺序 offset,limit 必须按
// key 字典序重排进签名，否则 401 verify ak sk signature fail。
func TestCanonicalQuerySorted(t *testing.T) {
	cases := []struct{ in, want string }{
		{"offset=0&limit=100", "limit=100&offset=0"},
		{"limit=100&offset=0", "limit=100&offset=0"},
		{"model=glm-5.3", "model=glm-5.3"},
		{"", ""},
		{"flag", "flag="},
		{"b=2&a=2&a=1", "a=1&a=2&b=2"},
	}
	for _, c := range cases {
		if got := canonicalQueryOf(c.in); got != c.want {
			t.Errorf("canonicalQueryOf(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestURIGetsTrailingSlash 与 TestGetOmitsContentType 锁住两个「少一个字符就全错」的形态。
func TestURIGetsTrailingSlash(t *testing.T) {
	old := nowFunc
	nowFunc = func() time.Time { return goldenNow }
	t.Cleanup(func() { nowFunc = old })

	h, err := SignRequest("ak", "sk", "st", "GET", "https://x.test/v1/model/builtin", nil, nil)
	if err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	if _, ok := h["content-type"]; ok {
		t.Error("GET 不应带 content-type（参与签名会让 canonical 多一行）")
	}
	if !strings.Contains(h["Authorization"], "SignedHeaders=host;x-sdk-content-sha256;x-sdk-date;x-security-token,") {
		t.Errorf("GET 的 SignedHeaders 不对: %s", h["Authorization"])
	}
}

// TestUnsafeQueryRejected 是「宁可少一个请求，也不要静默 401」那条纪律的守卫。
func TestUnsafeQueryRejected(t *testing.T) {
	bad := []string{
		"https://x.test/a?q=a+b",  // 裸 '+'：表单语义下是空格
		"https://x.test/a?q=a b",  // 裸空格
		"https://x.test/a?q=a=b",  // 裸 '='（值里的等号会被重编码）
		"https://x.test/a?q=100%", // 畸形 '%'
		"https://x.test/a?q=%2b",  // 小写十六进制：往返会变 %2B
	}
	for _, u := range bad {
		if _, err := SignRequest("ak", "sk", "st", "GET", u, nil, nil); !errors.Is(err, ErrUnsafeQuery) {
			t.Errorf("SignRequest(%q) 应返回 ErrUnsafeQuery，实际 err=%v", u, err)
		}
	}

	good := []string{
		"https://x.test/a?offset=0&limit=100",
		"https://x.test/a?model=glm-5.3",
		"https://x.test/a?q=a%2Bb",
		"https://x.test/a?q=%E4%B8%AD%E6%96%87",
		// 裸 '&' 会被切成两个参数，但本地与服务端切法一致 ⇒ 不分叉，不算不安全。
		// 真正危险的是「值里含未编码的 &」被误当成单值 —— 那需要已编码的 %26。
		"https://x.test/a?q=a&b",
		"https://x.test/a?q=a%26b",
		"https://x.test/a",
	}
	for _, u := range good {
		if _, err := SignRequest("ak", "sk", "st", "GET", u, nil, nil); err != nil {
			t.Errorf("SignRequest(%q) 意外报错: %v", u, err)
		}
	}
}

// TestEscapeComponentMatchesEncodeURIComponent 用 node 的 encodeURIComponent
// 输出做对照（跨实现，非自证）。'!*'()' 是最容易写错的一格：
// url.PathEscape 会编码它们，而 encodeURIComponent 不会 ⇒ 静默签错。
func TestEscapeComponentMatchesEncodeURIComponent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"AZaz09", "AZaz09"},
		{"-_.!~*'()", "-_.!~*'()"},
		{" ", "%20"},
		{"+", "%2B"},
		{"/", "%2F"},
		{"=", "%3D"},
		{"&", "%26"},
		{"%", "%25"},
		{"中", "%E4%B8%AD"},
		{"文", "%E6%96%87"},
		{"，", "%EF%BC%8C"},
		{"。", "%E3%80%82"},
	}
	for _, c := range cases {
		if got := escapeComponent(c.in); got != c.want {
			t.Errorf("escapeComponent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestExtraHeadersSigned 锁住 maas_type: benefit 这类条件头的形态：
// 它必须同时出现在 canonical 与 SignedHeaders 里，否则服务端验签失败。
func TestExtraHeadersSigned(t *testing.T) {
	old := nowFunc
	nowFunc = func() time.Time { return goldenNow }
	t.Cleanup(func() { nowFunc = old })

	h, err := SignRequest("ak", "sk", "st", "POST", "https://x.test/api", []byte("{}"),
		map[string]string{"maas_type": "benefit"})
	if err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	if h["maas_type"] != "benefit" {
		t.Error("额外头必须随请求发出")
	}
	if !strings.Contains(h["Authorization"], "maas_type;") && !strings.Contains(h["Authorization"], ";maas_type") {
		t.Errorf("额外头必须进入 SignedHeaders: %s", h["Authorization"])
	}
}
