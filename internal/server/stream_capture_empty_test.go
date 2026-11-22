package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEmptyCompletion 锁住「空壳」判据：正常的 chat/completions 响应一定有
// 至少一个 choice，choices 为空说明上游其实没给出结果。
func TestEmptyCompletion(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"空 body", "", true},
		{"只有空白", "  \n ", true},
		{"choices 为空数组", `{"choices":[],"usage":{"total_tokens":0}}`, true},
		{"没有 choices 字段", `{"id":"x","object":"chat.completion"}`, true},
		{"正常响应", `{"choices":[{"message":{"content":"ok"}}]}`, false},
		{"choices 里是空对象", `{"choices":[{}]}`, false},
		{"非 JSON 不判空壳", `data:{"choices":[]}`, false},
		{"有 data 字段（models 形态）", `{"data":[]}`, false},
	}
	for _, c := range cases {
		if got := isEmptyCompletion([]byte(c.body)); got != c.want {
			t.Errorf("%s: isEmptyCompletion(%q) = %v, want %v", c.name, c.body, got, c.want)
		}
	}
}

// TestSSECommitsOnFirstByte SSE 必须首字节就提交，否则实时流式退化成攒完再发。
func TestSSECommitsOnFirstByte(t *testing.T) {
	rec := httptest.NewRecorder()
	c := newStreamCaptureWriter(rec, "alias")
	c.Header().Set("Content-Type", "text/event-stream")
	c.WriteHeader(200)
	if !c.committed {
		t.Fatal("SSE 的 2xx 应立即提交")
	}
	if !c.successful() {
		t.Error("已提交的流一定算成功")
	}
}

// TestJSONDeferredForEmptyCheck 明确是 JSON 的 2xx 不能立刻提交 ——
// 否则收到空壳时已经没有机会切下一个候选。
func TestJSONDeferredForEmptyCheck(t *testing.T) {
	rec := httptest.NewRecorder()
	c := newStreamCaptureWriter(rec, "alias")
	c.Header().Set("Content-Type", "application/json")
	c.WriteHeader(200)
	if c.committed {
		t.Fatal("JSON 的 2xx 不应立即提交")
	}
	c.Write([]byte(`{"choices":[],"usage":{"total_tokens":0}}`))
	if c.successful() {
		t.Error("空壳必须判失败，否则 failover 被短路")
	}

	c2 := newStreamCaptureWriter(httptest.NewRecorder(), "alias")
	c2.Header().Set("Content-Type", "application/json")
	c2.WriteHeader(200)
	c2.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	if !c2.successful() {
		t.Error("有 choices 的响应应判成功")
	}
	if c2.committed {
		// successful 只做判断，不负责写出；写出由调用方 fallbackReplay 完成。
		t.Log("提示：successful 不应产生提交副作用")
	}
}

// TestNoContentTypeKeepsLegacy 没设 Content-Type 的 provider 保持旧行为
// （首字节即提交）—— 不能因为猜不出类型就把流式卡住。
func TestNoContentTypeKeepsLegacy(t *testing.T) {
	rec := httptest.NewRecorder()
	c := newStreamCaptureWriter(rec, "alias")
	if c.deferCommit() {
		t.Fatal("未设 Content-Type 时不应延迟提交")
	}
	c.WriteHeader(200)
	if !c.committed {
		t.Error("无 Content-Type 的 2xx 应立即提交（保持旧行为）")
	}
}

// TestFlushDoesNotCommitFailure 是 2026-11-22 实测漏洞的回归用例：
// provider 写完 400 后调 Flush（trae 就是这样），不能因此被标记为「已提交」——
// 否则 successful() 恒 true，failover 不再切下一家。
func TestFlushDoesNotCommitFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	c := newStreamCaptureWriter(rec, "alias")
	c.Header().Set("Content-Type", "application/json")
	c.WriteHeader(http.StatusBadRequest)
	_, _ = c.Write([]byte(`{"error":{"code":"invalid_request"}}`))
	c.Flush() // 关键：代理实现常见的收尾动作

	if c.committed {
		t.Fatal("4xx 不应被 Flush 提交（提交后 successful 恒 true，failover 失效）")
	}
	if c.successful() {
		t.Fatal("4xx 必须判失败")
	}
	if rec.Code != http.StatusOK { // httptest 默认 200；说明确实没写出
		t.Logf("downstream status = %d（应为未写出）", rec.Code)
	}
}

// TestFlushCommitsSuccess 正常 2xx 仍然要能被 Flush 提交（实时流式不能被破坏）。
func TestFlushCommitsSuccess(t *testing.T) {
	rec := httptest.NewRecorder()
	c := newStreamCaptureWriter(rec, "alias")
	c.Header().Set("Content-Type", "text/event-stream")
	c.WriteHeader(http.StatusOK)
	c.Flush()
	if !c.committed {
		t.Fatal("2xx 的 Flush 必须提交")
	}
}
