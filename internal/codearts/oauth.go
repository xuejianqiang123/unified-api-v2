package codearts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// authorizeEndpoint 是 CodeArts 的授权页（从 IDE 的真实登录链接解析得出）。
//
// IDE 的实际链路是：
//
//	https://codearts.huaweicloud.com/portal/authorize?...
//	  → 302 到 auth.huaweicloud.com/authui/login.html?service=<上面的 URL>
//	  → 登录后回调 http://127.0.0.1:<port>/oauth/callback?code=...
//
// 我们直接请求 authorize 页即可，它自己会跳登录。
const authorizeEndpoint = "https://codearts.huaweicloud.com/portal/authorize"

// 授权页要求的参数名与 IDE 一致（注意是 SHA-256，不是 OAuth 常见的 S256）。
const codeChallengeMethod = "SHA-256"

// PkcePair 一次登录用的 PKCE 材料。
type PkcePair struct {
	Verifier  string
	Challenge string
}

// NewPkcePair 生成 PKCE：verifier 48 字节 base64url，challenge = SHA-256 的 base64url。
func NewPkcePair() (PkcePair, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return PkcePair{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return PkcePair{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}

// loginEntry 是华为统一登录页（IDE 的真实链接从这里进入，带 service 参数）。
const loginEntry = "https://auth.huaweicloud.com/authui/login.html"

// AuthorizeURL 拼出用户该访问的**登录入口**链接。
//
// 注意不能直接给 authorize 端点：它返回的是一个靠 JS 跳转的页面（实测 HTTP 200
// 无 Location），用户点开看不到登录框。IDE 的做法是把 authorize 整个塞进
// authui/login.html 的 service 参数里，这里照做。
//
// port 是**本地回调端口**：登录完成后浏览器会请求 http://127.0.0.1:<port>/oauth/callback。
func AuthorizeURL(challenge string, port int) string {
	return loginEntry + "?service=" + url.QueryEscape(authorizeServiceURL(challenge, port)) + "#/login"
}

// authorizeServiceURL 是 service 参数里那份 CodeArts 授权地址。
func authorizeServiceURL(challenge string, port int) string {
	q := url.Values{
		"client_id":             {oauthClientID},
		"uri_scheme":            {"codearts"},
		"port":                  {fmt.Sprint(port)},
		"code_challenge":        {challenge},
		"code_challenge_method": {codeChallengeMethod},
		"theme":                 {"2"},
		"locale":                {"zh-cn"},
	}
	return authorizeEndpoint + "?" + q.Encode()
}

// RedirectURI 与授权页里 port 参数对应的回调地址。
func RedirectURI(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", port)
}

// ExchangeCode 用授权码换凭据（PKCE + DPoP），返回可落盘的 Credential。
func (c *Client) ExchangeCode(ctx context.Context, code, verifier string, port int, jwk *DpopJWK) (Credential, error) {
	dpop, err := signDPoP(jwk, http.MethodPost, stsTokenEndpoint)
	if err != nil {
		return Credential{}, err
	}
	form := url.Values{
		"client_id":     {oauthClientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {RedirectURI(port)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, stsTokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", dpop)

	resp, err := c.http.Do(req)
	if err != nil {
		return Credential{}, fmt.Errorf("codearts: 换码请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return Credential{}, fmt.Errorf("codearts: 换码响应不是 JSON（HTTP %d）", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tr.Credentials.SecurityToken == "" {
		msg := tr.ErrorMsg
		if msg == "" {
			msg = tr.Error
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return Credential{}, fmt.Errorf("codearts: 换码失败: %s (%s)", msg, tr.ErrorCode)
	}

	cred := Credential{
		AccessKeyID:     tr.Credentials.AccessKeyID,
		SecretAccessKey: tr.Credentials.SecretAccessKey,
		SecurityToken:   tr.Credentials.SecurityToken,
		RefreshToken:    tr.RefreshToken,
		CodeVerifier:    verifier,
		DpopPrivateJWK:  jwk,
	}
	if exp := tr.Credentials.Expiration; exp != "" {
		cred.ExpiresAt = normalizeExpiry(exp)
	}
	if cred.Name == "" {
		cred.Name = credentialName(cred)
	}
	return cred, nil
}

// GenerateDpopKey 生成一把新的 P-256 DPoP 密钥（登录时用一次即持久化）。
func GenerateDpopKey() (*DpopJWK, error) {
	priv, err := ecdsaGenerate()
	if err != nil {
		return nil, err
	}
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return &DpopJWK{
		Kty: "EC", Crv: "P-256",
		X: b64(priv.X.Bytes()),
		Y: b64(priv.Y.Bytes()),
		D: b64(priv.D.Bytes()),
	}, nil
}
