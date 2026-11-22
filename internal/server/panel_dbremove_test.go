package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unified-api-v2/internal/pool"
)

// TestPanelDBRemoveRoutes 守卫删除端点的分发：请求必须到达 panelDBRemove 并转发给 provider。
func TestPanelDBRemoveRoutes(t *testing.T) {
	var got string
	var body string
	h := NewHandler(Config{
		Pool: pool.New(""),
		DoubaoAdmin: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Method + " " + r.URL.Path
			buf := make([]byte, 256)
			n, _ := r.Body.Read(buf)
			body = string(buf[:n])
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/panel/accounts/db/remove",
		strings.NewReader(`{"name":"main"}`))
	rec := httptest.NewRecorder()
	h.panelAPI(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d，body=%s（说明请求没到删除分支）", rec.Code, rec.Body.String())
	}
	if got != "POST /admin/accounts/remove" {
		t.Errorf("provider 收到 %q，期望 POST /admin/accounts/remove", got)
	}
	if !strings.Contains(body, "main") {
		t.Errorf("转发体丢参数: %s", body)
	}
}
