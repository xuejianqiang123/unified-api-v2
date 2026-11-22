package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFailoverEmptyCompletionSwitches 是「假成功」修复的集成证据：
// 主候选返回 200 + 空 choices（doubao 对不认识的模型就是这样），
// 必须判失败并切到下一候选，而不是把空壳当成功返回给客户端。
func TestFailoverEmptyCompletionSwitches(t *testing.T) {
	wbCalled := false
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[],"usage":{"completion_tokens":0,"prompt_tokens":0,"total_tokens":0}}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			wbCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from-workbuddy"}}],"model":"deepseek-v4-flash"}`))
		},
		true,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !wbCalled {
		t.Fatal("空壳响应必须触发切换，否则 failover 被短路")
	}
	if !strings.Contains(rec.Body.String(), "from-workbuddy") {
		t.Errorf("客户端应拿到后备候选的内容，实际: %s", rec.Body.String())
	}
}

// TestFailoverEmptyBodySwitches 连 body 都不写（200 后立刻返回）也要切。
func TestFailoverEmptyBodySwitches(t *testing.T) {
	wbCalled := false
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK) // 不写 body
		},
		func(w http.ResponseWriter, r *http.Request) {
			wbCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		},
		true,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if !wbCalled {
		t.Fatal("200 但无 body 也应触发切换")
	}
}
