package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unified-api-v2/internal/pool"
)

// TestPanelLoomyImportLocalNoProfile 守卫：profile 不存在时必须给出可操作提示，
// 而不是空指针或 200 假成功。
func TestPanelLoomyImportLocalNoProfile(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	h := NewHandler(Config{Pool: pool.New("")})
	rec := httptest.NewRecorder()
	h.panelLoomyImportLocal(rec, httptest.NewRequest(http.MethodPost, "/api/panel/loomy/import-local", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态 = %d，期望 400；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "loomy-login.ps1") {
		t.Errorf("提示里应告诉用户运行登录脚本，实际: %s", rec.Body.String())
	}
}

// TestPanelLoomyAccountsShape 守卫：账号列表不得下发 cookie 明文。
func TestPanelLoomyAccountsShape(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	rec := httptest.NewRecorder()
	h.panelLoomyAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/panel/loomy/accounts", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "loomy_web_session=") {
		t.Errorf("响应泄露了 cookie 明文: %s", body)
	}
	if !strings.Contains(body, "login_script") {
		t.Errorf("应返回登录脚本路径供前端提示，实际: %s", body)
	}
}
