package dbpool

import (
	"testing"
	"time"
)

func TestPickWithoutPriorityKeepsOriginalStrategy(t *testing.T) {
	// 零回归：未配置优先级（全 0）时行为与原实现一致。
	// 原实现的 idle 排序只在候选 >5 时触发；≤5 时按添加顺序（Deterministic 取第一个）。
	p := New(Options{Deterministic: true})
	for _, n := range []string{"a", "b", "c"} {
		p.Add(Key{Name: n, Key: n})
	}
	got := p.Pick(nil, false)
	if got == nil || got.Name() != "a" {
		t.Fatalf("<=5 candidates should pick first-added a, got %v", got)
	}

	// >5 个候选时，原 idle 策略必须仍然生效：
	// 把前 5 个都标为最近使用，第 6 个保持零值（最闲置），应选中第 6 个。
	p2 := New(Options{Deterministic: true})
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		p2.Add(Key{Name: n, Key: n})
	}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		p2.byName[n].lastUsed = time.Now()
	}
	got2 := p2.Pick(nil, false)
	if got2 == nil || got2.Name() != "f" {
		t.Fatalf(">5 candidates should pick most-idle f, got %v", got2)
	}
}

func TestPickStrictPriority(t *testing.T) {
	p := New(Options{Deterministic: true})
	// 故意乱序 Add，验证 priority 而非添加顺序决定选号
	p.Add(Key{Name: "low", Key: "low", Priority: 3})
	p.Add(Key{Name: "high", Key: "high", Priority: 1})
	p.Add(Key{Name: "mid", Key: "mid", Priority: 2})
	// 让 high 最近使用过（idle 最短），仍必须优先选 high
	p.byName["high"].lastUsed = time.Now()
	got := p.Pick(nil, false)
	if got == nil || got.Name() != "high" {
		t.Fatalf("picked %v, want high (priority 1)", got)
	}
	// 高优先级冷却中 → 顺延到 mid
	p.byName["high"].coolUntil = time.Now().Add(time.Hour)
	got = p.Pick(nil, false)
	if got == nil || got.Name() != "mid" {
		t.Fatalf("picked %v, want mid when high cooling", got)
	}
}

func TestSetPriorityOrder(t *testing.T) {
	p := New(Options{Deterministic: true})
	for _, n := range []string{"x", "y", "z"} {
		p.Add(Key{Name: n, Key: n})
	}
	ok, order := p.SetPriorityOrder([]string{"z", "x", "y"})
	if !ok || len(order) != 3 || order[0] != "z" || order[1] != "x" || order[2] != "y" {
		t.Fatalf("SetPriorityOrder = %v %v", ok, order)
	}
	if p.byName["z"].priority != 1 || p.byName["x"].priority != 2 || p.byName["y"].priority != 3 {
		t.Fatalf("priorities wrong: z=%d x=%d y=%d", p.byName["z"].priority, p.byName["x"].priority, p.byName["y"].priority)
	}
	got := p.Pick(nil, false)
	if got == nil || got.Name() != "z" {
		t.Fatalf("picked %v, want z after reorder", got)
	}
	// 非法输入（缺账号）必须拒绝且不改状态
	ok, _ = p.SetPriorityOrder([]string{"x", "y"})
	if ok {
		t.Fatal("SetPriorityOrder should reject incomplete list")
	}
	got = p.Pick(nil, false)
	if got == nil || got.Name() != "z" {
		t.Fatalf("state changed after rejected reorder: %v", got)
	}
}
