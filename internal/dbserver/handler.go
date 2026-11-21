// Package server 把豆包 /chat/completion 包装成 OpenAI 兼容接口。
package dbserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	config "unified-api-v2/internal/dbconfig"
	pool "unified-api-v2/internal/dbpool"
	doubao "unified-api-v2/internal/doubaoprotocol"
	"unified-api-v2/internal/upstream"
)

const maxBodyBytes = 32 << 20

type convState struct {
	ConversationID string
	SectionID      string
	at             time.Time
}

// Server 网关主体。
type Server struct {
	cfg  *config.Config
	pool *pool.Pool
	mux  *http.ServeMux
	acl  [][]byte
	http *http.Client
	accs map[string]config.Account

	mu    sync.Mutex
	convs map[string]convState
	upAt  time.Time
}

// New 构造服务。
func New(cfg *config.Config) *Server {
	s := &Server{
		cfg:   cfg,
		pool:  pool.New(cfg.PoolOptions()),
		mux:   http.NewServeMux(),
		accs:  map[string]config.Account{},
		convs: map[string]convState{},
		upAt:  time.Now(),
		// 出站客户端：Timeout=0，不设「整请求总时长」。
		// http.Client.Timeout 覆盖 连接+等响应头+读 body 全程，会把超过该时长的
		// SSE 长流从中间掐断（2026-11-21 实测：5m0.012s 处 context deadline
		// exceeded，只收到 213 字，会话被截断而上游仍在生成）。
		//   - 首字节：Transport.ResponseHeaderTimeout（header_timeout_seconds）
		//   - 流中空闲：upstream.MonitorBody（idle_timeout_seconds，有数据即续命）
		http: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          50,
				MaxIdleConnsPerHost:   16,
				IdleConnTimeout:       90 * time.Second,
				ForceAttemptHTTP2:     true,
				ResponseHeaderTimeout: time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second,
			},
		},
	}
	for _, a := range cfg.Accounts {
		s.accs[a.Name] = a
		s.pool.Add(pool.Key{Name: a.Name, Key: a.Name, Priority: a.Priority})
	}
	logf("outbound timeout: total=off header=%ds idle=%ds（流式长请求不再被总时长掐断）",
		cfg.Upstream.HeaderTimeoutSeconds, cfg.Upstream.IdleTimeoutSeconds)
	s.loadKeys()
	s.mux.HandleFunc("POST /v1/chat/completions", s.withAuth(s.chatCompletions))
	s.mux.HandleFunc("GET /v1/models", s.withAuth(s.models))
	s.loadAccountsFile() // 启动时加载 accounts.json（面板写入的账号）
	go s.checkinLoop()   // 每日自动签到
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /status", s.withAuth(s.status))
	// 面板用的账号管理接口
	s.mux.HandleFunc("GET /admin/accounts", s.withAuth(s.adminListAccounts))
	s.mux.HandleFunc("GET /admin/quota", s.withAuth(s.adminQuota))
	s.mux.HandleFunc("GET /admin/models", s.withAuth(s.adminModels))
	s.mux.HandleFunc("GET /admin/status", s.withAuth(s.status))
	s.mux.HandleFunc("POST /admin/checkin", s.withAuth(s.adminCheckin))
	s.mux.HandleFunc("GET /admin/checkin", s.withAuth(s.adminCheckinStatus))
	s.mux.HandleFunc("GET /admin/checkin/log", s.withAuth(s.adminCheckinLog))
	s.mux.HandleFunc("GET /admin/keys", s.withAuth(s.adminListKeys))
	s.mux.HandleFunc("POST /admin/keys", s.withAuth(s.adminCreateKey))
	s.mux.HandleFunc("DELETE /admin/keys", s.withAuth(s.adminDeleteKey))
	s.mux.HandleFunc("GET /admin/usage", s.withAuth(s.adminUsage))
	// 最近请求记录（统一面板「实时使用」/「请求日志」数据源，loopback 专用）
	s.mux.HandleFunc("GET /admin/activity", s.withAuth(s.adminActivity))
	s.mux.HandleFunc("POST /admin/reload", s.withAuth(s.adminReload))
	s.mux.HandleFunc("POST /admin/cookie", s.withAuth(s.adminSetCookie))
	s.mux.HandleFunc("DELETE /admin/accounts", s.withAuth(s.adminDelete))
	s.mux.HandleFunc("POST /admin/accounts/qr", s.withAuth(s.qrStart))
	s.mux.HandleFunc("GET /admin/accounts/qr", s.withAuth(s.qrPoll))
	s.mux.HandleFunc("POST /admin/accounts/{name}/toggle", s.withAuth(s.adminToggleAccount))
	s.mux.HandleFunc("POST /admin/accounts/order", s.withAuth(s.adminAccountOrder))
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// HandleChat 供统一网关进程内调用：跳过本包 api key 鉴权（统一入口已完成鉴权），
// 直接执行 chat 逻辑。请求体的 model 应由调用方改写为上游真实模型名。
func (s *Server) HandleChat(w http.ResponseWriter, r *http.Request) { s.chatCompletions(w, r) }

// HealthStats 返回豆包号池的 (healthy, total)，供统一网关 /status 展示。
func (s *Server) HealthStats() (healthy, total int) { return s.pool.Healthy(), s.pool.Len() }

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1) 管理接口：面板与网关同机，只允许回环地址访问（人到面板有 Basic Auth）
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err == nil {
				ip := net.ParseIP(host)
				if ip != nil && ip.IsLoopback() {
					next(w, r)
					return
				}
			}
			// 远程管理：允许显式配置的 admin token（可选）
			tok := bearerToken(r)
			if s.cfg.AdminToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AdminToken)) == 1 {
				next(w, r)
				return
			}
			writeErr(w, http.StatusForbidden, "管理接口仅限本机访问", "admin_local_only", "permission_error")
			return
		}

		// 2) 业务接口：校验分发密钥
		tok := bearerToken(r)
		if k, ok := lookupKey(tok); ok {
			next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyID, k.ID)))
			return
		}
		keyMu.RLock()
		empty := len(keysDB) == 0
		keyMu.RUnlock()
		if empty {
			writeErr(w, http.StatusUnauthorized, "尚未创建任何密钥，请在管理面板「密钥」页生成", "no_keys", "invalid_request_error")
			return
		}
		writeErr(w, http.StatusUnauthorized, "invalid api key", "invalid_api_key", "invalid_request_error")
	}
}

// bearerToken 从 Authorization / x-api-key 取令牌。
func bearerToken(r *http.Request) string {
	t := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if t == "" {
		t = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	return t
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": s.pool.Len(), "healthy": s.pool.Healthy(),
		"uptime": int(time.Since(s.upAt).Seconds()),
	})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": s.pool.Stats(), "healthy": s.pool.Healthy(), "total": s.pool.Len(),
	})
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	list := s.cfg.ModelList()
	data := make([]map[string]any, 0, len(list))
	for _, m := range list {
		item := map[string]any{"id": m, "object": "model", "created": s.upAt.Unix(), "owned_by": "doubao"}
		// 带上上下文窗口：优先 model_params，其次全局 work_mode
		win := s.cfg.WorkMode.TotalWindow
		if mp, ok := s.cfg.ModelParams[m]; ok && mp.TotalWindowSize != "" {
			win = mp.TotalWindowSize
		}
		if n, err := parseInt(win); err == nil && n > 0 {
			item["context_length"] = n
			item["max_tokens"] = n
		}
		data = append(data, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// chatCompletions 主入口。
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求体失败: "+err.Error(), "bad_body", "invalid_request_error")
		return
	}
	var req doubao.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error(), "bad_json", "invalid_request_error")
		return
	}

	modelKey := s.cfg.ResolveModel(req.Model)
	effortOverride := strings.TrimSpace(req.ReasoningEffort)
	// 不同模型上下文窗口不同：优先用 model_params，其次全局 work_mode
	memProfile, winSize := s.cfg.WorkMode.MemoryProfile, s.cfg.WorkMode.TotalWindow
	if mp, ok := s.modelParamFor(req.Model, modelKey); ok {
		if mp.MemoryProfile != "" {
			memProfile = mp.MemoryProfile
		}
		if mp.TotalWindowSize != "" {
			winSize = mp.TotalWindowSize
		}
	}
	convKey := conversationKey(&req, raw, r.Header.Get(headerSessionID))
	st := s.getConv(convKey)

	body, err := doubao.BuildBody(&req, modelKey, doubao.BuildOptions{
		BotID: s.cfg.WorkMode.BotID, ModeID: s.cfg.WorkMode.ModeID,
		ReasoningEffort: effortOr(effortOverride, s.cfg.WorkMode.ReasoningEffort),
		MemoryProfile:   memProfile,
		TotalWindow:     winSize,
		EnableSandbox:   s.cfg.WorkMode.EnableSandbox,
		Workspace:       s.cfg.WorkMode.Workspace,
		DeviceName:      hostname(),
		WorkMode:        s.cfg.WorkMode.Enabled,
		ConversationID:  st.ConversationID,
		Flatten:         s.cfg.Upstream.FlattenHistory,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "invalid_request_error")
		return
	}

	large := s.cfg.Pool.LargeRequestBytes > 0 && len(body) >= s.cfg.Pool.LargeRequestBytes
	maxAttempts := s.pool.Len()
	if maxAttempts > 3 {
		maxAttempts = 3
	}
	tried := map[string]bool{}
	lastMsg := ""

	for i := 0; i < maxAttempts; i++ {
		h := s.pickKey(convKey, large, tried)
		if h == nil {
			break
		}
		name := h.Name()
		tried[name] = true
		if large {
			s.pool.NoteLarge(name)
		}
		acct, ok := s.accs[name]
		if !ok {
			s.pool.NoteAuthFailure(name, "account missing")
			continue
		}

		status, retry := s.forward(w, r, &req, body, acct, convKey, started)
		// 请求记录：只记真正落到某个账号上的尝试（status==0 表示还没写响应就换号，
		// 没有可展示的账号/状态）。面板「当前活跃账号/使用哪个模型」依赖这条。
		if status != 0 {
			recordDBActivity(dbActivityEntry{
				At: time.Now(), Account: name, Model: modelKey,
				ModelName: s.cfg.ModelLabel(modelKey),
				Alias:     strings.TrimSpace(r.Header.Get("X-Unified-Alias")),
				Status:    status, MS: time.Since(started).Milliseconds(),
				Stream: req.Stream, Tokens: -1,
			})
		}
		if status == 0 && retry {
			continue
		}
		if status == http.StatusOK {
			s.pool.NoteSuccess(name)
			s.pool.BindSticky(convKey, name)
			return
		}
		if retry {
			continue
		}
		return
	}

	if lastMsg == "" {
		lastMsg = "所有豆包账号均不可用"
	}
	logf("failed model=%s tried=%d msg=%s", modelKey, len(tried), lastMsg)
	writeErr(w, http.StatusServiceUnavailable, lastMsg, "no_available_account", "api_error")
}

// forward 单次转发；返回 (httpStatus, 是否可换号重试)。
func (s *Server) forward(w http.ResponseWriter, r *http.Request, req *doubao.ChatRequest,
	body []byte, acct config.Account, convKey string, started time.Time) (int, bool) {

	target, err := s.buildURL(acct)
	if err != nil {
		logf("build_url_error acct=%s err=%v", acct.Name, err)
		return 0, true
	}
	// 派生可取消 ctx：客户端离开 / 流中空闲断流时主动断开上游连接。
	// 只「停止读取」不行——上游会继续生成到结束，额度照烧、豆包侧会话仍停在生成中。
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, true
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "*/*")
	hreq.Header.Set("agw-js-conv", "str, str")
	hreq.Header.Set("last-event-id", "undefined")
	hreq.Header.Set("x-flow-trace", "04-0000000000000000000000000000000-0000000000000000-01")
	hreq.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	hreq.Header.Set("Referer", "https://www.doubao.com/")
	hreq.Header.Set("Cookie", acct.Cookie)

	resp, err := s.http.Do(hreq)
	if err != nil {
		logf("net_error acct=%s err=%v", acct.Name, err)
		s.pool.NoteOverload(acct.Name)
		return 0, true
	}
	// 流中空闲监控：每次读到底层数据即续命，静默超过 idle_timeout_seconds 才
	// cancel（中断阻塞中的 Read 并由 defer cancel 断开上游连接）。
	// 首字节阶段不计时——那一段由 Transport.ResponseHeaderTimeout 负责。
	resp.Body = upstream.MonitorBody(resp.Body, time.Duration(s.cfg.Upstream.IdleTimeoutSeconds)*time.Second, cancel)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(raw))
		logf("upstream_error acct=%s status=%d body=%s", acct.Name, resp.StatusCode, truncate(msg, 200))
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			s.pool.NoteAuthFailure(acct.Name, msg)
			return 0, true
		case resp.StatusCode == 429:
			s.pool.NoteRateLimit(acct.Name, parseRetryAfter(resp.Header.Get("Retry-After")))
			return 0, true
		case resp.StatusCode >= 500:
			s.pool.NoteOverload(acct.Name)
			return 0, true
		}
		writeErr(w, resp.StatusCode, msg, "upstream_error", "api_error")
		return resp.StatusCode, false
	}

	if req.Stream {
		// 把「关闭上游连接」的句柄交给 streamOut：客户端断开时要立刻断上游，
		// 而不是等整个 forward 返回再 defer——延迟越短，上游越可能提前停止生成。
		return s.streamOut(w, r, resp.Body, convKey, acct.Name, req.Model, started, func() {
			cancel()
			_ = resp.Body.Close()
		})
	}
	return s.wholeOut(w, r, resp.Body, convKey, req, acct.Name, started)
}

// sink 把 SSE 事件转发到 channel。
// ctx 非 nil 时发送带取消保护：下游已走（无人读 ch）时不会把 Pump goroutine
// 永久卡在 send 上——这是「客户端断开后上游无法及时停止」的隐性原因之一。
type sink struct {
	ch     chan ev
	ctx    context.Context
	final  strings.Builder
	reason strings.Builder

	// errMu/err 记录 Pump 的失败原因（上游 RST / 连接中断 / 空闲断流）。
	// 主循环在 ch 关闭后读它，用 close 保证的 happens-before 之外的互斥保护，
	// 避免 -race 下的数据竞争。
	errMu sync.Mutex
	err   error
}

// setErr 只记录第一个错误：后续错误通常是前一个的连带结果。
func (s *sink) setErr(err error) {
	if err == nil {
		return
	}
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
}

// getErr 取上游中断原因；nil 表示正常收尾。
func (s *sink) getErr() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// send 带 ctx 保护的事件投递。
func (s *sink) send(e ev) {
	if s.ctx == nil {
		s.ch <- e
		return
	}
	select {
	case s.ch <- e:
	case <-s.ctx.Done():
	}
}

type ev struct {
	kind    int
	text    string
	conv    string
	section string
	endType int
	brief   string
}

const (
	evText = iota + 1
	evReason
	evConv
	evFinish
)

func (s *sink) OnText(t string)              { s.final.WriteString(t); s.send(ev{kind: evText, text: t}) }
func (s *sink) OnReasoning(t string)         { s.reason.WriteString(t); s.send(ev{kind: evReason, text: t}) }
func (s *sink) OnConversation(c, sec string) { s.send(ev{kind: evConv, conv: c, section: sec}) }
func (s *sink) OnFinish(endType int, brief string) {
	s.send(ev{kind: evFinish, endType: endType, brief: brief})
}

// streamOut 流式输出。cutUpstream 非 nil 时，客户端断开/写失败会立即调用它
// （取消 ctx + 关闭上游 body），把「客户端已走」到「上游连接断开」的时延压到最小。
func (s *Server) streamOut(w http.ResponseWriter, r *http.Request, body io.Reader, convKey, acct, model string, started time.Time, cutUpstream func()) (int, bool) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	w.WriteHeader(http.StatusOK)

	sk := &sink{ch: make(chan ev, 64), ctx: r.Context()}
	go func() {
		defer close(sk.ch)
		if err := doubao.Pump(body, sk); err != nil && err != io.EOF {
			// 只记录原因，日志与收尾判定交给主循环：
			// 上游 RST 与空闲断流必须与「正常结束」区分，否则截断的响应会被
			// 伪装成完整成功（2026-11-21 实测：17:20/17:45 两次
			// stream error: INTERNAL_ERROR received from peer 均被记成 ok）。
			sk.setErr(err)
		}
	}()

	id := "chatcmpl-" + shortHash(convKey)
	created := time.Now().Unix()
	hb := time.Duration(s.cfg.Upstream.HeartbeatSeconds) * time.Second
	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	wrote := false
	finishReason := "stop"

	for {
		select {
		case <-r.Context().Done():
			// 客户端已断开：立刻断上游（不再等 forward 的 defer）。
			if cutUpstream != nil {
				cutUpstream()
			}
			logf("client_gone acct=%s elapsed=%s", acct, time.Since(started).Round(time.Millisecond))
			return 0, false
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				if cutUpstream != nil {
					cutUpstream()
				}
				// 写心跳失败 = 下游连接已废，同样要立刻断上游，否则上游继续烧额度。
				logf("client_write_fail acct=%s elapsed=%s", acct, time.Since(started).Round(time.Millisecond))
				return 0, false
			}
			if fl != nil {
				fl.Flush()
			}
		case e, ok := <-sk.ch:
			if !ok {
				// 收尾。必须区分「上游正常结束」与「上游中途断开（RST / 连接重置 /
				// 空闲断流）」：后者若照样写 [DONE] 并记 ok，截断的响应就被伪装成
				// 完整成功——这正是「网关这边已结束、豆包侧仍在生成」却无人察觉的成因。
				upErr := sk.getErr()
				finishReason = "stop"
				if upErr != nil {
					finishReason = "upstream_reset"
				}
				if !wrote && sk.final.Len() == 0 {
					msg := "(空响应)"
					if upErr != nil {
						msg = "(上游中断: " + truncate(upErr.Error(), 120) + ")"
					}
					writeChunk(w, fl, id, created, map[string]any{"content": msg}, nil)
				}
				writeChunk(w, fl, id, created, map[string]any{}, &finishReason)
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				if fl != nil {
					fl.Flush()
				}
				elapsed := time.Since(started).Round(time.Millisecond)
				if upErr != nil {
					// 空闲断流是我们主动 cancel（MonitorBody），上游重置是对端 RST，
					// 两者处置不同，日志分开记。
					if r.Context().Err() == nil && errors.Is(upErr, context.Canceled) {
						logf("upstream_idle_cut acct=%s elapsed=%s chars=%d", acct, elapsed, sk.final.Len())
					} else {
						logf("upstream_reset acct=%s elapsed=%s chars=%d err=%v", acct, elapsed, sk.final.Len(), upErr)
					}
				} else {
					logf("ok acct=%s stream=true latency=%s chars=%d", acct, elapsed, sk.final.Len())
				}
				// 状态仍记 200：HTTP 头早已发出，改不了状态码；截断事实由 finish_reason
				// 与日志体现，客户端可据此重试。
				s.recordUsage(apiKeyFrom(r.Context()), model, sk.final.Len(), http.StatusOK, int(time.Since(started)/time.Millisecond))
				return http.StatusOK, false
			}
			switch e.kind {
			case evText:
				wrote = true
				writeChunk(w, fl, id, created, map[string]any{"content": e.text}, nil)
			case evReason:
				writeChunk(w, fl, id, created, map[string]any{"reasoning_content": e.text}, nil)
			case evConv:
				s.setConv(convKey, e.conv, e.section)
			case evFinish:
				if e.endType != 1 && sk.final.Len() == 0 && e.brief != "" {
					wrote = true
					writeChunk(w, fl, id, created, map[string]any{"content": e.brief}, nil)
				}
				if e.endType != 1 && sk.final.Len() == 0 {
					finishReason = "stop"
				}
			}
		}
	}
}

// wholeOut 非流式输出。
func (s *Server) wholeOut(w http.ResponseWriter, r *http.Request, body io.Reader, convKey string, req *doubao.ChatRequest, acct string, started time.Time) (int, bool) {
	sk := &sink{ch: make(chan ev, 256)}
	done := make(chan struct{})
	go func() {
		// 文本/推理增量只累积在 sink（sk.final/sk.reason），无需转发；
		// 但会话确认事件必须落库，否则同步路径永远拿不到 conversation_id，
		// 下一次请求又会新建豆包会话（会话被打碎的第二个成因）。
		for e := range sk.ch {
			if e.kind == evConv {
				s.setConv(convKey, e.conv, e.section)
			}
		}
		close(done)
	}()
	err := doubao.Pump(body, sk)
	close(sk.ch)
	<-done
	if err != nil && err != io.EOF {
		if r.Context().Err() == nil && errors.Is(err, context.Canceled) {
			logf("upstream_idle_cut acct=%s mode=sync err=%v", acct, err)
		}
		writeErr(w, http.StatusBadGateway, "上游流读取失败: "+err.Error(), "upstream_stream_error", "api_error")
		return http.StatusBadGateway, false
	}
	text := sk.final.String()
	if text == "" {
		writeErr(w, http.StatusBadGateway, "上游没有返回内容", "empty_response", "api_error")
		return http.StatusBadGateway, false
	}
	respModel := req.Model
	if alias := strings.TrimSpace(r.Header.Get("X-Unified-Alias")); alias != "" {
		respModel = alias // 统一网关调用：回显客户端请求的别名
	}
	resp := map[string]any{
		"id":      "chatcmpl-" + shortHash(convKey),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   respModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
		},
	}
	writeJSON(w, http.StatusOK, resp)
	logf("ok acct=%s stream=false latency=%s chars=%d", acct, time.Since(started).Round(time.Millisecond), len(text))
	s.recordUsage(apiKeyFrom(r.Context()), req.Model, len(text), http.StatusOK, int(time.Since(started)/time.Millisecond))
	return http.StatusOK, false
}

// buildURL 拼上游 URL（基础参数 + 账号参数 + 签名）。
func (s *Server) buildURL(acct config.Account) (string, error) {
	u, err := url.Parse(s.cfg.Upstream.ChatURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, v := range s.cfg.Upstream.Query {
		if _, exists := q[k]; !exists {
			q.Set(k, v)
		}
	}
	dev := acct.DeviceID
	if dev == "" {
		dev = shortHash(acct.Cookie)
	}
	q.Set("device_id", dev)
	q.Set("fp", "verify_"+dev)
	q.Set("tea_uuid", dev)
	if acct.WebID != "" {
		q.Set("web_id", acct.WebID)
	}
	q.Set("web_tab_id", newUUID())
	u.RawQuery = q.Encode()

	if cmd := s.cfg.Upstream.SignCommand; cmd != "" {
		sig, err := runSign(cmd, u.String())
		if err != nil {
			logf("sign_error err=%v", err)
		} else if sig != "" {
			qq := u.Query()
			qq.Set("a_bogus", sig)
			u.RawQuery = qq.Encode()
		}
	} else if s.cfg.Upstream.ABogus != "" {
		qq := u.Query()
		qq.Set("a_bogus", s.cfg.Upstream.ABogus)
		u.RawQuery = qq.Encode()
	}
	return u.String(), nil
}

// pickKey 粘性优先 + 池策略。
func (s *Server) pickKey(conv string, large bool, tried map[string]bool) *pool.Handle {
	if name, ok := s.pool.ResolveSticky(conv); ok && !tried[name] {
		if h := s.pool.PickBySticky(name, large); h != nil {
			return h
		}
		s.pool.UnbindSticky(conv)
	}
	return s.pool.Pick(tried, large)
}

// 会话状态
func (s *Server) getConv(k string) convState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.convs[k]
}

func (s *Server) setConv(k, conv, section string) {
	if k == "" || conv == "" {
		return
	}
	s.mu.Lock()
	prev, had := s.convs[k]
	s.convs[k] = convState{ConversationID: conv, SectionID: section, at: time.Now()}
	if len(s.convs) > 5000 {
		for key, v := range s.convs {
			if time.Since(v.at) > time.Hour {
				delete(s.convs, key)
			}
		}
	}
	s.mu.Unlock()
	// 只在「新建会话 / 换会话」时记一行：这是判断会话是否被打碎的直接证据
	// （同一会话键的后续轮次不会再打印）。
	if !had || prev.ConversationID != conv {
		logf("conv_bind key=%s conv=%s", k, conv)
	}
}

// ---------- 输出辅助 ----------

func writeChunk(w http.ResponseWriter, fl http.Flusher, id string, created int64, delta map[string]any, finish *string) {
	chunk := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	b, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	if fl != nil {
		fl.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg, code, typ string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

func logf(format string, args ...any) { log.Printf(format, args...) }

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

func newUUID() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(time.Now().UnixNano() >> (i * 3))
	}
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

func hostname() string {
	h, err := exec.LookPath("hostname")
	_ = h
	if err != nil {
		return "USER"
	}
	return "USER"
}

func runSign(cmd, target string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	c.Stdin = strings.NewReader(target)
	out, err := c.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v + "s"); err == nil {
		return d
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// conversationKey 会话键：metadata.conversation_id → user → 首条消息哈希。
// headerSessionID 统一网关注入的稳定会话标识头（与 server.HeaderSessionID 同名，
// 两个 module 内部包不共享常量，此处保持字面一致）。
const headerSessionID = "X-Session-Id"

func conversationKey(req *doubao.ChatRequest, raw []byte, sessionID string) string {
	if req.Metadata != nil {
		for _, k := range []string{"conversation_id", "conversationId", "session_id"} {
			if v, ok := req.Metadata[k].(string); ok && v != "" {
				return "c:" + v
			}
		}
	}
	// 稳定会话标识（统一网关按调用方身份注入）：同一调用方的多轮请求必须落在
	// 同一个豆包会话。放在内容派生之前——内容逐轮变化，正是会话被打碎的原因。
	if id := strings.TrimSpace(sessionID); id != "" {
		return "s:" + id
	}
	if req.User != "" {
		return "u:" + req.User
	}
	for _, m := range req.Messages {
		if m.Role == "user" {
			sum := sha256.Sum256([]byte(m.Text()))
			return "h:" + hex.EncodeToString(sum[:12])
		}
	}
	sum := sha256.Sum256(raw)
	return "r:" + hex.EncodeToString(sum[:12])
}

// effortOr 请求级 reasoning_effort 优先，缺省用配置值。
func effortOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// modelParamFor 查某模型的上下文参数（先按请求名，再按 model_item_key）。
func (s *Server) modelParamFor(modelName, modelKey string) (config.ModelParam, bool) {
	if s.cfg.ModelParams == nil {
		return config.ModelParam{}, false
	}
	if p, ok := s.cfg.ModelParams[modelName]; ok {
		return p, true
	}
	if p, ok := s.cfg.ModelParams[modelKey]; ok {
		return p, true
	}
	return config.ModelParam{}, false
}

// adminModels 模型清单（管理面板用）：含豆包 key、item_id、上下文、倍率。
func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	list := s.cfg.ModelList()
	out := make([]map[string]any, 0, len(list))
	for _, name := range list {
		p := s.cfg.ModelParams[name]
		win := s.cfg.WorkMode.TotalWindow
		if p.TotalWindowSize != "" {
			win = p.TotalWindowSize
		}
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		item := map[string]any{
			"id":             name,
			"model_item_key": s.cfg.ResolveModel(name),
			"item_id":        p.ItemID,
			"memory_profile": p.MemoryProfile,
			"context_length": atoiSafe(win),
			"ratio":          p.Ratio,
			"display":        firstNonEmpty(p.Display, name),
			"enabled":        enabled,
			"is_default":     name == s.cfg.DefaultModel,
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "total": len(out),
		"default_model": s.cfg.DefaultModel})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func atoiSafe(s string) int {
	n, err := parseInt(s)
	if err != nil {
		return 0
	}
	return n
}
