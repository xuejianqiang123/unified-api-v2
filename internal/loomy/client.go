// Package loomy 对接讯飞 Loomy Web 的聊天接口。
//
// 上游形态（读 loomy2api 源码 + 本机实测确认）：
//
//	POST https://loomy.xunfei.cn/web/api/chat/completions
//	Cookie: loomy_web_session=...
//	{"conversationId":"<会话或空>","messageId":"web-<uuid>","model":"<模型>","content":"<单条内容>"}
//
// 响应是**标准 OpenAI SSE**（choices[0].delta.content + [DONE]），只多一个
// loomy.conversation_id 扩展字段。所以代理侧几乎可以原样透传。
//
// 三个与 OpenAI 不同的硬限制：
//  1. 没有 role 数组，只有单条 content；历史由 Loomy 自己在会话里维护。
//  2. 没有 tools/tool_calls，也没有 temperature/max_tokens —— 转过去会被忽略。
//  3. conversationId 归属单账号：换账号必须开新会话，否则上下文丢失。
package loomy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultAPIURL 上游聊天补全端点。
const DefaultAPIURL = "https://loomy.xunfei.cn/web/api/chat/completions"

// Message 是 OpenAI messages 里本包关心的最小字段。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request 是上游请求体：只有四个字段。
type Request struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	Model          string `json:"model"`
	Content        string `json:"content"`
}

// Client 与上游交互。
type Client struct {
	apiURL string
	http   *http.Client
}

// NewClient 构造客户端。apiURL 为空时用官方端点。
func NewClient(apiURL string) *Client {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	return &Client{
		apiURL: apiURL,
		// 不设总时长：流式响应可能持续数分钟。首字节超时靠 Transport。
		http: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 120 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// NewMessageID 生成上游要求的 messageId 形态：web-<uuid>。
// 用 crypto/rand 自己拼，避免为一个 UUID 引入额外依赖。
func NewMessageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("web-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	h := hex.EncodeToString(b[:])
	return "web-" + h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// BuildContent 把 OpenAI 的 messages 折成 Loomy 需要的单条 content。
//
// 为什么需要「指令头」：Loomy 自带 system prompt 与 agent 工具，直接把用户原话
// 丢过去，模型会继续按 Loomy 的人设回答。loomy2api 的做法是在首轮注入一段
// 明确指令，续轮只留一行提醒（历史由 Loomy 自己维护）。
//
// 这里保持同样的结构，但不搬运它的具体文案：我们只保留「按用户要求回答、
// 不要调用自身工具」这两句约束，避免把工具/schema 文本传给上游（会让它
// 尝试调用自己的工具而挂起）。
func BuildContent(messages []Message, hasConversation bool) string {
	user := lastUserText(messages)
	if hasConversation {
		return "继续按我的要求回答，不要调用你自己的工具。\n\n【当前用户消息】\n" + user
	}
	system := systemText(messages)
	var b strings.Builder
	b.WriteString("你是助手。请严格按用户的要求回答，不要调用你自己的工具，也不要把 JSON schema 或函数定义当作要执行的任务。")
	if system != "" {
		b.WriteString("\n\n【用户设定的身份/背景】\n")
		b.WriteString(system)
	}
	b.WriteString("\n\n【当前用户消息】\n")
	b.WriteString(user)
	return b.String()
}

// lastUserText 取最后一条 user 消息；没有则退回最后一条任意消息。
func lastUserText(msgs []Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" && strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.TrimSpace(msgs[i].Content) != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// systemText 拼接 system/developer 消息，作为「身份补充」。
func systemText(msgs []Message) string {
	parts := make([]string, 0, 2)
	for _, m := range msgs {
		if (m.Role == "system" || m.Role == "developer") && strings.TrimSpace(m.Content) != "" {
			parts = append(parts, m.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

// transientMarkers 是上游用 200 + JSON 表示的「可重试」信号。
var transientMarkers = []string{"conversation_busy", "nonterminal run", "900000", "Athena"}

// Open 发起流式请求。返回的 resp.Body 由调用方关闭。
//
// 关键校验：HTTP 200 但 content-type 不是 text/event-stream 时，上游其实是在
// 用 JSON 报错（例如 Athena 900000）——必须当失败处理，否则会得到一个静默的
// 空流，前端表现为「没有任何回复」。
func (c *Client) Open(ctx context.Context, req Request, cookie string) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Origin", "https://loomy.xunfei.cn")
	httpReq.Header.Set("Referer", "https://loomy.xunfei.cn/web")
	httpReq.Header.Set("Cookie", cookie)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("请求 Loomy 失败: %w", err)
	}
	if resp.StatusCode == http.StatusOK {
		ct := resp.Header.Get("Content-Type")
		if strings.Contains(ct, "text/event-stream") {
			return resp, nil
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("Loomy 返回非 SSE（%s）: %s", ct, truncate(string(raw), 300))
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return nil, fmt.Errorf("Loomy HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
}

// IsTransient 判断错误是否值得重试（同一账号内）。
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range transientMarkers {
		if strings.Contains(msg, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// Event 是解析后的一个 SSE 事件。
type Event struct {
	// Delta 是本帧的增量文本；空串表示这帧没有内容（如仅 role 或仅 recovery 状态）。
	Delta string
	// ConversationID 从 loomy.conversation_id 提取，用于续轮。
	ConversationID string
	// FinishReason 非空表示上游结束（stop / length / tool_calls）。
	FinishReason string
	// Done 表示收到 [DONE]。
	Done bool
}

// ParseEvent 解析一条 `data:` 负载（不含 "data:" 前缀）。
//
// 实测帧里 delta 可能先出现 `{}`（只有 role）或空对象，这类返回 Delta 为空。
func ParseEvent(payload string) (Event, error) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return Event{}, nil
	}
	if payload == "[DONE]" {
		return Event{Done: true}, nil
	}
	var obj struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Loomy struct {
			ConversationID string `json:"conversation_id"`
		} `json:"loomy"`
	}
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		// 非 JSON 帧不致命：上游偶尔插入心跳/注释行。
		return Event{}, nil
	}
	ev := Event{ConversationID: obj.Loomy.ConversationID}
	if len(obj.Choices) > 0 {
		ev.Delta = obj.Choices[0].Delta.Content
		ev.FinishReason = obj.Choices[0].FinishReason
	}
	return ev, nil
}

// ReadEvents 逐帧解析 SSE，直到 [DONE] 或流结束。
//
// 用 bufio.Scanner 而不是自写行读取：SSE 行可能较长（含 usage/loomy 扩展字段），
// 这里把上限放宽到 1MB。
func ReadEvents(r io.Reader, fn func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var buf []string
	emit := func() error {
		if len(buf) == 0 {
			return nil
		}
		payload := strings.Join(buf, "\n")
		buf = buf[:0]
		ev, perr := ParseEvent(payload)
		if perr != nil {
			return perr
		}
		if ev.Done {
			return errSSEDone
		}
		return fn(ev)
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") {
			buf = append(buf, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		if strings.TrimSpace(line) == "" {
			if err := emit(); err != nil {
				if errors.Is(err, errSSEDone) {
					return nil
				}
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := emit(); err != nil && !errors.Is(err, errSSEDone) {
		return err
	}
	return nil
}

// errSSEDone 仅用于在 emit 内部提前结束循环，不对外暴露。
var errSSEDone = errors.New("sse done")

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
