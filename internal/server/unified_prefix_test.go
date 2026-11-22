package server

import (
	"net/http"
	"testing"

	"unified-api-v2/internal/pool"
)

// TestResolveProviderPrefixed 锁住「客户端不写 provider 前缀」这条隐式解析：
// 只命中已注册的**单 provider** 别名，拼错必须仍然落到 unknown_model。
func TestResolveProviderPrefixed(t *testing.T) {
	h := NewHandler(Config{
		Pool: pool.New(""),
		// enabledProviders 认得 codearts 才会去试这个前缀。
		CodeArtsChat: func(http.ResponseWriter, *http.Request) {},
		Aliases: map[string]UnifiedAlias{
			"codearts-pangu-2.1-pro": {Provider: "codearts", Model: "openpangu-2.1-pro"},
			"codearts-kimi-k3.1-pro": {Provider: "codearts", Model: "kimi-k3.1-pro"},
			// 多候选别名不参与隐式解析（否则会绕过自动路由）。
			"codearts-glm-5.3": {Providers: []string{"codearts", "workbuddy"}, Failover: true, Model: "glm-5.3"},
		},
	})

	cases := []struct {
		name      string
		requested string
		wantKey   string
		wantHit   bool
	}{
		{"只有 codearts 有", "pangu-2.1-pro", "codearts-pangu-2.1-pro", true},
		{"带点号的模型名", "kimi-k3.1-pro", "codearts-kimi-k3.1-pro", true},
		{"拼错不命中", "pangu-2.1", "", false},
		{"空串不命中", "", "", false},
		{"多候选别名不参与", "glm-5.3", "", false},
	}
	for _, c := range cases {
		key, _, hit := h.resolveProviderPrefixed(c.requested)
		if hit != c.wantHit || key != c.wantKey {
			t.Errorf("%s: resolve(%q) = (%q, %v), want (%q, %v)",
				c.name, c.requested, key, hit, c.wantKey, c.wantHit)
		}
	}
}
