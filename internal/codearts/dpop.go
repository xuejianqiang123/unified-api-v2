package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// 接管模式用到的常量（来自 IDE 的 product.json，与官方客户端一致）。
const (
	// STS 端点：华为云统一身份服务的 token 接口（CN 区）。
	stsTokenEndpoint = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	// oauthClientID 是 CodeArts Agent 的 OAuth client_id（即其 URI scheme）。
	oauthClientID = "codearts"
)

// signDPoP 签发一枚 dpop+jwt（ES256），用于向 STS 证明「我持有这把私钥」。
//
// JWT 结构严格照官方客户端：
//
//	header  {alg:"ES256", typ:"dpop+jwt", jwk:{kty,crv,x,y}}
//	payload {htm, htu, iat, jti}
//
// 注意签名结果必须是 **R||S 各 32 字节**（JWS 规定的原始形式），
// 不是 ASN.1 DER —— 用错格式会得到一个看不出原因 401。
func signDPoP(jwk *DpopJWK, htm, htu string) (string, error) {
	priv, err := ecdsaKeyFromJWK(jwk)
	if err != nil {
		return "", err
	}

	header := map[string]any{
		"alg": "ES256",
		"typ": "dpop+jwt",
		"jwk": map[string]string{
			"kty": "EC",
			"crv": "P-256",
			"x":   jwk.X,
			"y":   jwk.Y,
		},
	}
	jti, err := randomHex(32)
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"htm": htm,
		"htu": htu,
		"iat": time.Now().Unix(),
		"jti": jti,
	}

	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) + "." +
		base64.RawURLEncoding.EncodeToString(pb)

	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return "", fmt.Errorf("codearts: DPoP 签名失败: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// ecdsaKeyFromJWK 从 JWK 还原 P-256 私钥。
func ecdsaKeyFromJWK(jwk *DpopJWK) (*ecdsa.PrivateKey, error) {
	if jwk == nil {
		return nil, errors.New("codearts: 缺少 DPoP 私钥（无法续期）")
	}
	if jwk.Kty != "" && jwk.Kty != "EC" {
		return nil, fmt.Errorf("codearts: DPoP 密钥类型不支持: %s", jwk.Kty)
	}
	if jwk.Crv != "" && jwk.Crv != "P-256" {
		return nil, fmt.Errorf("codearts: DPoP 曲线不支持: %s", jwk.Crv)
	}
	x, err := b64ToInt(jwk.X)
	if err != nil {
		return nil, fmt.Errorf("codearts: DPoP.jwk.x 非法: %w", err)
	}
	y, err := b64ToInt(jwk.Y)
	if err != nil {
		return nil, fmt.Errorf("codearts: DPoP.jwk.y 非法: %w", err)
	}
	d, err := b64ToInt(jwk.D)
	if err != nil {
		return nil, fmt.Errorf("codearts: DPoP.jwk.d 非法: %w", err)
	}
	curve := elliptic.P256()
	if !curve.IsOnCurve(x, y) {
		return nil, errors.New("codearts: DPoP 公钥不在 P-256 曲线上")
	}
	return &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         d,
	}, nil
}

// b64ToInt 解 base64url（容忍缺失的 padding）。
func b64ToInt(s string) (*big.Int, error) {
	if s == "" {
		return nil, errors.New("空值")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// 部分实现会带 padding，退回标准编码再试一次。
		raw, err = base64.URLEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
	}
	return new(big.Int).SetBytes(raw), nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", buf), nil
}
