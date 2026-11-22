package providercap

import "testing"

// TestSameModelDifferentProviders 是整张矩阵存在的理由：
// glm-5.3 在 doubao 是 300K、workbuddy 是 1M、codearts 是 1.048M。
// 若哪天有人把它「优化」成只按模型名索引，这条会立刻红。
func TestSameModelDifferentProviders(t *testing.T) {
	want := map[string]int64{
		"doubao":    300000,
		"workbuddy": 1000000,
		"codearts":  1048576,
	}
	for p, wantCtx := range want {
		c, ok := Lookup(p, "glm-5.3")
		if !ok {
			t.Errorf("%s: 应命中 glm-5.3", p)
			continue
		}
		if c.Context != wantCtx {
			t.Errorf("%s glm-5.3 context = %d, want %d", p, c.Context, wantCtx)
		}
		if !c.HasContext() {
			t.Errorf("%s: 有明确来源的长度应可参与校验", p)
		}
	}
}

// TestCaseInsensitive 上游大小写不统一（codearts 的 GLM-5.2 / glm-5.3 混用）。
func TestCaseInsensitive(t *testing.T) {
	a, ok1 := Lookup("codearts", "GLM-5.2")
	b, ok2 := Lookup("codearts", "glm-5.2")
	if !ok1 || !ok2 || a.Context != b.Context {
		t.Fatalf("大小写应等价: %+v / %+v", a, b)
	}
}

// TestTraeContextUnknown trae 只有多模态信息，长度未知 ⇒ 不得参与长度校验。
func TestTraeContextUnknown(t *testing.T) {
	c, ok := Lookup("trae", "kimi-k3")
	if !ok {
		t.Fatal("trae 的 kimi-k3 应命中（至少有多模态信息）")
	}
	if !c.ImagesKnown || !c.Images {
		t.Errorf("trae kimi-k3 应标为支持图片: %+v", c)
	}
	if c.HasContext() {
		t.Error("trae 的 context 未知，HasContext 必须为 false")
	}
}

// TestLoomyUnknown loomy 整家无数据：ok=false，调用方按未知放行。
func TestLoomyUnknown(t *testing.T) {
	if _, ok := Lookup("loomy", "GLM-5.3-Flash"); ok {
		t.Error("loomy 不应命中任何能力条目")
	}
}

// TestWorkbuddyFallsBackToStatic workbuddy 走 upstream 的静态表，不复制数据。
func TestWorkbuddyFallsBackToStatic(t *testing.T) {
	c, ok := Lookup("workbuddy", "kimi-k3")
	if !ok {
		t.Fatal("workbuddy kimi-k3 应命中静态表")
	}
	if c.Context != 1048576 {
		t.Errorf("context = %d, want 1048576", c.Context)
	}
	if c.Confidence != Static {
		t.Errorf("confidence = %v, want static", c.Confidence)
	}
}

// TestUnknownModelNotZero 未收录的模型必须 ok=false，不能返回「能力 0」。
func TestUnknownModelNotZero(t *testing.T) {
	if _, ok := Lookup("codearts", "no-such-model"); ok {
		t.Error("未收录模型应返回 ok=false")
	}
}

// TestSetDynamicOverridesStatic trae 的 context 来自它自己的 /v1/models，
// 写入后必须覆盖「静态表里的 Partial（无 context）」。
func TestSetDynamicOverridesStatic(t *testing.T) {
	before, _ := Lookup("trae", "glm-5.3")
	if before.HasContext() {
		t.Fatalf("前置条件不成立：静态表里 trae 不该有 context（%+v）", before)
	}

	SetDynamic("trae", "glm-5.3", 1000000)

	after, ok := Lookup("trae", "glm-5.3")
	if !ok {
		t.Fatal("SetDynamic 后应命中")
	}
	if after.Context != 1000000 {
		t.Errorf("context = %d, want 1000000", after.Context)
	}
	if after.Confidence != Upstream {
		t.Errorf("置信度 = %v, want upstream", after.Confidence)
	}
	if !after.HasContext() {
		t.Error("有权威来源的长度应可参与 require 校验")
	}
}

// TestSetDynamicIgnoresZero 0/负数不写入 —— 写了会变成「窗口为零」的错误结论。
func TestSetDynamicIgnoresZero(t *testing.T) {
	SetDynamic("trae", "some-model", 0)
	if c, ok := Lookup("trae", "some-model"); ok && c.HasContext() {
		t.Errorf("0 不应被当成有效窗口: %+v", c)
	}
}
