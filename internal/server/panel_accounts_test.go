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

func TestWBImportFlatCredential(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(Config{Pool: pool.New(""), AuthDir: dir, PanelEnabled: true})
	body := `{"access_token":"tok-abc","refresh_token":"ref-abc","expires_at":1799999999000,"uid":"uid-123","nickname":"测试","realm":"cn"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/panel/accounts/wb/import", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Added []string `json:"added"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Added) != 1 || out.Added[0] != "workbuddy-uid-123.json" {
		t.Fatalf("added=%v", out.Added)
	}
	// 校验落盘内容为嵌套格式，expires_at 毫秒转秒。
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-uid-123.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	auth := doc["auth"].(map[string]any)
	if auth["accessToken"] != "tok-abc" {
		t.Errorf("accessToken=%v", auth["accessToken"])
	}
	if auth["expiresAt"] != float64(1799999999) {
		t.Errorf("expiresAt=%v want 1799999999", auth["expiresAt"])
	}
	acct := doc["account"].(map[string]any)
	if acct["uid"] != "uid-123" {
		t.Errorf("uid=%v", acct["uid"])
	}
	// Windows 上 os.WriteFile 的 0600 会被上报为 0666（无 POSIX 权限位），
	// 与既有 auth.SaveAtomic 写入方式一致，故不在此断言权限位。
}

func TestWBImportNestedCredential(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(Config{Pool: pool.New(""), AuthDir: dir, PanelEnabled: true})
	body := `{"auth":{"accessToken":"tok-nested","refreshToken":"r","expiresAt":1800000000,"realm":"global"},"account":{"uid":"nested-uid","nickname":"N"}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/panel/accounts/wb/import", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-nested-uid.json")); err != nil {
		t.Fatalf("nested credential file not written: %v", err)
	}
}

func TestWBImportRejectsMissingToken(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(Config{Pool: pool.New(""), AuthDir: dir, PanelEnabled: true})
	body := `{"uid":"no-token"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/panel/accounts/wb/import", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestWBImportArray(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(Config{Pool: pool.New(""), AuthDir: dir, PanelEnabled: true})
	body := `[{"access_token":"t1","uid":"u1"},{"access_token":"t2","uid":"u2"}]`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/panel/accounts/wb/import", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Added []string `json:"added"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Added) != 2 {
		t.Fatalf("added=%v want 2 files", out.Added)
	}
}

func TestDBCookiePersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	os.WriteFile(path, []byte(`{"accounts":[{"name":"old","cookie":"old-cookie"}]}`), 0o600)
	h := NewHandler(Config{Pool: pool.New(""), DoubaoAccountsFile: path})
	if err := h.upsertDBCookieFile("new", "new-cookie", "备注"); err != nil {
		t.Fatal(err)
	}
	if err := h.upsertDBCookieFile("old", "updated-cookie", ""); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var f dbAccountFile
	_ = json.Unmarshal(raw, &f)
	if len(f.Accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(f.Accounts))
	}
	for _, a := range f.Accounts {
		if a.Name == "old" && a.Cookie != "updated-cookie" {
			t.Errorf("old cookie not updated: %s", a.Cookie)
		}
		if a.Name == "new" && a.Cookie != "new-cookie" {
			t.Errorf("new cookie wrong: %s", a.Cookie)
		}
	}
}
