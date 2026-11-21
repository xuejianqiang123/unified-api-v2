// activity_test.go 「实时使用 / 请求日志」数据源与端点测试。
//
// 判据设计：环形缓冲必须「有界 + 新的在前」，端点必须如实反映 pool 的选号模式与在途数，
// 模式切换端点必须拒绝非法值——三条任意一条写错都会让面板展示与实际行为不一致。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"unified-api-v2/internal/auth"
	"unified-api-v2/internal/pool"
)

func TestActivityStoreBoundedNewestFirst(t *testing.T) {
	s := newActivityStore(3)
	for i := 0; i < 5; i++ {
		s.add(activityEntry{At: time.Unix(int64(i), 0), Provider: "workbuddy", Model: "m", Status: 200})
	}
	got := s.snapshot(3)
	if len(got) != 3 {
		t.Fatalf("len(snapshot)=%d want 3（容量必须硬性生效）", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !got[i-1].At.After(got[i].At) {
			t.Fatalf("snapshot 非新的在前: [%d]=%v [%d]=%v", i-1, got[i-1].At, i, got[i].At)
		}
	}
	// 保留的应是最近 3 条（At=2,3,4），旧记录必须已被覆盖。
	if got[2].At.Unix() != 2 {
		t.Fatalf("最旧一条 = %v want At=2（覆盖策略必须是丢最旧）", got[2].At)
	}
}

func TestActivityStoreEmptyRingHasNoPhantomRows(t *testing.T) {
	s := newActivityStore(5)
	if got := s.snapshot(5); len(got) != 0 {
		t.Fatalf("空缓冲返回 %d 条，应该 0 条（不得伪造历史）", len(got))
	}
	s.add(activityEntry{At: time.Unix(10, 0), Provider: "workbuddy"})
	if got := s.snapshot(5); len(got) != 1 {
		t.Fatalf("写入 1 条后返回 %d 条，应该 1 条", len(got))
	}
}

func TestChatStatDoneRecordsActivity(t *testing.T) {
	wbActivity = newActivityStore(activityCap)
	st := &chatStat{start: time.Now(), model: "upstream-model", mode: "stream", uid: "u-12345678",
		nick: "昵称", status: 200, toks: 42, alias: "alias-x"}
	st.done()
	got := wbActivity.snapshot(1)
	if len(got) != 1 {
		t.Fatalf("done() 未记录活动：len=%d", len(got))
	}
	e := got[0]
	if e.Provider != "workbuddy" || e.Model != "upstream-model" || e.Alias != "alias-x" ||
		e.Status != 200 || e.Tokens != 42 || !e.Stream {
		t.Fatalf("记录字段不符：%+v", e)
	}
	if e.Account == "" || strings.Contains(e.Account, "u-12345678") {
		t.Fatalf("账号标签应只含 uid8，实际 %q", e.Account)
	}
}

func TestPanelActivityReportsModeAndInFlight(t *testing.T) {
	// 全局环形缓冲是包级状态：先清空，避免上一个用例写入的记录混进账号视图。
	wbActivity = newActivityStore(activityCap)
	p := pool.New("")
	p.SetSelectionMode("priority")
	p.Add(&auth.Auth{UID: "u1", Nickname: "main"})
	if !p.Acquire("u1") {
		t.Fatal("Acquire 应成功")
	}
	h := NewHandler(Config{Pool: p, PanelEnabled: true, PanelAuth: "u:p"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/panel/activity", nil)
	req.SetBasicAuth("u", "p")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		SelectionMode string         `json:"selection_mode"`
		InFlight      map[string]int `json:"in_flight"`
		Accounts      []struct {
			Account  string `json:"account"`
			InFlight int    `json:"in_flight"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out.SelectionMode != "priority" {
		t.Errorf("selection_mode=%q want priority", out.SelectionMode)
	}
	if out.InFlight["workbuddy"] != 1 {
		t.Errorf("workbuddy 在途=%d want 1（面板必须反映真实在途）", out.InFlight["workbuddy"])
	}
	if len(out.Accounts) != 1 || out.Accounts[0].InFlight != 1 {
		t.Errorf("账号视图=%+v want 单账号在途 1", out.Accounts)
	}
}

func TestPanelPoolModeSwitchAndReject(t *testing.T) {
	p := pool.New("")
	h := NewHandler(Config{Pool: p, PanelEnabled: true, PanelAuth: "u:p"})
	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/panel/pool/mode", strings.NewReader(body))
		req.SetBasicAuth("u", "p")
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(`{"mode":"priority"}`); rec.Code != http.StatusOK {
		t.Fatalf("切 priority status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := p.SelectionMode(); got != "priority" {
		t.Fatalf("pool 模式=%q want priority（端点必须真的改到 pool）", got)
	}
	if rec := do(`{"mode":"weighted"}`); rec.Code != http.StatusOK {
		t.Fatalf("切 weighted status=%d", rec.Code)
	}
	if got := p.SelectionMode(); got != "weighted" {
		t.Fatalf("pool 模式=%q want weighted", got)
	}
	if rec := do(`{"mode":"turbo"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("非法模式 status=%d want 400（不得静默接受）", rec.Code)
	}
}
