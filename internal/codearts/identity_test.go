package codearts

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// makeJWT 拼一个形如华为 STS refresh_token 的 JWT（不签真名，只拼载荷）。
// pad 控制 base64 是否带 '=' 填充 —— 实测上游两种形状并存，必须都吃。
func makeJWT(t *testing.T, profile map[string]any, pad bool) string {
	t.Helper()
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	enc := base64.RawURLEncoding
	if pad {
		enc = base64.URLEncoding
	}
	payload, err := json.Marshal(map[string]any{
		"exp":          1796232810,
		"type":         "refreshToken",
		"client_id":    "codearts-agent",
		"user_profile": enc.EncodeToString(profileJSON),
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestIdentityRootAccount(t *testing.T) {
	// 本机实测的典型形状：root 用户，urn 末段与 account_name 相同。
	profile := map[string]any{
		"account_id":             "01a19d50056f7354a3a13363256be613",
		"account_name":           "alice",
		"principal_is_root_user": true,
		"principal_urn":          "iam::01a19d50:user:alice",
	}
	got := IdentityFromCredential(Credential{RefreshToken: makeJWT(t, profile, false)})
	if got.Name != "alice" {
		t.Errorf("Name = %q, want alice", got.Name)
	}
	if got.AccountID != "01a19d50056f7354a3a13363256be613" {
		t.Errorf("AccountID = %q", got.AccountID)
	}
}

// TestIdentityRootWithDifferentNames 是最容易写错的一格：
// root 账号若两名不同，仍必须给**裸名** —— 拼 a@b 会渲染出不存在的账号名。
func TestIdentityRootWithDifferentNames(t *testing.T) {
	profile := map[string]any{
		"account_name":           "acme-corp",
		"principal_is_root_user": true,
		"principal_urn":          "iam::abc:user:alice",
	}
	if got := IdentityFromCredential(Credential{RefreshToken: makeJWT(t, profile, false)}); got.Name != "acme-corp" {
		t.Errorf("root 两名不同应取 account_name，got %q", got.Name)
	}
}

func TestIdentitySubUser(t *testing.T) {
	profile := map[string]any{
		"account_name":           "acme-corp",
		"principal_is_root_user": false,
		"principal_urn":          "iam::abc:user:bob",
	}
	if got := IdentityFromCredential(Credential{RefreshToken: makeJWT(t, profile, false)}); got.Name != "bob@acme-corp" {
		t.Errorf("子用户应为 bob@acme-corp，got %q", got.Name)
	}
}

// TestIdentityBase64Padding 两种填充形状必须解出同一个名字。
func TestIdentityBase64Padding(t *testing.T) {
	profile := map[string]any{
		"account_name":           "alice",
		"principal_is_root_user": true,
		"principal_urn":          "iam::abc:user:alice",
	}
	for _, pad := range []bool{false, true} {
		if got := IdentityFromCredential(Credential{RefreshToken: makeJWT(t, profile, pad)}); got.Name != "alice" {
			t.Errorf("pad=%v: Name = %q, want alice", pad, got.Name)
		}
	}
}

// TestIdentityFallsBackWithoutInventing 三条失败路径都必须「解不出就是空」，
// 绝不拿 AK 哈希或常量当名字 —— 那会让用户看到假账号名。
func TestIdentityFallsBackWithoutInventing(t *testing.T) {
	cases := []struct {
		name string
		c    Credential
		want string
	}{
		{"空凭据", Credential{}, ""},
		{"不是 JWT", Credential{RefreshToken: "opaque-token"}, ""},
		{"两段 JWT", Credential{RefreshToken: "a.b"}, ""},
		{"载荷无 user_profile", Credential{RefreshToken: "h." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".s"}, ""},
		{"旧 ticket 回退", Credential{UserName: "legacy-user"}, "legacy-user"},
	}
	for _, c := range cases {
		if got := IdentityFromCredential(c.c); got.Name != c.want {
			t.Errorf("%s: Name = %q, want %q", c.name, got.Name, c.want)
		}
	}
}

func TestCredentialMaterialGates(t *testing.T) {
	full := Credential{
		AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "st",
		RefreshToken: "rt", CodeVerifier: "cv",
		DpopPrivateJWK: &DpopJWK{Kty: "EC", Crv: "P-256", X: "x", Y: "y", D: "d"},
	}
	if !full.HasSTS() || !full.CanRefresh() {
		t.Fatal("完整材料应同时通过两道闸")
	}
	// 缺 DPoP 私钥 ⇒ 换不出票，即使 refresh_token 还没过期。
	noKey := full
	noKey.DpopPrivateJWK = nil
	if noKey.CanRefresh() {
		t.Error("缺 DPoP 私钥时不应判为可续期")
	}
	noVerifier := full
	noVerifier.CodeVerifier = ""
	if noVerifier.CanRefresh() {
		t.Error("缺 code_verifier 时不应判为可续期")
	}
	noST := full
	noST.SecurityToken = ""
	if noST.HasSTS() {
		t.Error("缺 security_token 时 STS 不完整")
	}
}
