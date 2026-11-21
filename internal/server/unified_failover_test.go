package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"unified-api-v2/internal/pool"
)

func TestAliasCandidates(t *testing.T) {
	cases := []struct {
		name string
		a    UnifiedAlias
		want []string
	}{
		{"single legacy", UnifiedAlias{Provider: "doubao"}, []string{"doubao"}},
		{"multi ordered dedup", UnifiedAlias{Provider: "workbuddy", Providers: []string{"doubao", "workbuddy", "doubao", ""}}, []string{"doubao", "workbuddy"}},
		{"empty", UnifiedAlias{}, []string{}},
	}
	for _, tc := range cases {
		got := tc.a.Candidates()
		if len(got) != len(tc.want) {
			t.Errorf("%s: candidates=%v want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: candidates[%d]=%s want %s", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestOrderCandidatesKeepsConfigOrder(t *testing.T) {
	h := NewHandler(Config{
		Pool:         pool.New(""),
		DoubaoHealth: func() (int, int) { return 1, 1 },
	})
	// 用户规则：配置顺序即优先级，健康状态不得重排。
	// 即使 wb 0 健康、doubao 1 健康，[workbuddy, doubao] 也必须原样返回。
	got := h.orderCandidates([]string{"workbuddy", "doubao"})
	if len(got) != 2 || got[0] != "workbuddy" || got[1] != "doubao" {
		t.Fatalf("orderCandidates = %v, want config order [workbuddy doubao]", got)
	}
	// 生产配置 [doubao, workbuddy] 同样原样保持（豆包优先）。
	got2 := h.orderCandidates([]string{"doubao", "workbuddy"})
	if len(got2) != 2 || got2[0] != "doubao" || got2[1] != "workbuddy" {
		t.Fatalf("orderCandidates = %v, want config order [doubao workbuddy]", got2)
	}
}

func failoverHandler(t *testing.T, doubaoFn, wbFn func(http.ResponseWriter, *http.Request), dbHealthy bool) *Handler {
	t.Helper()
	return NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"auto": {
				Providers: []string{"doubao", "workbuddy"},
				Failover:  true,
				Model:     "deepseek-v4-flash",
			},
		},
		DoubaoChat:    doubaoFn,
		WorkbuddyChat: wbFn,
		DoubaoHealth: func() (int, int) {
			if dbHealthy {
				return 1, 1
			}
			return 0, 1
		},
	})
}

func chatReq() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
}

func TestFailoverPrimarySuccessNoSwitch(t *testing.T) {
	wbCalled := false
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from-doubao"}}],"model":"deepseek-v4-flash"}`))
		},
		func(w http.ResponseWriter, r *http.Request) { wbCalled = true },
		true,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if wbCalled {
		t.Fatal("workbuddy should NOT be called when doubao succeeds")
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "auto" {
		t.Errorf("response model=%v, want auto (alias echoed)", resp["model"])
	}
	if !strings.Contains(rec.Body.String(), "from-doubao") {
		t.Errorf("response not from doubao: %s", rec.Body.String())
	}
}

func TestFailoverPrimaryFailureSwitches(t *testing.T) {
	wbCalled := false
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"doubao down"}}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			wbCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from-workbuddy"}}],"model":"deepseek-v4-flash"}`))
		},
		true, // doubao 健康优先作为主键（健康优选只排序，故障仍切换）
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !wbCalled {
		t.Fatal("workbuddy SHOULD be called when doubao fails")
	}
	if !strings.Contains(rec.Body.String(), "from-workbuddy") {
		t.Errorf("response not from workbuddy: %s", rec.Body.String())
	}
}

func TestFailoverAllFailReturnsLastError(t *testing.T) {
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"doubao down"}}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"wb down"}}`))
		},
		true,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want last error 502; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRewriteSSEModelAny(t *testing.T) {
	jsonBody := `{"model":"deepseek-v4-flash","choices":[]}`
	got := string(rewriteSSEModelAny([]byte(jsonBody), "auto"))
	if !strings.Contains(got, `"model":"auto"`) {
		t.Errorf("json model not rewritten: %s", got)
	}
	sse := "data: {\"model\":\"deepseek-v4-flash\",\"choices\":[]}\n\ndata: [DONE]\n"
	got = string(rewriteSSEModelAny([]byte(sse), "auto"))
	if !strings.Contains(got, `"model":"auto"`) {
		t.Errorf("sse model not rewritten: %s", got)
	}
	// 大整数精度不能被破坏
	big := `{"model":"x","n":1234567890123456789}`
	got = string(rewriteSSEModelAny([]byte(big), "auto"))
	if !strings.Contains(got, "1234567890123456789") {
		t.Errorf("big int lost: %s", got)
	}
}

func TestStreamingCaptureCommitsOnFirstByte(t *testing.T) {
	// 流式：2xx 首字节立即提交且之后不再切换 provider。
	var wbCalled bool
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// 模拟后续帧
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		},
		func(w http.ResponseWriter, r *http.Request) {
			wbCalled = true
		},
		true,
	)
	req := chatReq()
	// 打开流式
	var body map[string]any
	_ = json.Unmarshal([]byte(`{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`), &body)
	b, _ := json.Marshal(body)
	req.Body = io.NopCloser(bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if wbCalled {
		t.Fatal("workbuddy must NOT be called after doubao committed first byte")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"model":"auto"`) {
		t.Errorf("model not rewritten to alias: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Hi") {
		t.Errorf("stream content missing: %s", rec.Body.String())
	}
}

func TestStreamingCapture4xxCanSwitch(t *testing.T) {
	// 4xx 未提交，必须能切换下一候选。
	var wbCalled bool
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			wbCalled = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		},
		true,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if !wbCalled {
		t.Fatal("workbuddy SHOULD be called when doubao returns 429")
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("response not from workbuddy: %s", rec.Body.String())
	}
}
