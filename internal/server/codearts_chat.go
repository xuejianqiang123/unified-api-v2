package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	codeartspkg "unified-api-v2/internal/codearts"
)

// CodeArtsProvider 是 CodeArts 的进程内 provider。
//
// 与 loomy 的差别：CodeArts 上游是 **OpenAI 兼容形态**（chat/completions +
// SSE chunk），所以这里不需要把请求翻译成私有协议，只需：
//  1. 把 OpenAI 入参规整成上游接受的 11 个顶层字段；
//  2. 加签名（在 internal/codearts 里）；
//  3. 流式原样透传，仅把 model 改回别名。
type CodeArtsProvider struct {
	client *codeartspkg.Client
}

// NewCodeArtsProvider 构造 provider。credPath 指向凭据 JSON；为空表示不接线。
func NewCodeArtsProvider(credPath string) *CodeArtsProvider {
	return &CodeArtsProvider{client: codeartspkg.NewClient(credPath)}
}

// Client 暴露上游客户端（面板查额度 / 签到用）。
func (p *CodeArtsProvider) Client() *codeartspkg.Client { return p.client }

// AccountName 返回账号显示名（凭据里的 name，缺失时从 refresh_token 解）。
// 存不了名字时返回空串 —— 调用方回落固定标识，绝不编造。
func (p *CodeArtsProvider) AccountName() string {
	cred, err := p.client.Credential()
	if err != nil {
		return ""
	}
	if cred.Name != "" {
		return cred.Name
	}
	return codeartspkg.IdentityFromCredential(cred).Name
}

// codeArtsUpstreamBody 是上游接受的 11 个顶层字段（照 IDE 抓包形态）。
// 其余字段一律不透传：上游对未知字段的处理不保证，且 tool_stream 之类的默认值
// 与 IDE 不一致会改变行为。
type codeArtsUpstreamBody struct {
	EnableThinking *bool           `json:"enable_thinking,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Messages       json.RawMessage `json:"messages"`
	Model          string          `json:"model"`
	Stream         bool            `json:"stream,omitempty"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	ToolChoice     json.RawMessage `json:"tool_choice,omitempty"`
	ToolStream     *bool           `json:"tool_stream,omitempty"`
	Tools          json.RawMessage `json:"tools,omitempty"`
	UserPrompt     string          `json:"user_prompt,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// inboundChat 是网关收到的 OpenAI 请求里我们关心的字段。
type inboundChat struct {
	Model               string          `json:"model"`
	Messages            json.RawMessage `json:"messages"`
	Stream              bool            `json:"stream"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	Tools               json.RawMessage `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	EnableThinking      *bool           `json:"enable_thinking"`
}

// Chat 实现 /v1/chat/completions 的 codearts 分支。
func (p *CodeArtsProvider) Chat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "读取请求体失败: "+err.Error())
		return
	}
	var in inboundChat
	if err := json.Unmarshal(raw, &in); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(in.Model) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "缺少 model")
		return
	}
	if len(in.Messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "缺少 messages")
		return
	}

	up := codeArtsUpstreamBody{
		Messages:    in.Messages,
		Model:       in.Model,
		Stream:      in.Stream,
		Temperature: in.Temperature,
	}
	// max_tokens 与 max_completion_tokens 取其一；上界必须钳制（超了直接 400）。
	mt := in.MaxTokens
	if mt == 0 {
		mt = in.MaxCompletionTokens
	}
	if mt <= 0 {
		mt = codeartspkg.MaxTokensDefault
	}
	if mt > codeartspkg.MaxTokensCap {
		mt = codeartspkg.MaxTokensCap
	}
	up.MaxTokens = mt

	// 思考开关：显式 enable_thinking 优先；否则按 reasoning_effort 推断。
	thinking := false
	if in.EnableThinking != nil {
		thinking = *in.EnableThinking
	} else if in.ReasoningEffort != "" && in.ReasoningEffort != "none" {
		thinking = true
	}
	up.EnableThinking = &thinking

	if len(in.Tools) > 0 && string(in.Tools) != "null" && string(in.Tools) != "[]" {
		up.Tools = in.Tools
		if len(in.ToolChoice) > 0 {
			up.ToolChoice = in.ToolChoice
		}
		toolStream := false
		up.ToolStream = &toolStream
	}
	if in.Stream {
		// 上游只有在显式要求时才在最后一帧带 usage；网关的账本依赖它。
		up.StreamOptions = &streamOptions{IncludeUsage: true}
	}

	body, err := json.Marshal(up)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "组装上游请求失败: "+err.Error())
		return
	}

	displayModel := strings.TrimSpace(r.Header.Get("X-Unified-Alias"))
	if displayModel == "" {
		displayModel = in.Model
	}

	resp, err := p.client.Chat(r.Context(), body)
	if err != nil {
		if errors.Is(err, codeartspkg.ErrNoCredential) {
			writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable",
				"codearts 凭据不可用: "+err.Error())
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "请求 codearts 失败: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		upRaw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		status, code, msg := translateCodeArtsError(resp.StatusCode, upRaw)
		writeOpenAIError(w, status, code, msg)
		return
	}

	if in.Stream {
		p.passthroughSSE(w, resp, displayModel)
		return
	}

	// 非流式：整包读回，把 model 改回别名后写出（上游回的是真名）。
	upRaw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "读取上游响应失败: "+err.Error())
		return
	}
	out := rewriteModelJSON(unwrapDataPrefix(upRaw), displayModel)
	if _, merr := json.Marshal(out); merr != nil {
		log.Printf("[codearts] 回写序列化失败: %v | raw前200=%q", merr, string(upRaw[:min(200, len(upRaw))]))
		writeOpenAIError(w, http.StatusBadGateway, "upstream_unparsable",
			"codearts 响应无法回写: "+merr.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// passthroughSSE 逐帧透传 SSE，只把 model 字段改回别名。
func (p *CodeArtsProvider) passthroughSSE(w http.ResponseWriter, resp *http.Response, displayModel string) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	buf := make([]byte, 32<<10)
	var pending []byte
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			// 按行切，保留不完整的尾行等下一轮。
			for {
				idx := bytes.IndexByte(pending, '\n')
				if idx < 0 {
					break
				}
				line := pending[:idx+1]
				pending = pending[idx+1:]
				if _, werr := w.Write(rewriteDataLine(line, displayModel)); werr != nil {
					return
				}
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if len(pending) > 0 {
				_, _ = w.Write(rewriteDataLine(pending, displayModel))
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
	}
}

// unwrapDataPrefix 剥掉上游的 "data:" 包装。
//
// 实测：即使请求里 stream=false，上游也可能把完整 JSON 包成 "data:{...}"
// （无空格、无换行）—— 直接按 JSON 解析会得到 "invalid character 'd'"。
// 带 tools 的请求反而回纯 JSON，两种形态都要吃。
func unwrapDataPrefix(raw []byte) []byte {
	t := bytes.TrimSpace(raw)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return t
	}
	t = bytes.TrimSpace(t[len("data:"):])
	// 若仍是多帧（真流式形态），只取第一帧。
	if i := bytes.Index(t, []byte("\ndata:")); i > 0 {
		t = bytes.TrimSpace(t[:i])
	}
	return t
}

// rewriteDataLine 把 SSE 帧里的 model 改回别名。
//
// 不能直接用 rewriteSSEModel：它只认 "data: "（带空格），而 CodeArts 发的是
// "data:{...}"（无空格），照搬会静默不改 —— 客户端看到的上游真名。
// 只替换**第一处** model：delta.content 里若出现同样的文本不该被改。
func rewriteDataLine(line []byte, model string) []byte {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return line
	}
	loc := sseModelRE.FindIndex(line)
	if loc == nil {
		return line
	}
	quoted := strconv.Quote(model)
	out := make([]byte, 0, len(line)+len(model))
	out = append(out, line[:loc[0]]...)
	out = append(out, []byte(`"model":`+quoted)...)
	out = append(out, line[loc[1]:]...)
	return out
}

// rewriteModelJSON 只替换顶层 model 字段，其余用 RawMessage 原样保留。
func rewriteModelJSON(raw []byte, model string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return raw
	}
	if q, err := json.Marshal(model); err == nil {
		obj["model"] = q
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

// translateCodeArtsError 把上游业务码映射成客户端能理解的状态与文案。
//
// 不能按 HTTP 状态一刀切：上游把「WAF 拒发」「并发超限」「模型未开通」
// 都塞在 400/403 里，而客户端只认 401 = 密钥无效。
func translateCodeArtsError(status int, raw []byte) (int, string, string) {
	var e struct {
		ErrorCode string `json:"error_code"`
		ErrorMsg  string `json:"error_msg"`
	}
	_ = json.Unmarshal(raw, &e)
	msg := e.ErrorMsg
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	code := e.ErrorCode

	switch {
	// 精确码判等，不用 includes：TM.00000001 是 TM.000000011 的前缀。
	case code == "TM.00000001":
		// WAF 按 UA 前缀拒发。不重试、不换号 —— 是网关配置问题。
		return http.StatusForbidden, "upstream_waf_blocked",
			"codearts WAF 拒绝了本次请求（检查 User-Agent 是否为官方形态）: " + msg
	case code == "TM.00001041":
		return http.StatusTooManyRequests, "upstream_concurrency_limit",
			"codearts 账号并发已达上限（每号 3 个请求），稍后重试或换号: " + msg
	case code == "TM.000010051":
		return http.StatusForbidden, "model_not_enabled",
			"该模型未在当前 codearts 租户开通: " + msg
	case code == "APIG.0301":
		return http.StatusBadGateway, "upstream_signature_invalid",
			"codearts 验签失败（本地签名与上游口径不一致）: " + msg
	case code == "APIG.0602":
		return http.StatusServiceUnavailable, "upstream_credential_expired",
			"codearts 临时凭证已过期，需要 IDE 刷新或重新登录: " + msg
	case strings.HasPrefix(code, "InferHub.001001020"):
		return http.StatusBadRequest, "image_not_supported",
			"该模型不接受图片输入: " + msg
	case code == "InferHub.4004.200":
		return http.StatusForbidden, "no_benefit_package",
			"该账号没有福利包，不能使用此模型: " + msg
	case status == http.StatusUnauthorized:
		return http.StatusBadGateway, "upstream_unauthorized",
			"codearts 鉴权失败: " + msg
	case status == http.StatusTooManyRequests:
		return http.StatusTooManyRequests, "upstream_rate_limited", "codearts 限流: " + msg
	}
	return http.StatusBadGateway, "upstream_error",
		"codearts 上游 " + http.StatusText(status) + ": " + msg
}
