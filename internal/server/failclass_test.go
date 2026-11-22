package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestClassifyFailure 分类判据：只有「请求本身有问题」与「内容拒答」不值得切。
func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		want      FailClass
		wantRetry bool
	}{
		{"5xx 瞬态", 503, `{"error":{"message":"down"}}`, FailTransient, true},
		{"429 限流", 429, `{"error":{"message":"slow down"}}`, FailRateLimit, true},
		{"401 认证", 401, `{"error":{"message":"bad key"}}`, FailAuth, true},
		{"404 模型不存在", 404, `{"error":{"message":"no model"}}`, FailModelUnavailable, true},
		// 下面两条是 P1 的核心：不该切。
		{"参数错误不切", 400, `{"error":{"code":"invalid_request","message":"bad param"}}`, FailBadRequest, false},
		{"内容拒答不切", 400, `{"error":{"type":"content_filter","message":"blocked"}}`, FailRefused, false},
		// adapter 归一化后的业务码。
		{"模型未开通要切", 403, `{"error":{"code":"model_not_enabled"}}`, FailModelUnavailable, true},
		{"并发上限要切", 429, `{"error":{"code":"upstream_concurrency_limit"}}`, FailRateLimit, true},
		{"图片不支持要切", 400, `{"error":{"code":"image_not_supported"}}`, FailUnsupportedInput, true},
		{"验签失败", 502, `{"error":{"code":"upstream_signature_invalid"}}`, FailAuth, true},
		// 保守：400 但看不出原因 ⇒ 仍可切。
		{"400 原因不明仍切", 400, `{"error":{"message":"something odd"}}`, FailTransient, true},
	}
	for _, c := range cases {
		got := classifyFailure(c.status, []byte(c.body))
		if got != c.want {
			t.Errorf("%s: classify = %s, want %s", c.name, got, c.want)
		}
		if got.Retryable() != c.wantRetry {
			t.Errorf("%s: Retryable = %v, want %v", c.name, got.Retryable(), c.wantRetry)
		}
	}
}

// TestFailoverStopsOnBadRequest 是 P1 的行为证据：参数错误只打第一家就返回，
// 不再把后面两个候选白跑一遍。
func TestFailoverStopsOnBadRequest(t *testing.T) {
	calls := 0
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"bad param"}}`))
		},
		func(w http.ResponseWriter, r *http.Request) { calls++ },
		true,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if calls != 1 {
		t.Errorf("参数错误应只调用 1 次，实际 %d 次（后面的候选被白跑）", calls)
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400（原样回放）", rec.Code)
	}
}

// TestFailoverContinuesOnModelUnavailable 模型未开通是**最该切**的场景。
func TestFailoverContinuesOnModelUnavailable(t *testing.T) {
	wbCalled := false
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"model_not_enabled","message":"not enabled"}}`))
		},
		func(w http.ResponseWriter, r *http.Request) {
			wbCalled = true
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"from-workbuddy"}}]}`))
		},
		true,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if !wbCalled {
		t.Fatal("模型未开通必须切到下一家")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestFailoverStopsOnContentFilter 内容拒答不切（换一家同样拒，且可能触发风控）。
func TestFailoverStopsOnContentFilter(t *testing.T) {
	calls := 0
	h := failoverHandler(t,
		func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"content_filter","message":"blocked by policy"}}`))
		},
		func(w http.ResponseWriter, r *http.Request) { calls++ },
		true,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq())
	if calls != 1 {
		t.Errorf("内容拒答应只调用 1 次，实际 %d 次", calls)
	}
}
