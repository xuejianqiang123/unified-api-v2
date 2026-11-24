package server

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	loomypkg "unified-api-v2/internal/loomy"
)

// LoomyProvider 是 loomy 的进程内 provider。
//
// 与 doubao/trae 的差别：loomy 没有独立进程，也没有它自己的鉴权层——账号
// cookie 由网关直接管理（loomy/accounts.json），所以这里只做「OpenAI 请求
// → 单 content → SSE 透传」的转换。
type LoomyProvider struct {
	client *loomypkg.Client
	convs  *loomypkg.ConversationStore
	// accountsPath 指向 loomy/accounts.json，每次请求读一次以支持热更新导入。
	accountsPath string

	// mu/rr 保护轮询游标：多账号按顺序分流，避免总是压在一个号上。
	mu sync.Mutex
	rr int
}

// NewLoomyProvider 构造 provider。convsPath 为空时用默认位置。
func NewLoomyProvider(convsPath string) *LoomyProvider {
	if convsPath == "" {
		convsPath = "loomy/conversations.json"
	}
	return &LoomyProvider{
		client:       loomypkg.NewClient(""),
		convs:        loomypkg.NewConversationStore(convsPath, 6*time.Hour),
		accountsPath: loomyAccountsPath(),
	}
}

// Conversations 暴露会话存储（面板展示用）。
func (p *LoomyProvider) Conversations() *loomypkg.ConversationStore { return p.convs }

type loomyChatRequest struct {
	Model    string             `json:"model"`
	Messages []loomypkg.Message `json:"messages"`
	Stream   bool               `json:"stream"`
}

// Chat 实现 /v1/chat/completions 的 loomy 分支。
func (p *LoomyProvider) Chat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "读取请求体失败: "+err.Error())
		return
	}
	var req loomyChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "缺少 model")
		return
	}
	// 带图请求：解析阶段已把 image_url part 提取进 Messages[].Images。
	// 超上限截断（上游附件与会话绑定，多图按首张的 conversationId 归并）。
	var images []*loomypkg.ImageInput
	for _, m := range req.Messages {
		images = append(images, m.Images...)
	}
	if len(images) > maxImagesPerRequest {
		images = images[:maxImagesPerRequest]
	}

	// 会话键：网关已按请求头族统一生成并注入 X-Session-Id；缺失时用首条
	// 用户消息派生，保证同一对话能续上 conversationId。
	sess := strings.TrimSpace(r.Header.Get(HeaderSessionID))
	if sess == "" {
		sess = loomyLocalSessionKey(req.Messages)
	}

	// 多账号轮询 + 单次故障转移：账号级失败（凭证失效、瞬态过载）换下一个号
	// 重试一次，而不是整轮报错。conversationId 按账号分键，换号自然开新会话。
	tried := map[string]bool{}
	var accName, cookie, conv string
	var resp *http.Response
	var openErr error
	// hadAccount 必须单独记：pickAccount 在「唯一账号已被排除」时返回空串，
	// 若拿它当「有没有账号」的判据，就会把**上游调用失败**误报成
	// 「未导入账号」——用户明明是登录着的（面板能看到账号与积分）。
	hadAccount := false
	for attempt := 0; attempt <= 1; attempt++ {
		name, ck := p.pickAccount(tried)
		if ck == "" {
			break
		}
		hadAccount = true
		accName, cookie = name, ck
		conv = p.convs.Get(sess, req.Model, accName)
		var content any = loomypkg.BuildContent(req.Messages, conv != "")
		if len(images) > 0 {
			// 图片必须与消息同会话（上游按 conversationId 挂附件）：先上传，
			// 本次请求改用上传返回的会话，并用数组形态 content 携带 file_ref。
			// 换号重试时图片属于旧账号的会话，必须整批重传。
			ups, uerr := uploadAll(r.Context(), p.client, cookie, images)
			if uerr != nil {
				openErr = fmt.Errorf("图片上传失败: %w", uerr)
				tried[accName] = true
				continue
			}
			conv = ups[0].ConversationID
			content = loomypkg.ContentParts(loomypkg.BuildContent(req.Messages, false), ups)
		}
		resp, openErr = p.client.Open(r.Context(), loomypkg.Request{
			ConversationID: conv,
			MessageID:      loomypkg.NewMessageID(),
			Model:          req.Model,
			Content:        content,
		}, cookie)
		if openErr == nil {
			break
		}
		tried[accName] = true
	}
	if !hadAccount {
		writeOpenAIError(w, http.StatusServiceUnavailable, "loomy_not_logged_in",
			"未导入 Loomy 账号：面板 → 账号 → Loomy ▾ → 导入本机 Loomy")
		return
	}
	if openErr != nil {
		// 有账号但调用失败：把上游原因原样说出来（模型 id 不对、cookie 失效、
		// 额度用尽都在这条），不要再指向「没登录」。
		writeOpenAIError(w, http.StatusBadGateway, "loomy_upstream",
			"Loomy 上游调用失败（账号 "+accName+"）："+openErr.Error())
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		p.stream(w, resp.Body, sess, req.Model, accName, conv)
		return
	}
	p.nonStream(w, resp.Body, sess, req.Model, accName, conv)
}

// stream 把上游 SSE 转成 OpenAI SSE 写给客户端。
//
// 上游本身就是 OpenAI 格式，所以只做三件事：过滤空 delta、记录
// conversationId、补 [DONE]。
func (p *LoomyProvider) stream(w http.ResponseWriter, body io.Reader, sess, model, account, conv string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "no_flusher", "响应不支持流式")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	id := "chatcmpl-" + randomID()
	created := time.Now().Unix()
	roleSent := false
	wrote := false

	writeChunk := func(delta map[string]any, finish any) {
		payload := map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model":   model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	err := loomypkg.ReadEvents(body, func(ev loomypkg.Event) error {
		if ev.ConversationID != "" && ev.ConversationID != conv {
			conv = ev.ConversationID
			p.convs.Set(sess, model, account, conv)
		}
		if !roleSent {
			writeChunk(map[string]any{"role": "assistant"}, nil)
			roleSent = true
		}
		if ev.Delta != "" {
			writeChunk(map[string]any{"content": ev.Delta}, nil)
			wrote = true
		}
		if ev.FinishReason != "" {
			writeChunk(map[string]any{}, ev.FinishReason)
		}
		return nil
	})
	if err != nil && !wrote {
		// 一个字节都没发出去时可以改用错误响应；已发就只能在流里收尾。
		writeChunk(map[string]any{"content": "[上游中断] " + err.Error()}, nil)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// nonStream 聚合所有增量，返回一个 chat.completion。
func (p *LoomyProvider) nonStream(w http.ResponseWriter, body io.Reader, sess, model, account, conv string) {
	var sb strings.Builder
	finish := "stop"
	err := loomypkg.ReadEvents(body, func(ev loomypkg.Event) error {
		if ev.ConversationID != "" && ev.ConversationID != conv {
			conv = ev.ConversationID
			p.convs.Set(sess, model, account, conv)
		}
		sb.WriteString(ev.Delta)
		if ev.FinishReason != "" {
			finish = ev.FinishReason
		}
		return nil
	})
	if err != nil && sb.Len() == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "loomy_stream", err.Error())
		return
	}
	panelJSON(w, http.StatusOK, map[string]any{
		"id":      "chatcmpl-" + randomID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": sb.String()},
			"finish_reason": finish,
		}},
	})
}

// pickAccount 轮询取一个可用账号；exclude 用于故障转移时跳过已失败的号。
// 返回空表示没有可用账号（未导入或全部被排除）。
func (p *LoomyProvider) pickAccount(exclude map[string]bool) (name, cookie string) {
	f := loadLoomyAccounts()
	if len(f.Accounts) == 0 {
		return "", ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < len(f.Accounts); i++ {
		idx := (p.rr + i) % len(f.Accounts)
		a := f.Accounts[idx]
		if exclude[a.Name] || strings.TrimSpace(a.Cookie) == "" {
			continue
		}
		p.rr = (idx + 1) % len(f.Accounts)
		return a.Name, a.Cookie
	}
	return "", ""
}

// maxImagesPerRequest 单请求图片上限。上游按 conversationId 挂附件，多图
// 归并到首张上传返回的会话；量级对齐前端常见的多图发送场景。
const maxImagesPerRequest = 4

// uploadAll 依次上传图片；任一失败整体报错（failover 换号时整批重传）。
func uploadAll(ctx context.Context, c *loomypkg.Client, cookie string, imgs []*loomypkg.ImageInput) ([]*loomypkg.UploadResult, error) {
	ups := make([]*loomypkg.UploadResult, 0, len(imgs))
	for _, img := range imgs {
		u, err := c.UploadImage(ctx, cookie, img)
		if err != nil {
			return nil, err
		}
		ups = append(ups, u)
	}
	return ups, nil
}

// loomyLocalSessionKey 在没有 X-Session-Id 时，用首条用户消息派生会话键。
func loomyLocalSessionKey(msgs []loomypkg.Message) string {
	for _, m := range msgs {
		text := m.Content.String()
		if m.Role == "user" && strings.TrimSpace(text) != "" {
			sum := sha1.Sum([]byte(text))
			return "local-" + hex.EncodeToString(sum[:8])
		}
	}
	return "local-default"
}

// randomID 生成短随机串（日志关联用）。
func randomID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
