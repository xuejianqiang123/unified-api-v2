package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"unified-api-v2/internal/pool"
)

// 设置优先序后，自动路由候选按它重排；未设置时保持配置原序。
func TestProviderOrderReordersCandidates(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	if got := h.orderCandidates([]string{"doubao", "workbuddy"}); len(got) != 2 || got[0] != "doubao" {
		t.Fatalf("未设置优先序时应保持配置原序，got=%v", got)
	}
	if _, err := h.SetProviderOrder([]string{"workbuddy", "doubao"}); err != nil {
		t.Fatalf("SetProviderOrder: %v", err)
	}
	got := h.orderCandidates([]string{"doubao", "workbuddy"})
	if got[0] != "workbuddy" || got[1] != "doubao" {
		t.Fatalf("设置 WorkBuddy 优先后应重排，got=%v", got)
	}
	if _, err := h.SetProviderOrder([]string{"doubao", "workbuddy"}); err != nil {
		t.Fatalf("SetProviderOrder: %v", err)
	}
	if got = h.orderCandidates([]string{"doubao", "workbuddy"}); got[0] != "doubao" {
		t.Fatalf("恢复豆包优先后应重排，got=%v", got)
	}
}

// 非法输入整单拒绝，不允许部分生效。
func TestProviderOrderRejectsInvalid(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	if _, err := h.SetProviderOrder([]string{"doubao", "doubao"}); err == nil {
		t.Fatal("重复项应被拒绝")
	}
	if _, err := h.SetProviderOrder([]string{"doubao", "unknown"}); err == nil {
		t.Fatal("未知 provider 应被拒绝")
	}
	if _, err := h.SetProviderOrder(nil); err == nil {
		t.Fatal("空数组应被拒绝")
	}
	if got := h.ProviderOrder(); len(got) != 0 {
		t.Fatalf("拒绝后内存不应被改动，got=%v", got)
	}
}

// 落盘后新 handler 能读回同一顺序（重启仍然生效）；损坏文件回落 nil。
func TestProviderOrderPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider_order.json")
	h := NewHandler(Config{Pool: pool.New(""), ProviderOrderFile: path})
	persisted, err := h.SetProviderOrder([]string{"workbuddy", "doubao"})
	if err != nil || !persisted {
		t.Fatalf("persisted=%v err=%v", persisted, err)
	}
	h2 := NewHandler(Config{Pool: pool.New(""), ProviderOrderFile: path})
	if got := h2.ProviderOrder(); len(got) != 2 || got[0] != "workbuddy" {
		t.Fatalf("重启后应读回优先序，got=%v", got)
	}
	if err := os.WriteFile(path, []byte("{bad json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h3 := NewHandler(Config{Pool: pool.New(""), ProviderOrderFile: path})
	if got := h3.ProviderOrder(); len(got) != 0 {
		t.Fatalf("损坏文件应回落 nil，got=%v", got)
	}
}

// 候选里出现优先序未覆盖的 provider 时不能丢，补在末尾。
func TestProviderOrderKeepsUnknownCandidate(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	if _, err := h.SetProviderOrder([]string{"workbuddy", "doubao"}); err != nil {
		t.Fatal(err)
	}
	got := h.orderCandidates([]string{"doubao", "legacy", "workbuddy"})
	want := []string{"workbuddy", "doubao", "legacy"}
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}

// 面板端点：POST 设置 / GET 读取，来源标注正确。
func TestPanelProviderOrderEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), PanelEnabled: true,
		Aliases: map[string]UnifiedAlias{
			"a": {Providers: []string{"doubao", "workbuddy"}, Failover: true, Model: "m"},
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/panel/provider-order", strings.NewReader(`{"first":"workbuddy"}`))
	h.panelAPI(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	order, _ := got["order"].([]any)
	if len(order) == 0 || order[0] != "workbuddy" {
		t.Fatalf("order=%v", order)
	}

	rec = httptest.NewRecorder()
	h.panelAPI(rec, httptest.NewRequest(http.MethodGet, "/api/panel/provider-order", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["source"] != "override" {
		t.Fatalf("GET 应标注来源 override，got=%v", got["source"])
	}

	// 非法 first 必须拒绝，且不改动已有优先序。
	rec = httptest.NewRecorder()
	h.panelAPI(rec, httptest.NewRequest(http.MethodPost, "/api/panel/provider-order", strings.NewReader(`{"first":"xxx"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 first 应 400，got code=%d", rec.Code)
	}
	if got := h.ProviderOrder(); got[0] != "workbuddy" {
		t.Fatalf("非法请求不应改动优先序，got=%v", got)
	}
}
