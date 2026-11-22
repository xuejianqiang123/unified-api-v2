package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unified-api-v2/internal/pool"
)

func aliasReq(model string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
}

// TestUnifiedAliasProviderNotWired 别名指向未接线的 provider 时必须是明确的 503，
// 而不是静默 200 空响应。这是「加 provider 漏接一处」最常见的表现。
func TestUnifiedAliasProviderNotWired(t *testing.T) {
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"orphan": {Provider: "codearts", Model: "glm-5.3"},
		},
		// 故意不注入 CodeArtsChat
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aliasReq("orphan"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "provider_unavailable") {
		t.Errorf("应给出 provider_unavailable 码，实际: %s", rec.Body.String())
	}
}

// TestUnifiedAliasEchoesAlias 客户端写别名，响应必须回显别名（不是上游真名），
// 否则客户端会以为模型被偷偷换掉了。
func TestUnifiedAliasEchoesAlias(t *testing.T) {
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"my-glm": {Provider: "doubao", Model: "glm-5.3"},
		},
		DoubaoChat: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// 上游回的是真名，网关要改成别名
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}],"model":"glm-5.3"}`))
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aliasReq("my-glm"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"model":"my-glm"`) {
		t.Errorf("响应未回显别名: %s", body)
	}
	if strings.Contains(body, `"model":"glm-5.3"`) {
		t.Errorf("响应泄露了上游真名: %s", body)
	}
}

// TestUnifiedUnknownAliasRejected 未注册别名必须 400，不许静默路由到任何一家。
func TestUnifiedUnknownAliasRejected(t *testing.T) {
	h := NewHandler(Config{
		Pool:       pool.New(""),
		Aliases:    map[string]UnifiedAlias{"known": {Provider: "doubao", Model: "m"}},
		DoubaoChat: func(http.ResponseWriter, *http.Request) { t.Error("不应被调用") },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aliasReq("no-such-model"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestRequireSkipsSmallWindowInDispatch 验证 require 真的作用在**分发路径**上，
// 而不只是 filterByRequire 这个纯函数：doubao 窗口 300K < 需要 800K，必须不被调用。
func TestRequireSkipsSmallWindowInDispatch(t *testing.T) {
	doubaoCalled, codeartsCalled := false, false
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"need-big": {
				Providers: []string{"doubao", "codearts"},
				Failover:  true,
				Model:     "glm-5.3",
				Require:   &AliasRequire{Context: 800000},
			},
		},
		DoubaoChat: func(w http.ResponseWriter, r *http.Request) {
			doubaoCalled = true
			w.WriteHeader(http.StatusInternalServerError)
		},
		CodeArtsChat: func(w http.ResponseWriter, r *http.Request) {
			codeartsCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from-codearts"}}]}`))
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aliasReq("need-big"))

	if doubaoCalled {
		t.Error("doubao(300K) 应被 require 跳过，不该被调用")
	}
	if !codeartsCalled {
		t.Fatal("codearts(1.048M) 应被调用")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestStreamCommittedSkipsFailover 流式一旦提交就不能再切：
// HTTP 无法撤回已发字节，硬切会让客户端收到两段响应。
func TestStreamCommittedSkipsFailover(t *testing.T) {
	doubaoCalled, codeartsCalled := false, false
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"s": {Providers: []string{"doubao", "codearts"}, Failover: true, Model: "glm-5.3"},
		},
		DoubaoChat: func(w http.ResponseWriter, r *http.Request) {
			doubaoCalled = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		},
		CodeArtsChat: func(w http.ResponseWriter, r *http.Request) { codeartsCalled = true },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, aliasReq("s"))
	if !doubaoCalled {
		t.Fatal("主候选应被调用")
	}
	if codeartsCalled {
		t.Error("首字节已提交后不应再切下一家")
	}
}
