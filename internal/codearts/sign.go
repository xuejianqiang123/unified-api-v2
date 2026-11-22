// Package codearts 华为云 CodeArts（码道）provider 的协议实现。
//
// 本文件只做一件事：把华为云 API 网关的 SDK-HMAC-SHA256 签名算对。
// 它是所有 CodeArts 请求（聊天 / 额度 / 签到 / 模型目录）的共同地基 ——
// 签错一个字，上游恒回 401 verify ak sk signature fail，且与凭据好坏无关。
//
// 算法口径与参考实现逐字节对齐（非猜测）：
//   - canonicalRequest = METHOD \n URI \n QUERY \n HEADERS \n \n SIGNEDHEADERS \n PAYLOADHASH
//   - stringToSign     = "SDK-HMAC-SHA256" \n DATE \n SHA256(canonicalRequest)
//   - signature        = HMAC-SHA256(SK, stringToSign) 的十六进制小写
//   - Authorization    = SDK-HMAC-SHA256 Access=<AK>,SignedHeaders=<...>,Signature=<...>
//
// 三处容易写错、且都已在真机定案过的细节：
//  1. CanonicalURI 必须以 "/" 结尾（无尾斜杠服务端重算后与本地不一致）；
//  2. CanonicalQuery 按 key 字典序重排（wire URL 顺序无关），且只解 %XX、不解 '+'；
//  3. 编码口径是 encodeURIComponent 的**不编码集**（A-Za-z0-9-_.!~*'()），
//     不是 url.PathEscape / url.QueryEscape —— 后者对 '!*'()' 的处理不同，
//     会在含这些字符的 query 上静默签错。
package codearts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrUnsafeQuery 表示 query 值不在「三种编码口径逐字节相同」的安全集内。
//
// 这类值（裸 '+'、空格、'&'、'='、畸形 '%'）会让本地 canonical 与服务端
// 重算的口径分叉，结果是静默 401。选择「拒绝发这个请求」而不是「赌一把」
// —— 宁可少一个请求，也不要给出一个看起来像凭据问题的假故障。
var ErrUnsafeQuery = errors.New("codearts: query 值不在签名安全集内")

// nowFunc 供测试注入固定时刻（签名含 x-sdk-date，不可复现的时钟会让金标准用例失效）。
var nowFunc = time.Now

// SignRequest 为一次华为云 APIG 请求生成需要合并到 HTTP 头的签名结果。
//
// method 为 HTTP 方法（大小写不敏感；GET 不签 content-type）；
// rawURL 是**将要实际发出**的完整 URL（query 顺序不作要求）；
// body 为请求体字节（GET 传 nil）；
// extra 是额外参与签名的头（如 maas_type: benefit），会原样进入 SignedHeaders。
//
// 返回的头里不含 host —— 由 HTTP 客户端按实际连接目标生成，手工设置会被忽略。
func SignRequest(ak, sk, securityToken, method, rawURL string, body []byte, extra map[string]string) (map[string]string, error) {
	if ak == "" || sk == "" {
		return nil, errors.New("codearts: 凭据缺少 AK/SK")
	}
	if !isSignedQuerySafe(rawURL) {
		// 只报路径，不报 query 值：query 可能含账号相关信息。
		return nil, fmt.Errorf("%w: %s", ErrUnsafeQuery, rawURL[:strings.IndexByte(rawURL+"?", '?')])
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("codearts: 解析 URL 失败: %w", err)
	}

	uri := u.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}

	query := canonicalQueryOf(u.RawQuery)
	dateStamp := nowFunc().UTC().Format("20060102T150405Z")
	payloadHash := sha256Hex(body)

	headers := map[string]string{
		"host":                 u.Host,
		"x-sdk-date":           dateStamp,
		"x-sdk-content-sha256": payloadHash,
	}
	if securityToken != "" {
		headers["x-security-token"] = securityToken
	}
	for k, v := range extra {
		headers[strings.ToLower(k)] = v
	}
	if !strings.EqualFold(method, "GET") {
		headers["content-type"] = "application/json"
	}

	signedHeaders := make([]string, 0, len(headers))
	for k := range headers {
		signedHeaders = append(signedHeaders, k)
	}
	sort.Strings(signedHeaders)

	canonicalRequest := buildCanonicalRequest(method, uri, query, headers, signedHeaders, payloadHash)
	stringToSign := "SDK-HMAC-SHA256\n" + dateStamp + "\n" + sha256Hex([]byte(canonicalRequest))
	signature := hmacSHA256Hex([]byte(sk), []byte(stringToSign))

	out := map[string]string{
		"x-sdk-date":           dateStamp,
		"x-sdk-content-sha256": payloadHash,
		"Authorization": fmt.Sprintf(
			"SDK-HMAC-SHA256 Access=%s,SignedHeaders=%s,Signature=%s",
			ak, strings.Join(signedHeaders, ";"), signature,
		),
	}
	if securityToken != "" {
		out["x-security-token"] = securityToken
	}
	for k, v := range extra {
		out[k] = v
	}
	return out, nil
}

// buildCanonicalRequest 逐字节复刻参考实现的行拼接。
//
// 注意 headers 段是「多行作为一个整体」参与 join，因此最终形态里有连续两个
// 换行（headers 末行与 SignedHeaders 之间）—— 这是规范要求的空行，不是笔误。
func buildCanonicalRequest(method, uri, query string, headers map[string]string, signedHeaders []string, payloadHash string) string {
	lines := make([]string, 0, len(signedHeaders))
	for _, k := range signedHeaders {
		lines = append(lines, k+":"+headers[k])
	}
	return strings.Join([]string{
		method,
		uri,
		query,
		strings.Join(lines, "\n"),
		"",
		strings.Join(signedHeaders, ";"),
		payloadHash,
	}, "\n")
}

// canonicalQueryOf 把 wire query 规范成按 key（同 key 再按 value）排序的形态。
//
// 只解 %XX、不解 '+'：表单语义里 '+' 是空格，而 decodeURIComponent 不解它 ——
// 两边口径一旦分叉就是 401。安全集已把裸 '+' 拦在外面，这里保持保守。
func canonicalQueryOf(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	type pair struct{ k, v string }
	pairs := make([]pair, 0, strings.Count(rawQuery, "&")+1)
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, found := strings.Cut(part, "=")
		if !found {
			v = ""
		}
		pairs = append(pairs, pair{safeDecode(k), safeDecode(v)})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, escapeComponent(p.k)+"="+escapeComponent(p.v))
	}
	return strings.Join(out, "&")
}

// safeDecode 解 %XX；畸形序列（如 "?a=100%"）原样返回，绝不抛。
// 一条本来能发的请求不该因为值里有个裸 '%' 变成异常路径。
func safeDecode(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// escapeComponent 是 encodeURIComponent 的等价物。
//
// 不能用 url.PathEscape / url.QueryEscape：
//   - QueryEscape 把空格编成 '+'（错，需 %20）；
//   - PathEscape 的保留集与 encodeURIComponent 不一致（对 '!' '*' "'" '(' ')'
//     的处理不同），而这些字符恰好在安全集内 —— 会在含它们的值上静默签错。
func escapeComponent(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreservedByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperhex[c>>4])
		b.WriteByte(upperhex[c&0x0f])
	}
	return b.String()
}

// isUnreservedByte 判定 encodeURIComponent 不编码的 ASCII 字节。
// 非 ASCII 一律编码（与 encodeURIComponent 对 UTF-8 的处理一致）。
func isUnreservedByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	}
	return false
}

// isSignedQuerySafe 检查 URL 的**键与值**是否都落在安全集内。
// 键也参与 canonical 的编码往返，键出集合同样会分叉。
func isSignedQuerySafe(rawURL string) bool {
	_, rawQuery, found := strings.Cut(rawURL, "?")
	if !found || rawQuery == "" {
		return true
	}
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, hasEq := strings.Cut(part, "=")
		if !hasEq {
			v = ""
		}
		if !isCanonicalQueryValueSafe(k) || !isCanonicalQueryValueSafe(v) {
			return false
		}
	}
	return true
}

// safeSetRe 的等价实现：安全字符 = encodeURIComponent 的不编码集 + 大写 %XX。
//
// 逐字节手写而非用正则，是为了让「裸 '+'、裸 '%'、空格、'&'、'=' 不安全」
// 这条判据在代码里一眼可读 —— 它已经踩过两次（真机 401，症状像凭据坏了）。
func isCanonicalQueryValueSafe(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == '%' {
			if i+3 > len(v) || !isUpperHex(v[i+1]) || !isUpperHex(v[i+2]) {
				return false
			}
			i += 2
			continue
		}
		if !isUnreservedByte(c) {
			return false
		}
	}
	return true
}

func isUpperHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F')
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256Hex(key, data []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}
