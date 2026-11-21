package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unified-api-v2/internal/pool"
	"unified-api-v2/internal/upstream"
)

func TestUnifiedModelsExposesAliasesOnly(t *testing.T) {
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"smart": {Provider: "doubao", Model: "deepseek-v4-flash", ContextLength: 128000},
			"kimi":  {Provider: "doubao", Model: "kimi-k3.1-pro"},
			"wb":    {Provider: "workbuddy", Model: "smart"},
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Data) != 3 {
		t.Fatalf("data len = %d, want 3", len(got.Data))
	}
	// 排序稳定：kimi / smart / wb
	wantIDs := []string{"kimi", "smart", "wb"}
	for i, want := range wantIDs {
		if got.Data[i]["id"] != want {
			t.Errorf("data[%d].id = %v, want %s", i, got.Data[i]["id"], want)
		}
	}
	if got.Data[1]["owned_by"] != "doubao" {
		t.Errorf("smart.owned_by = %v", got.Data[1]["owned_by"])
	}
	if got.Data[2]["owned_by"] != "workbuddy" {
		t.Errorf("wb.owned_by = %v", got.Data[2]["owned_by"])
	}
	if got.Data[1]["context_length"] != float64(128000) {
		t.Errorf("smart.context_length = %v", got.Data[1]["context_length"])
	}
}

func TestUnifiedChatDoubaoDispatch(t *testing.T) {
	var seenModel string
	var seenBody string
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"smart": {Provider: "doubao", Model: "deepseek-v4-flash"},
		},
		DoubaoChat: func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("X-Unified-Alias"); got != "smart" {
				t.Errorf("X-Unified-Alias = %q, want smart", got)
			}
			raw, _ := io.ReadAll(r.Body)
			seenBody = string(raw)
			var req map[string]any
			_ = json.Unmarshal(raw, &req)
			seenModel, _ = req["model"].(string)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"provider":"doubao"}`))
		},
	})
	// 额外字段 + 大整数：验证 RawMessage 改写不会破坏其他参数或精度。
	payload := `{"model":"smart","stream":false,"max_tokens":1234567890123,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider":"doubao"`) {
		t.Fatalf("response not from doubao provider: %s", rec.Body.String())
	}
	if seenModel != "deepseek-v4-flash" {
		t.Fatalf("upstream model = %q, want deepseek-v4-flash", seenModel)
	}
	if !strings.Contains(seenBody, "1234567890123") {
		t.Fatalf("large integer lost precision: %s", seenBody)
	}
	if !strings.Contains(seenBody, `"content":"hi"`) {
		t.Fatalf("messages lost: %s", seenBody)
	}
}

func TestUnifiedChatUnknownAliasRejected(t *testing.T) {
	h := NewHandler(Config{
		Pool:    pool.New(""),
		Aliases: map[string]UnifiedAlias{"smart": {Provider: "doubao", Model: "deepseek-v4-flash"}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"not-registered","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown_model") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestUnifiedChatDoubaoNotWired503(t *testing.T) {
	h := NewHandler(Config{
		Pool:    pool.New(""),
		Aliases: map[string]UnifiedAlias{"smart": {Provider: "doubao", Model: "deepseek-v4-flash"}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"smart","messages":[]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "provider_unavailable") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestDispatchUnifiedAliasWorkbuddyRewrite(t *testing.T) {
	h := NewHandler(Config{
		Pool:    pool.New(""),
		Aliases: map[string]UnifiedAlias{"wb": {Provider: "workbuddy", Model: "smart"}},
	})
	body, handled, model := h.dispatchUnifiedAlias(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		[]byte(`{"model":"wb","stream":true,"messages":[]}`), "wb")
	if handled {
		t.Fatal("workbuddy alias must continue in-process, not be handled here")
	}
	if model != "smart" {
		t.Fatalf("effective model = %q, want smart", model)
	}
	if !strings.Contains(string(body), `"model":"smart"`) {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(string(body), `"stream":true`) {
		t.Fatalf("stream flag lost: %s", body)
	}
}

func TestDoubaoAdminForward(t *testing.T) {
	var gotPath string
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":[],"total":0}`))
	})
	h := NewHandler(Config{Pool: pool.New(""), APIKey: "master-key", DoubaoAdmin: fake})

	// 主密钥：应去掉 /admin/doubao 前缀后交给 doubao mux。
	req := httptest.NewRequest(http.MethodGet, "/admin/doubao/accounts", nil)
	req.Header.Set("Authorization", "Bearer master-key")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/admin/accounts" {
		t.Fatalf("forwarded path = %q, want /admin/accounts", gotPath)
	}

	// 无密钥：统一入口先拦，不能到 provider。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/admin/doubao/accounts", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", rec2.Code)
	}
}

func TestDoubaoAdminNotWiredIs404(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New("")})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/doubao/accounts", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when provider not wired", rec.Code)
	}
}

func TestAliasStreamWriterRewritesModelAndFlushes(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &aliasStreamWriter{ResponseWriter: rec, model: "flash"}
	_, _ = w.Write([]byte(`data: {"model":"deepseek-v4-flash","choices":[]}` + "\n\n"))
	w.Flush()
	got := rec.Body.String()
	if !strings.Contains(got, `"model":"flash"`) {
		t.Fatalf("model not rewritten: %s", got)
	}
	if strings.Contains(got, `"model":"deepseek-v4-flash"`) {
		t.Fatalf("upstream model leaked: %s", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("SSE frame terminator lost: %q", got)
	}
}

func TestUnifiedAliasesEmptyKeepsLegacyModels(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		Data []any `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	// 无账号/无模型探测失败时动态列表为空，但必须仍是原版响应形状（不是别名列表报错）。
	if got.Data == nil {
		t.Fatalf("data missing: %s", rec.Body.String())
	}
}
