package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unified-api-v2/internal/pool"
)

// TestRemoveViaMux 走完整 mux，验证 /api/panel/accounts/db/remove 不会被 "/" 兜底吞掉。
func TestRemoveViaMux(t *testing.T) {
	var got string
	h := NewHandler(Config{
		Pool:         pool.New(""),
		PanelEnabled: true,
		PanelAuth:    "",
		DoubaoAdmin: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Method + " " + r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
	})
	for _, path := range []string{
		"/api/panel/accounts/db/import-local",
		"/api/panel/accounts/db/remove",
	} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"name":"x"}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		t.Logf("%-45s -> %d forwarded=%q body=%q", path, rec.Code, got, strings.TrimSpace(rec.Body.String()))
	}
	if !strings.Contains(got, "/admin/accounts/remove") {
		t.Fatalf("删除请求未被转发（mux 分派异常），最后转发=%q", got)
	}
}
