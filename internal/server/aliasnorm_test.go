package server

import (
	"net/http"
	"testing"

	"unified-api-v2/internal/pool"
)

// TestMergeAliasVariants 用户明确要求的场景：同一个模型的不同写法要合并成
// 一条多候选别名，写哪种都能自动切换。
func TestMergeAliasVariants(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		DoubaoChat:   func(http.ResponseWriter, *http.Request) {},
		LoomyChat:    func(http.ResponseWriter, *http.Request) {},
		CodeArtsChat: func(http.ResponseWriter, *http.Request) {},
	})
	raw := map[string]UnifiedAlias{
		"glm-5.3-flash":          {Providers: []string{"doubao"}, Model: "glm-5.3-flash"},
		"GLM-5.3-Flash":          {Provider: "loomy", Model: "GLM-5.3-Flash"},
		"codearts-glm-5.3-flash": {Provider: "codearts", Model: "glm-5.3-flash"},
		// 不该被合并：版本不同。
		"deepseek-v4.1-flash": {Provider: "doubao", Model: "deepseek-v4-flash"},
	}
	got := mergeAliasVariants(raw, h.providerPrefixes())

	for _, name := range []string{"glm-5.3-flash", "GLM-5.3-Flash", "codearts-glm-5.3-flash"} {
		a, ok := got[name]
		if !ok {
			t.Fatalf("%s 应仍在表里", name)
		}
		cands := a.Candidates()
		if len(cands) != 3 {
			t.Errorf("%s 候选 = %v, want 3 家（doubao/loomy/codearts）", name, cands)
		}
		if !a.Failover {
			t.Errorf("%s 应开启 failover", name)
		}
		// 逐家模型名必须保留各自的写法。
		if m := a.ModelFor("loomy"); m != "GLM-5.3-Flash" {
			t.Errorf("loomy 的模型名 = %q, want GLM-5.3-Flash", m)
		}
		if m := a.ModelFor("doubao"); m != "glm-5.3-flash" {
			t.Errorf("doubao 的模型名 = %q, want glm-5.3-flash", m)
		}
	}

	// 版本不同不应合并。
	if a := got["deepseek-v4.1-flash"]; len(a.Candidates()) != 1 {
		t.Errorf("不同版本被误合并: %v", a.Candidates())
	}
}

// TestNormalizeAliasKey 归一规则只做小写 + 去前缀，不做模糊匹配。
func TestNormalizeAliasKey(t *testing.T) {
	prefixes := []string{"codearts-", "loomy-", "workbuddy-"}
	cases := map[string]string{
		"GLM-5.3-Flash":          "glm-5.3-flash",
		"codearts-glm-5.3-flash": "glm-5.3-flash",
		"loomy-GLM-5.3-Flash":    "glm-5.3-flash",
		"glm52":                  "glm52",
		"deepseek-v4.1-flash":    "deepseek-v4.1-flash",
	}
	for in, want := range cases {
		if got := normalizeAliasKey(in, prefixes); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
