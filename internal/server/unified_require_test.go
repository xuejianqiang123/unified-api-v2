package server

import (
	"net/http"
	"testing"

	"unified-api-v2/internal/pool"
)

// TestFilterByRequireSkipsSmallWindow 能力下限的核心用例：
// glm-5.3 在 doubao 只有 300K、在 codearts 有 1.048M。声明需要 800K 时，
// doubao 必须在**切之前**就被跳过，而不是等上游 400。
func TestFilterByRequireSkipsSmallWindow(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		DoubaoChat:   func(http.ResponseWriter, *http.Request) {},
		CodeArtsChat: func(http.ResponseWriter, *http.Request) {},
	})
	alias := UnifiedAlias{
		Providers: []string{"doubao", "codearts"},
		Model:     "glm-5.3",
		Require:   &AliasRequire{Context: 800000},
	}
	got := h.filterByRequire(alias.Candidates(), alias)
	if len(got) != 1 || got[0] != "codearts" {
		t.Fatalf("应只剩 codearts（1.048M ≥ 800K），实际 %v", got)
	}
}

// TestFilterByRequireUnknownNotBlocked 未知能力不得阻断（loomy 整家无数据）。
func TestFilterByRequireUnknownNotBlocked(t *testing.T) {
	h := NewHandler(Config{
		Pool:      pool.New(""),
		LoomyChat: func(http.ResponseWriter, *http.Request) {},
	})
	alias := UnifiedAlias{
		Providers: []string{"loomy"},
		Model:     "GLM-5.3-Flash",
		Require:   &AliasRequire{Context: 1000000},
	}
	got := h.filterByRequire(alias.Candidates(), alias)
	if len(got) != 1 || got[0] != "loomy" {
		t.Fatalf("未知能力应放行，实际 %v", got)
	}
}

// TestFilterByRequireFallsBackWhenAllSkipped 全被跳过时回退，不能让用户没有候选。
func TestFilterByRequireFallsBackWhenAllSkipped(t *testing.T) {
	h := NewHandler(Config{
		Pool:       pool.New(""),
		DoubaoChat: func(http.ResponseWriter, *http.Request) {},
	})
	alias := UnifiedAlias{
		Providers: []string{"doubao"},
		Model:     "glm-5.3",
		Require:   &AliasRequire{Context: 99999999},
	}
	got := h.filterByRequire(alias.Candidates(), alias)
	if len(got) != 1 || got[0] != "doubao" {
		t.Fatalf("全被跳过应回退原候选，实际 %v", got)
	}
}
