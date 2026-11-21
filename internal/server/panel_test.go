package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unified-api-v2/internal/pool"
)

func panelFakeDoubao() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/admin/status":
			_, _ = w.Write([]byte(`{"healthy":1,"total":1,"accounts":[]}`))
		case "/admin/accounts":
			_, _ = w.Write([]byte(`{"accounts":[{"name":"main","disabled":false,"cooling":false}],"healthy":1,"total":1}`))
		case "/admin/keys":
			_, _ = w.Write([]byte(`{"keys":[{"id":"k1","name":"test","key":"sk-1234567890abcdef"}],"total":1}`))
		case "/admin/checkin":
			_, _ = w.Write([]byte(`{"enabled":true,"results":[]}`))
		case "/admin/usage":
			_, _ = w.Write([]byte(`{"total":0,"rows":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}
}

func TestPanelDisabledIs404(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	for _, p := range []string{"/", "/api/panel/overview"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", p, rec.Code)
		}
	}
}

func TestPanelOverviewAggregatesProviders(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		PanelEnabled: true,
		DoubaoAdmin:  panelFakeDoubao(),
		Aliases: map[string]UnifiedAlias{
			"flash": {Provider: "workbuddy", Model: "deepseek-v4-flash"},
			"smart": {Provider: "doubao", Model: "deepseek-v4-flash"},
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/panel/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["service"] != "unified-api-v2" {
		t.Errorf("service = %v", got["service"])
	}
	if got["aliases"] != float64(2) {
		t.Errorf("aliases = %v", got["aliases"])
	}
	wb, ok := got["workbuddy"].(map[string]any)
	if !ok || wb["ok"] != true {
		t.Errorf("workbuddy = %v", got["workbuddy"])
	}
	db, ok := got["doubao"].(map[string]any)
	if !ok || db["ok"] != true {
		t.Errorf("doubao = %v", got["doubao"])
	}
}

func TestPanelAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		PanelEnabled: true,
		PanelAuth:    "admin:secret",
	})
	// 未登录 API：401，且绝不能发 WWW-Authenticate（那是浏览器原生弹窗的根源）
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/panel/settings", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("WWW-Authenticate must NOT be set (causes native browser popup)")
	}
	// 未登录访问页面：302 跳 /login
	recPage := httptest.NewRecorder()
	h.ServeHTTP(recPage, httptest.NewRequest(http.MethodGet, "/", nil))
	if recPage.Code != http.StatusFound || recPage.Header().Get("Location") != "/login" {
		t.Fatalf("page redirect = %d %q, want 302 /login", recPage.Code, recPage.Header().Get("Location"))
	}
	// 正确密码登录：返回会话 cookie
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, httptest.NewRequest(http.MethodPost, "/api/panel/login",
		strings.NewReader(`{"password":"secret"}`)))
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", loginRec.Code, loginRec.Body.String())
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != "wb2api_panel" || !cookies[0].HttpOnly {
		t.Fatalf("session cookie missing/insecure: %v", cookies)
	}
	// 带 cookie 访问 API：200
	req := httptest.NewRequest(http.MethodGet, "/api/panel/settings", nil)
	req.AddCookie(cookies[0])
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("authorized by cookie status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	// Basic Auth 兼容仍在（脚本/curl）
	reqBA := httptest.NewRequest(http.MethodGet, "/api/panel/settings", nil)
	reqBA.SetBasicAuth("admin", "secret")
	recBA := httptest.NewRecorder()
	h.ServeHTTP(recBA, reqBA)
	if recBA.Code != http.StatusOK {
		t.Fatalf("basic-auth compatibility status = %d", recBA.Code)
	}
}

func TestPanelKeysClientAccessInfo(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		PanelEnabled: true,
		APIKey:       "sk-master-abcdefgh",
		DoubaoAdmin:  panelFakeDoubao(),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/panel/keys", nil)
	req.Host = "127.0.0.1:8900"
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// 新设计：API 地址 + 统一 Key 明文下发供复制；不再暴露 provider 内部密钥。
	if !strings.Contains(body, `"api_url":"http://127.0.0.1:8900/v1"`) {
		t.Fatalf("api_url missing/wrong: %s", body)
	}
	if !strings.Contains(body, "sk-master-abcdefgh") {
		t.Fatalf("unified api_key missing: %s", body)
	}
	// 豆包 provider 内部 key 绝不能出现
	if strings.Contains(body, "sk-1234567890abcdef") {
		t.Fatalf("provider-internal key leaked: %s", body)
	}
}

func TestPanelModelsReturnsAliases(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		PanelEnabled: true,
		Aliases: map[string]UnifiedAlias{
			"flash": {Provider: "workbuddy", Model: "deepseek-v4-flash"},
			"smart": {Provider: "doubao", Model: "deepseek-v4-flash"},
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/panel/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Aliases []map[string]any `json:"aliases"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Aliases) != 2 {
		t.Fatalf("aliases len = %d", len(got.Aliases))
	}
	if got.Aliases[0]["alias"] != "flash" || got.Aliases[0]["provider"] != "workbuddy" {
		t.Errorf("aliases[0] = %v", got.Aliases[0])
	}
}

func TestPanelRootServesHTML(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), PanelEnabled: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("not HTML: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("content-type = %s", rec.Header().Get("Content-Type"))
	}
}

func TestPanelModelsReturnsProvidersForPublicAlias(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		PanelEnabled: true,
		Aliases: map[string]UnifiedAlias{
			"auto": {
				Providers: []string{"workbuddy", "doubao"},
				Failover:  true,
				Model:     "auto",
			},
			"hy": {Provider: "workbuddy", Model: "hy3"},
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/panel/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Aliases []map[string]any `json:"aliases"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Aliases) != 2 {
		t.Fatalf("aliases len = %d", len(got.Aliases))
	}
	// 按别名排序：auto 在前
	auto := got.Aliases[0]
	if auto["alias"] != "auto" {
		t.Fatalf("aliases[0]=%v want auto", auto["alias"])
	}
	ps, ok := auto["providers"].([]any)
	if !ok || len(ps) != 2 || ps[0] != "workbuddy" || ps[1] != "doubao" {
		t.Fatalf("auto.providers=%v want [workbuddy doubao]", auto["providers"])
	}
	if auto["failover"] != true {
		t.Errorf("auto.failover=%v want true", auto["failover"])
	}
	// 单 provider 别名：providers 数组也应给出单值
	hy := got.Aliases[1]
	ps2, ok := hy["providers"].([]any)
	if !ok || len(ps2) != 1 || ps2[0] != "workbuddy" {
		t.Fatalf("hy.providers=%v want [workbuddy]", hy["providers"])
	}
}

func TestPanelModelsSortsAutoFirst(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		PanelEnabled: true,
		Aliases: map[string]UnifiedAlias{
			"zebra":   {Provider: "workbuddy", Model: "z"},
			"alpha":   {Provider: "doubao", Model: "a"},
			"ka":      {Providers: []string{"doubao", "workbuddy"}, Failover: true, Model: "k"},
			"beta":    {Failover: false, Model: "b"},
			"aa-auto": {Providers: []string{"doubao", "workbuddy"}, Failover: true, Model: "aa"},
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/panel/models", nil))
	var got struct {
		Aliases []map[string]any `json:"aliases"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Aliases) != 5 {
		t.Fatalf("len=%d", len(got.Aliases))
	}
	// 前两个必须是自动切换（aa-auto, ka，组内字母序），后三个为独立（alpha, beta, zebra）
	wantOrder := []string{"aa-auto", "ka", "alpha", "beta", "zebra"}
	for i, want := range wantOrder {
		if got.Aliases[i]["alias"] != want {
			t.Errorf("aliases[%d]=%v want %s", i, got.Aliases[i]["alias"], want)
		}
		if got.Aliases[i]["auto_order"] != float64(i+1) {
			t.Errorf("aliases[%d].auto_order=%v want %d", i, got.Aliases[i]["auto_order"], i+1)
		}
	}
}
