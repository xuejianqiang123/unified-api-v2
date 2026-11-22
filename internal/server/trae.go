package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Trae2api 是独立的 Python 进程（FastAPI，默认 127.0.0.1:8000），自己管理
// Trae CN 的多账号、签到与积分。统一网关只把它当作一个 OpenAI 兼容上游：
// 聊天请求原样转发，管理操作走白名单代理。
//
// 为什么不内嵌：Trae2api 是 Python 实现且自带 OAuth/JWT 刷新、设备指纹、
// 9074 风控退避等一整套逻辑，用 Go 重写的正确性风险远高于进程间调用。

const (
	// traeDefaultBaseURL Trae2api 默认监听地址。
	traeDefaultBaseURL = "http://127.0.0.1:8000"
	// traeMaxChatBody 聊天请求体上限（与网关其它入口同级，防止内存被单请求撑爆）。
	traeMaxChatBody = 32 << 20
	// traeHeaderTimeout 等上游响应头的时间。Trae 创建会话可能较慢（排队/选号），
	// 但不应无限等；body 阶段不设总超时，靠调用方 ctx 与流式空闲控制。
	traeHeaderTimeout = 5 * time.Minute
)

// TraeClient Trae provider 的 HTTP 客户端（聊天反代 + 管理代理）。
type TraeClient struct {
	baseURL string
	apiKey  string
	http    *http.Client

	// 健康探测缓存：面板会高频拉 overview，每次真打 /healthz 会放大开销。
	pingMu sync.Mutex
	pingAt time.Time
	pingOK bool
}

// NewTraeClient 构造客户端。baseURL 为空时回落默认地址。
func NewTraeClient(baseURL, apiKey string) *TraeClient {
	b := strings.TrimSpace(baseURL)
	if b == "" {
		b = traeDefaultBaseURL
	}
	return &TraeClient{
		baseURL: strings.TrimRight(b, "/"),
		apiKey:  strings.TrimSpace(apiKey),
		http: &http.Client{
			Transport: &http.Transport{
				// 关闭整请求总超时：流式长回答不能按总时长掐断（第 25 节的教训）。
				ResponseHeaderTimeout: traeHeaderTimeout,
				IdleConnTimeout:       90 * time.Second,
				MaxIdleConnsPerHost:   16,
			},
		},
	}
}

// BaseURL 返回生效的上游地址（供日志与面板展示）。
func (t *TraeClient) BaseURL() string { return t.baseURL }

// isHopByHop 逐跳头不应在反代时透传。
func isHopByHop(k string) bool {
	switch http.CanonicalHeaderKey(k) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// Chat 反代 POST /v1/chat/completions 到 Trae2api。
//
// 语义与 doubao 的进程内入口一致：状态码与响应头原样透传，body 边读边 flush，
// 因此统一网关的故障转移（首字节后锁定）与流式透传对它同样生效。
func (t *TraeClient) Chat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, traeMaxChatBody))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "读取请求体失败: "+err.Error())
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, t.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", r.Header.Get("Accept"))
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}

	start := time.Now()
	resp, err := t.http.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_unreachable",
			"Trae 上游不可达（"+t.baseURL+"）："+err.Error())
		recordActivity(activityEntry{
			At: time.Now(), Provider: "trae", Model: probeModel(body),
			Alias: r.Header.Get("X-Unified-Alias"), Account: "—",
			Status: http.StatusBadGateway, MS: time.Since(start).Milliseconds(), Stream: true, Tokens: -1,
		})
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
	// 流结束后记一条活动：Trae 的账号由它内部轮询选号，网关拿不到具体账号，
	// 账号列固定为「—」；成功与否以 HTTP 状态判定。
	recordActivity(activityEntry{
		At: time.Now(), Provider: "trae", Model: probeModel(body),
		Alias: r.Header.Get("X-Unified-Alias"), Account: "—",
		Status: resp.StatusCode, MS: time.Since(start).Milliseconds(), Stream: true, Tokens: -1,
	})
}

// probeModel 从请求体取模型名，仅用于活动日志展示（取不到返回空）。
func probeModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &v)
	return v.Model
}

// flushCopy 流式拷贝：能 flush 就每块写完立即下发，避免攒包导致"看起来不走流式"。
func flushCopy(w http.ResponseWriter, src io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 下游断开，停止读取上游
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// traeAdminAllow 管理接口白名单：path → 允许的方法。
//
// 只放通面板确实要用的端点，不做任意路径转发——Trae2api 还有 /web/login、
// /api/logout、/api/settings 等会改变全局状态的接口，不应经由网关暴露。
var traeAdminAllow = map[string][]string{
	"/api/checkin/accounts":         {http.MethodGet},
	"/api/checkin/status":           {http.MethodGet},
	"/api/checkin/credits/accounts": {http.MethodGet}, // 权益刷新（导入账号后重新查积分）
	"/api/accounts":                 {http.MethodGet},
	"/api/overview":                 {http.MethodGet},
	"/api/accounts/model-enabled":   {http.MethodPost},
	"/api/accounts/remove":          {http.MethodPost},
	"/api/accounts/reorder":         {http.MethodPost}, // 账号轮询顺序（面板拖拽优先级）
	"/api/web-auth":                 {http.MethodPost}, // 直接写入凭证（面板「导入账号」用，替代跳转 OAuth 页）
	"/api/checkin/claim-all":        {http.MethodPost},
}

// traeAdminPrefix 带路径参数的管理端点：前缀 → 允许的方法。
// 参数段由 traeSafeIDSegment 校验，禁止 ../ 与路径分隔符逃逸。
var traeAdminPrefix = []struct {
	prefix  string
	methods []string
}{
	{"/api/checkin/account/", []string{http.MethodGet, http.MethodPost}},
}

// traeSafeIDSegment 账号 ID 只允许字母数字、下划线与短横线。
func traeSafeIDSegment(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// traeAllowed 判断 (method, path) 是否放通。
func traeAllowed(method, path string) bool {
	if methods, ok := traeAdminAllow[path]; ok {
		for _, m := range methods {
			if m == method {
				return true
			}
		}
		return false
	}
	for _, rule := range traeAdminPrefix {
		if !strings.HasPrefix(path, rule.prefix) {
			continue
		}
		seg := strings.TrimPrefix(path, rule.prefix)
		if !traeSafeIDSegment(seg) {
			return false
		}
		for _, m := range rule.methods {
			if m == method {
				return true
			}
		}
		return false
	}
	return false
}

// Admin 调用 Trae2api 的管理端点，返回解析后的 JSON 与 HTTP 状态码。
//
// path 必须命中白名单；未命中直接 403，避免把 Trae2api 的全部管理面暴露给面板。
func (t *TraeClient) Admin(ctx context.Context, method, path string, body []byte) (map[string]any, int) {
	if !traeAllowed(method, path) {
		return map[string]any{"error": "该 Trae 管理端点未被网关放通: " + method + " " + path}, http.StatusForbidden
	}

	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.baseURL+path, rd)
	if err != nil {
		return map[string]any{"error": err.Error()}, http.StatusBadRequest
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}

	resp, err := t.http.Do(req)
	if err != nil {
		return map[string]any{"error": "Trae 管理接口不可达: " + err.Error()}, http.StatusBadGateway
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"error": "Trae 响应不是 JSON: " + truncateStr(string(raw), 200)}, resp.StatusCode
	}
	return out, resp.StatusCode
}

// Ping 探测 Trae2api 是否在线（供面板 Provider 状态表）。
func (t *TraeClient) Ping(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.baseURL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode == http.StatusOK
}

// PingCached 3 秒内复用上次探测结果，避免面板高频刷新反复打 /healthz。
func (t *TraeClient) PingCached(ctx context.Context) bool {
	t.pingMu.Lock()
	if !t.pingAt.IsZero() && time.Since(t.pingAt) < 3*time.Second {
		ok := t.pingOK
		t.pingMu.Unlock()
		return ok
	}
	t.pingMu.Unlock()

	ok := t.Ping(ctx)

	t.pingMu.Lock()
	t.pingAt, t.pingOK = time.Now(), ok
	t.pingMu.Unlock()
	return ok
}

// truncateStr 截断字符串用于错误信息，避免把整页 HTML 塞进 JSON。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
