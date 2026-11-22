package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
)

// TestSignDPoPShape 锁住 JWT 形状与签名格式：
// 头必须是 dpop+jwt + 内嵌公钥 JWK，签名必须是 R||S 64 字节（不是 DER）。
func TestSignDPoPShape(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	jwk := &DpopJWK{
		Kty: "EC", Crv: "P-256",
		X: b64(priv.X.Bytes()),
		Y: b64(priv.Y.Bytes()),
		D: b64(priv.D.Bytes()),
	}

	jws, err := signDPoP(jwk, "POST", stsTokenEndpoint)
	if err != nil {
		t.Fatalf("signDPoP: %v", err)
	}
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		t.Fatalf("JWS 段数 = %d, want 3", len(parts))
	}

	// 头
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var hdr struct {
		Alg string            `json:"alg"`
		Typ string            `json:"typ"`
		JWK map[string]string `json:"jwk"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		t.Fatalf("头不是 JSON: %v", err)
	}
	if hdr.Alg != "ES256" || hdr.Typ != "dpop+jwt" {
		t.Errorf("头 = %+v, want ES256/dpop+jwt", hdr)
	}
	if hdr.JWK["x"] != jwk.X || hdr.JWK["y"] != jwk.Y {
		t.Error("头里的公钥应与输入一致")
	}
	if _, exists := hdr.JWK["d"]; exists {
		t.Error("公钥 JWK 绝不能带私钥 d")
	}

	// 载荷
	pb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var pl struct {
		Htm string `json:"htm"`
		Htu string `json:"htu"`
		Iat int64  `json:"iat"`
		Jti string `json:"jti"`
	}
	if err := json.Unmarshal(pb, &pl); err != nil {
		t.Fatalf("载荷不是 JSON: %v", err)
	}
	if pl.Htm != "POST" || pl.Htu != stsTokenEndpoint || pl.Iat == 0 || pl.Jti == "" {
		t.Errorf("载荷字段不全: %+v", pl)
	}

	// 签名：64 字节且可通过验签（证明是 R||S 而非 DER）
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if len(sig) != 64 {
		t.Fatalf("签名长度 = %d, want 64（R||S）", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	sv := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&priv.PublicKey, digest[:], r, sv) {
		t.Error("签名验签失败（格式或曲线不对）")
	}
}

// TestIsTerminalRefreshError 终态判定：只有 refresh_token 本身失效才算。
func TestIsTerminalRefreshError(t *testing.T) {
	cases := []struct {
		errStr, code, msg string
		want              bool
	}{
		{"invalid_grant", "", "bad", true},
		{"", "ExpiredRefreshToken", "bad", true},
		// STS5.1806 只有「已被兑换」是终态。
		{"", "STS5.1806", "invalid refresh token: 'the refresh token has been used'", true},
		{"", "STS5.1806", "invalid client id: codearts", false},
		// 非终态：一次请求层面的拒绝，不该把账号标死。
		{"", "InvalidDPoPHeader", "bad", false},
		{"", "STS5.0001", "bad", false},
		{"server_error", "", "bad", false},
	}
	for _, c := range cases {
		if got := isTerminalRefreshError(c.errStr, c.code, c.msg); got != c.want {
			t.Errorf("(%q,%q,%q) = %v, want %v", c.errStr, c.code, c.msg, got, c.want)
		}
	}
}

// TestNormalizeExpiry STS 的毫秒格式要能转成 RFC3339。
func TestNormalizeExpiry(t *testing.T) {
	if got := normalizeExpiry("2026-11-23T08:00:00.000Z"); got != "2026-11-23T08:00:00Z" {
		t.Errorf("normalizeExpiry = %q, want 2026-11-23T08:00:00Z", got)
	}
}
