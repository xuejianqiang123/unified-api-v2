package server

import (
	"testing"

	"unified-api-v2/internal/pool"
)

// TestReloadAliasesDiff 热载要能说清「变了什么」，否则用户只能靠猜。
func TestReloadAliasesDiff(t *testing.T) {
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"keep":     {Provider: "doubao", Model: "m1"},
			"change":   {Provider: "doubao", Model: "m2"},
			"removeMe": {Provider: "doubao", Model: "m3"},
		},
	})

	added, removed, changed := h.ReloadAliases(map[string]UnifiedAlias{
		"keep":   {Provider: "doubao", Model: "m1"},
		"change": {Provider: "doubao", Model: "m2-new"},
		"newOne": {Provider: "codearts", Model: "glm-5.3"},
	})
	if added != 1 || removed != 1 || changed != 1 {
		t.Fatalf("diff = (+%d -%d ~%d), want (+1 -1 ~1)", added, removed, changed)
	}

	got := h.Aliases()
	if _, ok := got["removeMe"]; ok {
		t.Error("已删除的别名不该还在表里")
	}
	if a, ok := got["newOne"]; !ok || a.Model != "glm-5.3" {
		t.Errorf("新增别名未生效: %+v", a)
	}
	if a := got["change"]; a.Model != "m2-new" {
		t.Errorf("变更未生效: %+v", a)
	}
}

// TestReloadAliasesIsolatesCallerMap 存进去的必须是副本：
// 调用方（main 的轮询）之后可能复用或改写自己的 map。
func TestReloadAliasesIsolatesCallerMap(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	next := map[string]UnifiedAlias{"a": {Provider: "doubao", Model: "m1"}}
	h.ReloadAliases(next)
	next["a"] = UnifiedAlias{Provider: "doubao", Model: "hacked"}
	if a := h.Aliases()["a"]; a.Model != "m1" {
		t.Errorf("外部 map 的修改不应影响已生效的别名表，实际 %q", a.Model)
	}
}
