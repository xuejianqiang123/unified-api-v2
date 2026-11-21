package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pool "unified-api-v2/internal/pool"
)

func withAuthKey(r *http.Request, key string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKeyAuthKey{}, &AuthKey{Key: key}))
}

func TestDoubaoSessionIDPriority(t *testing.T) {
	// 1) 显式头原样透传（面板测试台走这条）。
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(HeaderSessionID, "panel-test")
	if got := doubaoSessionID(r); got != "panel-test" {
		t.Errorf("显式会话标识 = %q, want panel-test", got)
	}

	// 2) 鉴权键派生：同一密钥稳定，且不泄露明文密钥。
	r2 := withAuthKey(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "sk-secret-abc")
	id := doubaoSessionID(r2)
	if id == "" || id == "default" {
		t.Errorf("鉴权键派生标识为空或回落默认：%q", id)
	}
	if strings.Contains(id, "sk-secret-abc") {
		t.Errorf("会话标识泄露明文密钥：%q", id)
	}
	if id2 := doubaoSessionID(r2); id2 != id {
		t.Errorf("同一密钥标识不稳定：%q vs %q", id, id2)
	}

	// 3) 不同密钥必须不同标识（不同调用方不共用豆包会话）。
	r3 := withAuthKey(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "sk-secret-xyz")
	if id3 := doubaoSessionID(r3); id3 == id {
		t.Errorf("不同密钥得到相同标识 %q", id3)
	}

	// 4) 无鉴权键、无显式头 → 固定默认标识（单用户部署）。
	if got := doubaoSessionID(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)); got != "default" {
		t.Errorf("无标识时 = %q, want default", got)
	}
}

// 转发链路必须真的把稳定标识送到豆包 provider，且跨轮不变。
func TestDoubaoForwardCarriesStableSessionID(t *testing.T) {
	var seen []string
	h := NewHandler(Config{
		Pool: pool.New(""),
		Aliases: map[string]UnifiedAlias{
			"auto": {Provider: "doubao", Model: "deepseek-v4-flash"},
		},
		DoubaoChat: func(w http.ResponseWriter, r *http.Request) {
			seen = append(seen, r.Header.Get(HeaderSessionID))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		},
	})

	for _, text := range []string{"第一条消息", "第二条消息"} {
		payload := `{"model":"auto","messages":[{"role":"user","content":"` + text + `"}]}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", text, rec.Code, rec.Body.String())
		}
	}

	if len(seen) != 2 {
		t.Fatalf("doubao provider 收到 %d 次调用，want 2", len(seen))
	}
	if seen[0] == "" {
		t.Fatal("转发未携带 X-Session-Id（豆包侧会按消息内容拆会话）")
	}
	if seen[0] != seen[1] {
		t.Errorf("跨轮会话标识不一致：%q vs %q", seen[0], seen[1])
	}
}

// 面板测试台必须与外部客户端隔离：固定自己的会话标识。
func TestPanelTestUsesOwnSessionID(t *testing.T) {
	var got string
	h := NewHandler(Config{
		Pool:         pool.New(""),
		PanelEnabled: true,
		Aliases: map[string]UnifiedAlias{
			"auto": {Provider: "doubao", Model: "deepseek-v4-flash"},
		},
		DoubaoChat: func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Get(HeaderSessionID)
			_, _ = w.Write([]byte(`{"ok":true}`))
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/panel/test",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got != "panel-test" {
		t.Fatalf("面板测试台会话标识 = %q, want panel-test", got)
	}
}
