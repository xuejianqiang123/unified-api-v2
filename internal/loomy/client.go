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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIURL 上游聊天补全端点。
const DefaultAPIURL = "https://loomy.xunfei.cn/web/api/chat/completions"

// MessageContent 是 OpenAI messages[].content 的文本归一化结果：纯字符串形态
// 原样保存；part 数组形态取 text part 拼接。
type MessageContent string

// String 返回纯文本内容。
func (c MessageContent) String() string { return string(c) }

// ImageInput 是从 messages[].content[] 提取出来、待上传到 Loomy 的图片引用。
// 解析阶段只记 URL（http(s) 或 data:base64），物化（下载/解码）延迟到上传时。
type ImageInput struct {
	URL string
}

// Message 是 OpenAI messages 里本包关心的最小字段。
type Message struct {
	Role    string         `json:"role"`
	Content MessageContent `json:"content"`
	// Images 从 content 数组的 image_url part 提取，不参与序列化。
	Images []*ImageInput `json:"-"`
}

// UnmarshalJSON 解析 message，并把 content 的两种形态归一化：
//   - 纯字符串 → Content
//   - part 数组 → text/input_text 拼进 Content；image_url 提取进 Images
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	c := bytes.TrimSpace(raw.Content)
	if len(c) == 0 || string(c) == "null" {
		return nil
	}
	switch c[0] {
	case '"':
		var s string
		if err := json.Unmarshal(c, &s); err != nil {
			return err
		}
		m.Content = MessageContent(s)
	case '[':
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(c, &parts); err != nil {
			return err
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
			}
			if p.ImageURL != nil && strings.TrimSpace(p.ImageURL.URL) != "" {
				m.Images = append(m.Images, &ImageInput{URL: p.ImageURL.URL})
			}
		}
		m.Content = MessageContent(b.String())
	default:
		return fmt.Errorf("content 必须是字符串或 part 数组")
	}
	return nil
}

// Request 是上游请求体：只有四个字段。Content 通常是把 messages 折叠成的单条
// 字符串；带图时是 part 数组（[{type:"text",...},{type:"file_ref",...}]，
// 前端 bundle 实测形态，2026-11-24 上游实测两种形态都接受）。
type Request struct {
	ConversationID string `json:"conversationId"`
	MessageID      string `json:"messageId"`
	Model          string `json:"model"`
	Content        any    `json:"content"`
}

// DefaultUploadPath 是图片上传端点的固定路径（同一 host 下的 /api 前缀，
// 与 chat 的 /web/api 前缀不同；2026-11-24 前端抓包确认）。
const DefaultUploadPath = "/api/workspace/chat/files"

// Client 与上游交互。
type Client struct {
	apiURL    string
	uploadURL string
	http      *http.Client
}

// NewClient 构造客户端。apiURL 为空时用官方端点。上传端点复用 apiURL 的
// host（反代场景自动跟随），路径固定为 DefaultUploadPath。
func NewClient(apiURL string) *Client {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	uploadURL := apiURL
	if u, err := url.Parse(apiURL); err == nil && u.Host != "" {
		uploadURL = u.Scheme + "://" + u.Host + DefaultUploadPath
	}
	return &Client{
		apiURL: apiURL,
		uploadURL: uploadURL,
		// 不设总时长：流式响应可能持续数分钟。首字节超时靠 Transport。
		http: &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 120 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// newUUID 生成 RFC 4122 v4 UUID（上传接口的 idempotency-key 用）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// NewMessageID 生成上游要求的 messageId 形态：web-<uuid>。
// 用 crypto/rand 自己拼，避免为一个 UUID 引入额外依赖。
func NewMessageID() string { return "web-" + newUUID() }

// UploadResult 是一次成功上传的图片文件（与上传时绑定的会话）。
type UploadResult struct {
	ConversationID string
	FileID         string
	FileName       string
	MediaType      string
	Size           int64
}

// maxImageBytes 单张图片物化后的上限；超过直接拒绝，不做压缩。
const maxImageBytes = 20 << 20

// httpDown 物化 http(s) 图片的短超时客户端（与流式 chat 的 client 分开）。
var httpDown = &http.Client{Timeout: 60 * time.Second}

// browserUA 上传端点的浏览器形态 UA（该端点对非浏览器指纹拒绝）。
const browserUA = "Mozilla/5.0 (Linux; Android 16; Pixel 10) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Edg/157.0.0.0 Mobile Safari/537.36"

// materializeImage 把 image_url 的 URL 物化成字节与媒体类型。
// 支持 data:base64 与 http(s)；媒体类型按白名单归一化。
func materializeImage(rawURL string) (name, mediaType string, data []byte, err error) {
	switch {
	case strings.HasPrefix(rawURL, "data:"):
		head, payload, ok := strings.Cut(rawURL, ",")
		if !ok || !strings.Contains(head, ";base64") {
			return "", "", nil, fmt.Errorf("不支持的 data URL（只接受 base64）")
		}
		head = strings.TrimPrefix(strings.TrimPrefix(head, "data:"), ";base64")
		data, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return "", "", nil, fmt.Errorf("data URL base64 解码失败: %w", err)
		}
		mediaType = strings.TrimSpace(strings.SplitN(head, ";", 2)[0])
	case strings.HasPrefix(rawURL, "http://"), strings.HasPrefix(rawURL, "https://"):
		resp, derr := httpDown.Get(rawURL)
		if derr != nil {
			return "", "", nil, fmt.Errorf("下载图片失败: %w", derr)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", "", nil, fmt.Errorf("下载图片失败: HTTP %d", resp.StatusCode)
		}
		data, err = io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
		if err != nil {
			return "", "", nil, fmt.Errorf("读取图片失败: %w", err)
		}
		if len(data) > maxImageBytes {
			return "", "", nil, fmt.Errorf("图片超过 %d MB 上限", maxImageBytes>>20)
		}
		mediaType = strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0])
	default:
		return "", "", nil, fmt.Errorf("不支持的图片 URL 形态")
	}

	mediaType = strings.ToLower(mediaType)
	if mediaType == "image/jpg" {
		mediaType = "image/jpeg"
	}
	switch mediaType {
	case "image/png":
		name = "image.png"
	case "image/jpeg":
		name = "image.jpg"
	case "image/webp":
		name = "image.webp"
	case "image/gif":
		name = "image.gif"
	default:
		return "", "", nil, fmt.Errorf("不支持的图片类型 %q（支持 png/jpeg/webp/gif）", mediaType)
	}
	if len(data) == 0 {
		return "", "", nil, fmt.Errorf("图片内容为空")
	}
	return name, mediaType, data, nil
}

// UploadImage 把一张图片上传到 Loomy，返回 file 与它绑定的会话。
// 发消息时必须使用返回的 ConversationID，附件才对模型可见（2026-11-24 实测）。
func (c *Client) UploadImage(ctx context.Context, cookie string, img *ImageInput) (*UploadResult, error) {
	if img == nil || strings.TrimSpace(img.URL) == "" {
		return nil, fmt.Errorf("图片 URL 为空")
	}
	name, mediaType, data, err := materializeImage(img.URL)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	// 不能用 CreateFormFile：它把 part 的 Content-Type 硬编码为
	// application/octet-stream，上游按 part MIME 判白名单会直接拒绝
	// （2026-11-24 实测 400 code=100001）。这里显式携带真实图片类型。
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, name))
	h.Set("Content-Type", mediaType)
	fw, err := w.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := fw.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.uploadURL, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("idempotency-key", newUUID())
	req.Header.Set("Origin", originOf(c.uploadURL))
	req.Header.Set("Referer", originOf(c.uploadURL)+"/chat")
	// 上传端点做指纹校验：Go 默认 UA 会被判「MIME 不在白名单」（2026-11-24
	// 实测 400 code=100001），带浏览器形态 UA 后同一文件即可通过。
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "*/*")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上传请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上传失败: HTTP %d %s", resp.StatusCode, truncateBody(body))
	}
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			ConversationID string `json:"conversation_id"`
			List           []struct {
				FileID    string `json:"file_id"`
				FileName  string `json:"file_name"`
				MediaType string `json:"media_type"`
				Size      int64  `json:"size"`
				Status    string `json:"status"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("上传响应解析失败: %w", err)
	}
	if !out.Success || len(out.Data.List) == 0 {
		return nil, fmt.Errorf("上传被上游拒绝: %s", truncateBody(body))
	}
	f := out.Data.List[0]
	if !strings.EqualFold(f.Status, "ready") || f.FileID == "" {
		return nil, fmt.Errorf("上传未就绪（status=%s）", f.Status)
	}
	return &UploadResult{
		ConversationID: out.Data.ConversationID,
		FileID:         f.FileID,
		FileName:       f.FileName,
		MediaType:      f.MediaType,
		Size:           f.Size,
	}, nil
}

func originOf(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return "https://loomy.xunfei.cn"
}

func truncateBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ContentParts 组装上游 content：无图时就是单条字符串；带图时是
// [{type:"text",...},{type:"file_ref",...},...]（前端 bundle 实测形态）。
func ContentParts(text string, ups []*UploadResult) any {
	if len(ups) == 0 {
		return text
	}
	parts := []map[string]any{{"type": "text", "text": text}}
	for _, u := range ups {
		parts = append(parts, map[string]any{
			"type": "file_ref",
			"file_ref": map[string]any{
				"file_id":    u.FileID,
				"file_name":  u.FileName,
				"media_type": u.MediaType,
				"size":       u.Size,
				"file_kind":  "image",
				"status":     "ready",
			},
		})
	}
	return parts
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
		text := msgs[i].Content.String()
		if msgs[i].Role == "user" && strings.TrimSpace(text) != "" {
			return text
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		text := msgs[i].Content.String()
		if strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

// systemText 拼接 system/developer 消息，作为「身份补充」。
func systemText(msgs []Message) string {
	parts := make([]string, 0, 2)
	for _, m := range msgs {
		text := m.Content.String()
		if (m.Role == "system" || m.Role == "developer") && strings.TrimSpace(text) != "" {
			parts = append(parts, text)
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
