package codearts

// Credential 是一份 CodeArts 账号凭据。
//
// 与「一家一个 cookie」的 provider 不同，CodeArts 是**四件套 + 一把私钥**：
// STS 临时凭证（AK/SK/security_token，约 24h）只能靠 refresh_token 续，
// 而 refresh_token 是**一次性轮换**的（兑换即失效，新的必须立刻落盘），
// 且刷新请求必须带 PKCE 的 code_verifier 与登录时生成的 DPoP 私钥签名。
//
// 因此：只拿到 cookie 或单个 token **不足以**接入 —— 缺 code_verifier 或
// DPoP 私钥时，refresh_token 即使没过期也换不出新票（绑定在 cnf.jkt 上）。
type Credential struct {
	// Name 是账号显示名（仅用于面板与运维识别，不参与签名/鉴权）。
	// 留空时由 IdentityFromCredential 从 refresh_token 解出。
	Name            string `json:"name,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	// ExpiresAt 是 STS 凭证到期时刻（RFC3339，来自响应的 credentials.expiration）。
	ExpiresAt string `json:"expires_at,omitempty"`
	// RefreshToken 是 JWT，一次性轮换；本地 refresh 成功后必须原子落盘。
	RefreshToken string `json:"refresh_token,omitempty"`
	// CodeVerifier 是登录时的 PKCE verifier，刷新时仍要原样带上。
	CodeVerifier string `json:"code_verifier,omitempty"`
	// DpopPrivateJWK 是 ES256 私钥，与 refresh_token 的 cnf.jkt 绑定。
	DpopPrivateJWK *DpopJWK `json:"dpop_private_key_jwk,omitempty"`
	// UserName 仅旧式 ticket 凭据有；新式 OAuth 凭据为空，名字在 refresh_token 里。
	UserName string `json:"user_name,omitempty"`
}

// DpopJWK 是 ES256（P-256）密钥对的最小 JWK 表示。d 为私钥标量；
// 公钥可由 x/y 重建，故只持久化私钥形态。
type DpopJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	D   string `json:"d"`
}

// HasSTS 判断临时凭证是否齐全（三项缺一不可，缺 security_token 时上游鉴权必失败）。
func (c Credential) HasSTS() bool {
	return c.AccessKeyID != "" && c.SecretAccessKey != "" && c.SecurityToken != ""
}

// CanRefresh 判断是否具备静默续期的全部材料。
//
// 注意：这不是「账号是否有效」的判断，只是「本地材料是否够发起一次续期」。
func (c Credential) CanRefresh() bool {
	return c.RefreshToken != "" && c.CodeVerifier != "" && c.DpopPrivateJWK != nil && c.DpopPrivateJWK.D != ""
}
