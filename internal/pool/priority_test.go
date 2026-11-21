package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"unified-api-v2/internal/auth"
)

func TestPriorityModePicksTopOrderedAccount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetSelectionMode("priority")
	for _, uid := range []string{"u1", "u2", "u3"} {
		p.Add(&auth.Auth{UID: uid})
	}
	ok, order := p.SetPriorityOrder([]string{"u3", "u1", "u2"})
	if !ok || len(order) != 3 || order[0] != "u3" {
		t.Fatalf("SetPriorityOrder ok=%v order=%v", ok, order)
	}
	for i := 0; i < 20; i++ {
		got := p.Pick("")
		if got == nil || got.UID != "u3" {
			t.Fatalf("pick[%d]=%v want u3 (priority 1)", i, got)
		}
	}

	p.Cooldown("u3", CoolHard, time.Hour, "test")
	for i := 0; i < 20; i++ {
		got := p.Pick("")
		if got == nil || got.UID != "u1" {
			t.Fatalf("after cooldown pick[%d]=%v want u1 (next priority)", i, got)
		}
	}
}

func TestPriorityModeAllowsUnrankedOnlyWhenNoRankedHealthy(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetSelectionMode("priority")
	p.Add(&auth.Auth{UID: "ranked"})
	// SetPriorityOrder 契约：必须恰好包含全部账号各一次（state.go:160）。
	// 这里只有一个账号，传入 {"ranked"} 是合法的；"unranked" 在排序之后加入，
	// 因此 priority=0（未排序）——正是本用例要覆盖的形态。
	if ok, _ := p.SetPriorityOrder([]string{"ranked"}); !ok {
		t.Fatal("SetPriorityOrder([ranked]) should be accepted (covers all accounts)")
	}
	p.Add(&auth.Auth{UID: "unranked"})
	if got := p.Pick(""); got == nil || got.UID != "ranked" {
		t.Fatalf("pick=%v want ranked account (unranked must not preempt a healthy ranked one)", got)
	}
	p.Disable("ranked", "test")
	if got := p.Pick(""); got == nil || got.UID != "unranked" {
		t.Fatalf("pick=%v want unranked fallback", got)
	}
}

func TestSetPriorityOrderRejectsInvalidInput(t *testing.T) {
	p := New("")
	for _, uid := range []string{"u1", "u2"} {
		p.Add(&auth.Auth{UID: uid})
	}
	for _, in := range [][]string{
		{"u1"},
		{"u1", "u1"},
		{"u1", "missing"},
	} {
		if ok, _ := p.SetPriorityOrder(in); ok {
			t.Errorf("SetPriorityOrder(%v) should fail", in)
		}
	}
	if got := p.Pick(""); got == nil {
		t.Fatal("accounts should remain usable after rejected order")
	}
}

func TestPriorityPersistsAcrossRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	t.Cleanup(p.Close)
	for _, uid := range []string{"u1", "u2"} {
		p.Add(&auth.Auth{UID: uid})
	}
	if ok, _ := p.SetPriorityOrder([]string{"u2", "u1"}); !ok {
		t.Fatal("SetPriorityOrder failed")
	}
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("state file empty")
	}

	p2 := New(fp)
	t.Cleanup(p2.Close)
	for _, uid := range []string{"u1", "u2"} {
		p2.Add(&auth.Auth{UID: uid})
	}
	p2.SetSelectionMode("priority")
	withNoPickGap(t)
	if got := p2.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("after restart pick=%v want u2", got)
	}
}
