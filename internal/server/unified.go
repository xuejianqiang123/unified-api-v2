// Package server 大统一入口：全局模型别名与 provider 分发。
//
// 设计约束（用户 2026-11 明确要求）：
//   - 不套一层代理：WorkBuddy 与 doubao 在同一个 HTTP Server、同一个进程内运行。
//   - 不加前缀，只暴露全局唯一别名：/v1/models 只列别名，/v1/chat/completions 只接受别名。
//   - 未注册别名必须 400 明确拒绝，不静默路由。
//
// Aliases 为空时所有新增逻辑旁路，行为与 workbuddy2api 原版逐字一致（回归保护）。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// sseModelRE 匹配 SSE data 帧里的 "model":"..." 字段（只改这一处）。
var sseModelRE = regexp.MustCompile(`"model"\s*:\s*"[^"]*"`)

func rewriteSSEModel(line []byte, model string) []byte {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return line
	}
	quoted := strconv.Quote(model)
	return sseModelRE.ReplaceAllFunc(line, func(_ []byte) []byte {
		return []byte(`"model":` + quoted)
	})
}

// aliasStreamWriter 在 SSE 逐帧透传时把 model 字段改回客户端请求的别名。
// 必须实现 Flush，否则上层 StreamHint 的类型断言拿不到 http.Flusher，流式会卡在缓冲里。
type aliasStreamWriter struct {
	http.ResponseWriter
	model string
	buf   []byte
}

func (w *aliasStreamWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i+1]
		w.buf = w.buf[i+1:]
		if _, err := w.ResponseWriter.Write(rewriteSSEModel(line, w.model)); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func (w *aliasStreamWriter) Flush() {
	if len(w.buf) > 0 {
		line := w.buf
		w.buf = w.buf[:0]
		_, _ = w.ResponseWriter.Write(rewriteSSEModel(line, w.model))
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ctxDisplayModelKey 记录客户端请求的别名。上游要拿真实模型名，但响应里应回显别名。
type ctxDisplayModelKey struct{}

func displayModelFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxDisplayModelKey{}).(string)
	return v
}

// UnifiedAlias 一条全局模型别名。
//
// Provider 只接受 "workbuddy" / "doubao"；Model 是上游真实模型名，分发前写回请求体。
// Name / ContextLength 只用于 /v1/models 展示，不影响路由。
type UnifiedAlias struct {
	// Provider 单 provider（旧格式，向后兼容）。为空时使用 Providers。
	Provider string `json:"provider"`
	// Providers 多候选 provider，按顺序参与健康优选与故障转移。
	// 非空时优先于 Provider；每个候选通常提供同一个上游 Model。
	Providers []string `json:"providers,omitempty"`
	// Failover 为 true 时，主 provider 失败（未写出成功响应前）自动切下一候选。
	Failover      bool   `json:"failover,omitempty"`
	Model         string `json:"model"`
	Name          string `json:"name,omitempty"`
	ContextLength int    `json:"context_length,omitempty"`
}

// Candidates 返回该别名的 provider 候选列表（去重、去空、保序）。
// 导出供 cmd/server 启动期校验复用，避免两处各写一份解析逻辑产生漂移。
func (a UnifiedAlias) Candidates() []string {
	src := a.Providers
	if len(src) == 0 && a.Provider != "" {
		src = []string{a.Provider}
	}
	out := make([]string, 0, len(src))
	seen := map[string]bool{}
	for _, p := range src {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// rewriteModelInBody 只替换 JSON 顶层 "model" 字段，其余字段用 RawMessage 原样保留。
// 不用 map[string]any 整包解码：那会把大整数转成 float64，可能改变客户端参数精度。
func rewriteModelInBody(body []byte, model string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	v, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	obj["model"] = v
	return json.Marshal(obj)
}

// aliasNames 返回排序后的别名列表，供错误提示与 /v1/models 使用（稳定输出）。
func (h *Handler) aliasNames() []string {
	names := make([]string, 0, len(h.cfg.Aliases))
	for n := range h.cfg.Aliases {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// aliasModelList 把别名表转成 OpenAI /v1/models 的 data 数组。
func (h *Handler) aliasModelList() []map[string]any {
	names := h.aliasNames()
	data := make([]map[string]any, 0, len(names))
	for _, n := range names {
		a := h.cfg.Aliases[n]
		item := map[string]any{
			"id":       n,
			"object":   "model",
			"owned_by": a.Provider,
		}
		if a.Name != "" {
			item["name"] = a.Name
		}
		if a.ContextLength > 0 {
			item["context_length"] = a.ContextLength
			item["max_tokens"] = a.ContextLength
		}
		data = append(data, item)
	}
	return data
}

// dispatchUnifiedAlias 在 chatCompletions 读 body 之后调用。
//
// 返回值：
//   - newBody    ：可能被重写 model 字段的请求体
//   - handled    ：true 表示 provider 已完整写入响应，调用方必须直接 return
//   - model      ：分发后实际使用的模型名（供后续 realm 解析 / 选号 / 账本）
func (h *Handler) dispatchUnifiedAlias(w http.ResponseWriter, r *http.Request, body []byte, model string) (newBody []byte, handled bool, effectiveModel string) {
	newBody, effectiveModel = body, model
	if len(h.cfg.Aliases) == 0 {
		return
	}
	// 内部递归（故障转移调用 workbuddy 原流程）已解析过别名，跳过分发防止死循环。
	if r.Context().Value(ctxUnifiedBypass{}) != nil {
		return
	}

	alias, ok := h.cfg.Aliases[model]
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "unknown_model",
			fmt.Sprintf("未注册的模型别名 %q；可用别名: %v", model, h.aliasNames()))
		return newBody, true, model
	}

	// 多候选 + 故障转移：按配置顺序依次尝试，首字节前可切、首字节后透传。
	// 用 streamCaptureWriter 实现实时流式：2xx 首字节立即推给客户端并锁定 provider；
	// 4xx/5xx 不提交，攒着等切下一候选；全失败回最后一个错误。
	cands := alias.Candidates()
	if alias.Failover && len(cands) > 1 {
		ordered := h.orderCandidates(cands)
		var last *streamCaptureWriter
		for _, p := range ordered {
			rewritten, err := rewriteModelInBody(body, alias.Model)
			if err != nil {
				writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "重写模型名失败: "+err.Error())
				return newBody, true, model
			}
			cap := newStreamCaptureWriter(w, model)
			_ = h.callProvider(r.Context(), r, p, rewritten, cap, model)
			last = cap
			if cap.successful() {
				if !cap.committed {
					// 理论上 2xx 会触发 commit；兜底回放（如 mock 只 WriteHeader 无 body）。
					cap.fallbackReplay()
				}
				return newBody, true, model
			}
		}
		if last != nil {
			last.fallbackReplay()
		} else {
			writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable", "无可用 provider")
		}
		return newBody, true, model
	}

	// 单 provider（旧路径，零回归）：仅当显式声明了多个候选但未开 Failover 时按健康选第一个。
	if len(cands) > 1 {
		ordered := h.orderCandidates(cands)
		alias.Provider = ordered[0]
	}

	switch alias.Provider {
	case "doubao":
		if h.cfg.DoubaoChat == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable",
				"doubao provider 未接线：检查 providers.doubao.enabled 与启动日志")
			return newBody, true, model
		}
		rewritten, err := rewriteModelInBody(body, alias.Model)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "重写模型名失败: "+err.Error())
			return newBody, true, model
		}
		// 进程内调用：克隆请求并替换 body，跳过 doubao 自己那套 api key 鉴权
		// （鉴权已由统一入口 withAuth 完成），避免两套密钥体系互相打架。
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(bytes.NewReader(rewritten))
		r2.ContentLength = int64(len(rewritten))
		// 让 provider 在响应里回显别名而不是上游真名（dbserver 读此头）。
		r2.Header = r.Header.Clone()
		r2.Header.Set("X-Unified-Alias", model)
		// 稳定会话标识：dbserver 据此复用同一个豆包会话，而不是按消息内容
		// 每轮新建（见 session_id.go）。
		r2.Header.Set(HeaderSessionID, doubaoSessionID(r))
		h.cfg.DoubaoChat(w, r2)
		return newBody, true, model

	case "trae":
		// 单 provider 路径与大统一路径（callProvider）是两段独立分发，
		// 新增 provider 时两处都要接——漏一处只会在这条路径上报 501。
		if h.cfg.TraeChat == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable",
				"trae provider 未接线：检查 providers.trae.enabled 与 Trae2api 是否在线")
			return newBody, true, model
		}
		rewritten, err := rewriteModelInBody(body, alias.Model)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "重写模型名失败: "+err.Error())
			return newBody, true, model
		}
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(bytes.NewReader(rewritten))
		r2.ContentLength = int64(len(rewritten))
		r2.Header = r.Header.Clone()
		r2.Header.Set("X-Unified-Alias", model)
		h.cfg.TraeChat(w, r2)
		return newBody, true, model

	case "loomy":
		if h.cfg.LoomyChat == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable",
				"loomy provider 未接线：检查启动日志与 loomy/accounts.json")
			return newBody, true, model
		}
		rewritten, err := rewriteModelInBody(body, alias.Model)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "重写模型名失败: "+err.Error())
			return newBody, true, model
		}
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(bytes.NewReader(rewritten))
		r2.ContentLength = int64(len(rewritten))
		r2.Header = r.Header.Clone()
		r2.Header.Set("X-Unified-Alias", model)
		// 会话键：loomy 要按会话复用 conversationId，与豆包同源。
		r2.Header.Set(HeaderSessionID, doubaoSessionID(r))
		h.cfg.LoomyChat(w, r2)
		return newBody, true, model

	case "workbuddy", "":
		rewritten, err := rewriteModelInBody(body, alias.Model)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "重写模型名失败: "+err.Error())
			return newBody, true, model
		}
		return rewritten, false, alias.Model

	default:
		writeOpenAIError(w, http.StatusNotImplemented, "provider_unavailable",
			fmt.Sprintf("provider %q 尚未接入统一网关", alias.Provider))
		return newBody, true, model
	}
}

// providerStatus 供 /status 与面板展示各 provider 的健康度与别名归属。
//
// aliases 口径（曾引起误解）：公共模型由多侧共同提供，同名别名会同时计入每一侧，
// 所以两栏可能都显示同一个数字。这里额外给出 aliases_public（该 provider 参与的
// 公共别名数）与 aliases_own（仅该 provider 提供的别名数），让归属一目了然。
//
// 统计必须通用而非按"是否含 workbuddy"硬编码：加了 trae 之后，存在只含
// doubao+trae 的公共别名，旧判断会把它错算成 workbuddy 的独有别名。
func (h *Handler) providerStatus() map[string]any {
	out := map[string]any{}
	if len(h.cfg.Aliases) == 0 {
		return out
	}
	type stat struct{ public, own int }
	stats := map[string]*stat{}
	at := func(p string) *stat {
		s := stats[p]
		if s == nil {
			s = &stat{}
			stats[p] = s
		}
		return s
	}
	for _, a := range h.cfg.Aliases {
		seen := map[string]bool{}
		for _, p := range a.Candidates() {
			if p != "" {
				seen[p] = true
			}
		}
		switch len(seen) {
		case 0:
			continue
		case 1:
			for p := range seen {
				at(p).own++
			}
		default:
			for p := range seen {
				at(p).public++
			}
		}
	}

	// WorkBuddy：总是输出（即使 0 别名，面板也用它显示本地号池健康）。
	wh, wt := 0, 0
	if h.cfg.Pool != nil {
		wt, wh, _, _, _ = h.cfg.Pool.CountsDetailed()
	}
	wbs, wbOK := stats["workbuddy"]
	if !wbOK {
		wbs = &stat{}
	}
	out["workbuddy"] = map[string]any{
		"aliases": wbs.public + wbs.own, "aliases_public": wbs.public, "aliases_own": wbs.own,
		"healthy": wh, "total": wt,
	}

	// 豆包：未接线时仍给出别名归属，但不带健康数（未知不等于 0 健康）。
	ds, dOK := stats["doubao"]
	if !dOK {
		ds = &stat{}
	}
	dbItem := map[string]any{
		"aliases": ds.public + ds.own, "aliases_public": ds.public, "aliases_own": ds.own,
	}
	if h.cfg.DoubaoHealth != nil {
		healthy, total := h.cfg.DoubaoHealth()
		dbItem["healthy"] = healthy
		dbItem["total"] = total
	}
	out["doubao"] = dbItem

	// Trae：外部进程，健康度用带缓存的 /healthz 探测；未接线时不出现在状态表。
	if h.cfg.TraeChat != nil || h.cfg.TraeAdmin != nil {
		ts, tOK := stats["trae"]
		if !tOK {
			ts = &stat{}
		}
		item := map[string]any{
			"aliases": ts.public + ts.own, "aliases_public": ts.public, "aliases_own": ts.own,
		}
		if h.cfg.TraeAdmin != nil {
			// 面板「Trae 控制台」入口需要知道它的地址（登录/OAuth 只能在那边完成）。
			item["base_url"] = h.cfg.TraeAdmin.BaseURL()
			if ok := h.cfg.TraeAdmin.PingCached(context.Background()); ok {
				item["healthy"], item["total"] = 1, 1
			} else {
				item["healthy"], item["total"] = 0, 1
			}
		}
		out["trae"] = item
	}
	return out
}

// orderCandidates 严格保持配置的候选顺序（优先级），健康状态不参与排序。
//
// 用户规则（2026-11-21）：「优先使用豆包路由，失败后再使用 workbuddy」——
// 这是配置顺序 + 请求级故障转移语义，不是健康优选。若按健康账号数重排，
// 会让 workbuddy（健康数更多）反向抢占豆包，违反明确优先级。
// 主 provider 不健康/失败时，由 Failover 逐个尝试下一个候选来处理，
// 而不是提前把主 provider 换到最后。
func (h *Handler) orderCandidates(cands []string) []string {
	if len(cands) <= 1 {
		return cands
	}
	pref := h.ProviderOrder()
	if len(pref) == 0 {
		return cands
	}
	// 按全局优先序重排：优先序里有的人按序前置，其余的保持候选原顺序补在末尾
	// （缺省不丢候选——未知 provider 若被配置意外引入，仍会被尝试到）。
	out := make([]string, 0, len(cands))
	for _, p := range pref {
		for _, c := range cands {
			if c == p {
				out = append(out, c)
			}
		}
	}
	for _, c := range cands {
		found := false
		for _, o := range out {
			if o == c {
				found = true
				break
			}
		}
		if !found {
			out = append(out, c)
		}
	}
	return out
}
