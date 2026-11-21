package dbserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	doubao "unified-api-v2/internal/doubaoprotocol"
)

func parseChatReq(t *testing.T, raw string) (*doubao.ChatRequest, []byte) {
	t.Helper()
	b := []byte(raw)
	var r doubao.ChatRequest
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("unmarshal chat request: %v", err)
	}
	return &r, b
}

// 稳定会话标识下：不同消息内容必须得到同一个会话键——这是「同一会话中完成」
// 的直接判据（旧实现按首条 user 消息哈希，键会随消息变化）。
func TestConversationKeyStableBySessionID(t *testing.T) {
	r1, raw1 := parseChatReq(t, `{"model":"m","messages":[{"role":"user","content":"写一个200字的短故事"}]}`)
	r2, raw2 := parseChatReq(t, `{"model":"m","messages":[{"role":"user","content":"我刚才说的数字是多少"}]}`)

	k1 := conversationKey(r1, raw1, "client-abc123")
	k2 := conversationKey(r2, raw2, "client-abc123")
	if k1 != k2 {
		t.Fatalf("同一会话标识、不同消息得到不同键：%q vs %q（会话会被拆碎）", k1, k2)
	}
	if k1 != "s:client-abc123" {
		t.Fatalf("会话键 = %q, want s:client-abc123", k1)
	}

	// 不同调用方必须隔离，不能共用一个豆包会话。
	if k3 := conversationKey(r2, raw2, "client-other"); k3 == k1 {
		t.Fatalf("不同会话标识得到相同键 %q（跨调用方串台）", k3)
	}
}

// 客户端显式给的 conversation_id 优先级最高，不能被网关标识覆盖。
func TestConversationKeyExplicitMetadataWins(t *testing.T) {
	r, raw := parseChatReq(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-explicit"}}`)
	if got := conversationKey(r, raw, "client-abc123"); got != "c:conv-explicit" {
		t.Fatalf("会话键 = %q, want c:conv-explicit", got)
	}
}

// 无会话标识时保持原行为：按首条 user 消息哈希（不同消息不同键）。
func TestConversationKeyFallsBackToContentHash(t *testing.T) {
	r1, raw1 := parseChatReq(t, `{"model":"m","messages":[{"role":"user","content":"alpha"}]}`)
	r2, raw2 := parseChatReq(t, `{"model":"m","messages":[{"role":"user","content":"beta"}]}`)
	k1 := conversationKey(r1, raw1, "")
	k2 := conversationKey(r2, raw2, "")
	if k1 == k2 {
		t.Fatalf("无会话标识时不同消息应得到不同键，实际都是 %q", k1)
	}
	if k1[:2] != "h:" {
		t.Fatalf("回落键前缀 = %q, want h:", k1)
	}
}

// user 字段存在时优先于内容哈希（保持原行为），但低于网关注入的会话标识。
func TestConversationKeyUserFieldOrdering(t *testing.T) {
	r, raw := parseChatReq(t, `{"model":"m","user":"toby","messages":[{"role":"user","content":"hi"}]}`)
	if got := conversationKey(r, raw, ""); got != "u:toby" {
		t.Fatalf("会话键 = %q, want u:toby", got)
	}
	if got := conversationKey(r, raw, "client-abc123"); got != "s:client-abc123" {
		t.Fatalf("网关注入标识应优先于 user 字段，实际 %q", got)
	}
}

// 非流式（同步）路径同样必须落库会话确认事件：两轮不同消息、同一会话标识，
// 第二轮必须复用第一轮上游返回的 conversation_id。
func TestSyncConversationReusesAcrossTurns(t *testing.T) {
	f := newFakeDoubao(t)
	s := newTestServer(t, f.srv.URL)

	send := func(content string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"deepseek","messages":[{"role":"user","content":"`+content+`"}]}`))
		req.Header.Set("Authorization", "Bearer sk-down")
		req.Header.Set("X-Session-Id", "client-loop")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec
	}

	if rec := send("alpha"); rec.Code != http.StatusOK {
		t.Fatalf("第一轮 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := send("beta"); rec.Code != http.StatusOK {
		t.Fatalf("第二轮 status=%d body=%s", rec.Code, rec.Body.String())
	}

	f.mu.Lock()
	bodies := append([]string(nil), f.bods...)
	f.mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("上游收到 %d 次请求，want 2", len(bodies))
	}
	if !strings.Contains(bodies[0], `"need_create_conversation":true`) {
		t.Fatalf("第一轮应新建会话: %s", bodies[0][:200])
	}
	if !strings.Contains(bodies[1], `"conversation_id":"38453576668627970"`) {
		t.Fatalf("第二轮未复用 conversation_id（非流式路径丢了会话事件）: %s", bodies[1][:200])
	}
	if strings.Contains(bodies[1], `"need_create_conversation":true`) {
		t.Fatalf("第二轮不应再建会话")
	}
}
