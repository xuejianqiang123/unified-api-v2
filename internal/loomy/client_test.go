package loomy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBuildContent 守卫「首轮带指令、续轮只留提醒」的折装规则。
// Loomy 没有 role 数组，system/developer 必须折进单条 content。
func TestBuildContent(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "你是一名法律顾问"},
		{Role: "user", Content: "第一问"},
		{Role: "assistant", Content: "第一答"},
		{Role: "user", Content: "第二问"},
	}

	first := BuildContent(msgs, false)
	if !strings.Contains(first, "不要调用你自己的工具") {
		t.Errorf("首轮必须带指令头，实际: %s", first)
	}
	if !strings.Contains(first, "你是一名法律顾问") {
		t.Errorf("system 内容应作为身份补充进入首轮，实际: %s", first)
	}
	if !strings.Contains(first, "第二问") {
		t.Errorf("应取最后一条 user 消息，实际: %s", first)
	}
	if strings.Contains(first, "第一问") {
		t.Errorf("历史消息不应拼入（由 Loomy 会话自行维护），实际: %s", first)
	}

	cont := BuildContent(msgs, true)
	if strings.Contains(cont, "你是一名法律顾问") {
		t.Errorf("续轮不应重复注入 system（会与上游会话记忆冲突），实际: %s", cont)
	}
	if !strings.Contains(cont, "第二问") {
		t.Errorf("续轮仍应带当前用户消息，实际: %s", cont)
	}
}

// TestMessageContentArray 守卫 CodeBuddy/Claude Code 等客户端用数组形态传
// content 的场景（历史上会因 Content string 直接 400）。
func TestMessageContentArray(t *testing.T) {
	raw := `{"model":"GLM-5.3-Flash","messages":[
		{"role":"system","content":[{"type":"text","text":"你是法律顾问"}]},
		{"role":"user","content":[{"type":"text","text":"第一问"},{"type":"text","text":"第二问"}]}
	]}`
	var req struct {
		Model    string    `json:"model"`
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("数组形态 content 解析失败: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("消息数不对: %d", len(req.Messages))
	}
	if got := req.Messages[0].Content.String(); got != "你是法律顾问" {
		t.Errorf("system 文本提取错误: %q", got)
	}
	if got := req.Messages[1].Content.String(); got != "第一问\n第二问" {
		t.Errorf("多 part 应换行拼接，实际: %q", got)
	}
	built := BuildContent(req.Messages, false)
	if !strings.Contains(built, "你是法律顾问") || !strings.Contains(built, "第二问") {
		t.Errorf("BuildContent 应包含提取出的文本，实际: %s", built)
	}
}

// TestMessageImageParts 守卫 content 数组的 image_url 提取与 ContentParts 组装。
func TestMessageImageParts(t *testing.T) {
	raw := `{"role":"user","content":[
		{"type":"text","text":"看图"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,` + tinyPNGBase64 + `"}}
	]}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := m.Content.String(); got != "看图" {
		t.Errorf("text 提取错误: %q", got)
	}
	if len(m.Images) != 1 || m.Images[0].URL == "" {
		t.Fatalf("应提取到 1 张图片，实际 %d", len(m.Images))
	}

	ups := []*UploadResult{{
		ConversationID: "c1", FileID: "upl_1",
		FileName: "image.png", MediaType: "image/png", Size: 70,
	}}
	if got := ContentParts("纯文本", nil); got != "纯文本" {
		t.Errorf("无图时应保持字符串形态，实际 %T", got)
	}
	arr, ok := ContentParts("文本", ups).([]map[string]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("带图应产出 2 个 part，实际 %T", ContentParts("文本", ups))
	}
	if arr[0]["type"] != "text" || arr[1]["type"] != "file_ref" {
		t.Errorf("part 类型错误: %v / %v", arr[0]["type"], arr[1]["type"])
	}
}

// tinyPNGBase64 1x1 像素 PNG。
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// TestParseEvent 守卫 SSE 帧解析：内容、会话 ID、结束标记。
func TestParseEvent(t *testing.T) {
	// 实测首帧：只有 role，没有 content
	ev, err := ParseEvent(`{"choices":[{"delta":{"role":"assistant"},"finish_reason":null,"index":0}],"loomy":{"conversation_id":"46a57871-5220-44e5-ae97-0a88d410ad1c"},"model":"GLM-5.3-Flash"}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if ev.Delta != "" {
		t.Errorf("只有 role 的帧不应产出内容，实际 %q", ev.Delta)
	}
	if ev.ConversationID != "46a57871-5220-44e5-ae97-0a88d410ad1c" {
		t.Errorf("未提取到 conversation_id，实际 %q", ev.ConversationID)
	}

	// 空 delta（实测会出现 {"delta":{}}）
	ev, _ = ParseEvent(`{"choices":[{"delta":{},"finish_reason":null}],"loomy":{"conversation_id":"x"}}`)
	if ev.Delta != "" {
		t.Errorf("空 delta 不应产出内容")
	}

	// 正常内容帧
	ev, _ = ParseEvent(`{"choices":[{"delta":{"content":"可用"},"finish_reason":null}]}`)
	if ev.Delta != "可用" {
		t.Errorf("内容提取错误，实际 %q", ev.Delta)
	}

	// 结束帧
	ev, _ = ParseEvent(`[DONE]`)
	if !ev.Done {
		t.Errorf("[DONE] 未被识别为结束")
	}
}

// TestReadEvents 守卫多帧拼接与 [DONE] 终止。
func TestReadEvents(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}],"loomy":{"conversation_id":"conv-1"}}`,
		``,
		`data: {"choices":[{"delta":{"content":"你"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"好"}}],"loomy":{"conversation_id":"conv-1"}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var got strings.Builder
	var conv string
	err := ReadEvents(strings.NewReader(stream), func(ev Event) error {
		got.WriteString(ev.Delta)
		if ev.ConversationID != "" {
			conv = ev.ConversationID
		}
		return nil
	})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.String() != "你好" {
		t.Errorf("文本拼接错误，实际 %q", got.String())
	}
	if conv != "conv-1" {
		t.Errorf("会话 ID 未捕获，实际 %q", conv)
	}
}

// TestOpenRejectsNonSSE 守卫「HTTP 200 但非 SSE」必须当失败。
// 上游用 200 + JSON 报错（Athena 900000 等），不校验会得到静默空流。
func TestOpenRejectsNonSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); got != "loomy_web_session=abc" {
			t.Errorf("cookie 未透传，实际 %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":900000,"message":"Athena"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	_, err := c.Open(context.Background(), Request{MessageID: "web-1", Model: "GLM-5.3-Flash", Content: "hi"}, "loomy_web_session=abc")
	if err == nil {
		t.Fatal("200 + JSON 必须返回错误，否则前端会看到空回复")
	}
	if !IsTransient(err) {
		t.Errorf("900000/Athena 应判定为可重试，实际: %v", err)
	}
}

// TestOpenAcceptsSSE 守卫正常路径：SSE 直接放行。
func TestOpenAcceptsSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	resp, err := c.Open(context.Background(), Request{MessageID: "web-1", Model: "m", Content: "hi"}, "loomy_web_session=abc")
	if err != nil {
		t.Fatalf("SSE 应被接受: %v", err)
	}
	defer resp.Body.Close()
}
