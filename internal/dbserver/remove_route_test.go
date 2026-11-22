package dbserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRemoveAccountEndpoint 验证 dbserver 的删除路由本身可达。
func TestRemoveAccountEndpoint(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/remove",
		strings.NewReader(`{"name":"a1"}`))
	req.RemoteAddr = "127.0.0.1:0"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	t.Logf("status=%d body=%s", rec.Code, rec.Body.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("删除路由不可达：status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "a1") {
		t.Errorf("响应未提及被删账号: %s", rec.Body.String())
	}
}
