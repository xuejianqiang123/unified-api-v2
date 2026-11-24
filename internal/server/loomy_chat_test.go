package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	loomypkg "unified-api-v2/internal/loomy"
)

// tinyPNG 1x1 像素 PNG，仅用于 data URL 物化。
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestLoomyChatWithImageEndToEnd 守卫带图请求全链路：image_url part → 上传
// （multipart）→ content 换成数组形态携带 file_ref → conversationId 改用
// 上传返回的会话（2026-11-24 上游实测：附件绑定该会话，模型才看得见图）。
func TestLoomyChatWithImageEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, "loomy"), 0o755); err != nil {
		t.Fatal(err)
	}
	accounts := `{"accounts":[{"name":"local","cookie":"loomy_web_session=test-cookie"}]}`
	if err := os.WriteFile(filepath.Join(dir, "loomy", "accounts.json"), []byte(accounts), 0o600); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/workspace/chat/files", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"conversation_id":"conv-1","list":[{"file_id":"upl_1","file_name":"image.png","media_type":"image/png","size":70,"status":"ready"}]}}`))
	})
	mux.HandleFunc("POST /web/api/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ConversationID string `json:"conversationId"`
			Content        []struct {
				Type    string          `json:"type"`
				FileRef json.RawMessage `json:"file_ref"`
			} `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("chat body 解析失败: %v", err)
		}
		if body.ConversationID != "conv-1" {
			t.Errorf("conversationId = %q，期望上传返回的 conv-1", body.ConversationID)
		}
		hasText, hasRef := false, false
		for _, p := range body.Content {
			switch p.Type {
			case "text":
				hasText = true
			case "file_ref":
				hasRef = len(p.FileRef) > 0
			}
		}
		if !hasText || !hasRef {
			t.Errorf("content 应含 text 与 file_ref part，实际 %+v", body.Content)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}],\"loomy\":{\"conversation_id\":\"conv-1\"}}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"红色\"},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	p := &LoomyProvider{
		client: loomypkg.NewClient(ts.URL + "/web/api/chat/completions"),
		convs:  loomypkg.NewConversationStore(filepath.Join(dir, "convs.json"), time.Hour),
	}
	body := `{"model":"GLM-5.3-Flash","stream":false,"messages":[{"role":"user","content":[
		{"type":"text","text":"图中什么颜色"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + tinyPNG + `"}}
	]}]}`
	rec := httptest.NewRecorder()
	p.Chat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "红色") {
		t.Errorf("响应应含模型回复，实际: %s", rec.Body.String())
	}
}

// TestLoomyChatAcceptsArrayContent 守卫：纯文本数组形态 content 不再 400。
// 此前 Message.Content 定义为 string，CodeBuddy/Claude Code 等客户端的数组
// 形态会直接解析失败（400 请求体不是合法 JSON）。
func TestLoomyChatAcceptsArrayContent(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := NewLoomyProvider(filepath.Join(dir, "convs.json"))
	body := `{"model":"GLM-5.3-Flash","messages":[{"role":"user","content":[{"type":"text","text":"只回复：可用"}]}]}`
	rec := httptest.NewRecorder()
	p.Chat(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "请求体不是合法 JSON") {
		t.Fatalf("数组形态 content 不应再判为非法 JSON: %s", rec.Body.String())
	}
}
