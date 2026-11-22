// unified_failover.go 多 provider 健康优选与请求级故障转移。
//
// 设计约束：
//   - 只在别名声明了多个候选（alias.Providers 非空）时才启用转移；旧单 provider 别名零改动。
//   - 故障转移只在「尚未写出任何响应字节」前生效：HTTP 一旦写了首字节就无法撤回，
//     流式首帧之后的上游失败只能透传，不做重放（恶意重复输出比报错更糟）。
//   - workbuddy 走完整 chatCompletions（含其内部选号/重试），doubao 走进程内 DoubaoChat。
//     两者都先写进 captureWriter，成功（2xx）才回放到真实 ResponseWriter。
package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// ctxUnifiedBypass 标记「本次调用已进入统一分发，chatCompletions 不要再走别名分发」，
// 防止内部递归死循环。仅进程内使用，不对外暴露。
type ctxUnifiedBypass struct{}

// 说明：captureWriter 定义在 panel.go，含 Flush（满足 http.Flusher）。
// 故障转移路径复用它攒够整个响应再决定是否回放；单 provider 别名（旧路径）
// 不经过 captureWriter，保持原样边收边发。

// successful 判定一次 provider 调用是否可作为最终结果回放。
// 只看状态码 2xx；4xx/5xx（含上游错误、限流、鉴权失败）都视为失败，触发切换。
func (c *captureWriter) successful() bool {
	return c.status >= 200 && c.status < 300
}

// replay 把捕获的响应写回真实 writer。
func (c *captureWriter) replay(w http.ResponseWriter, alias string) {
	for k, vals := range c.hdr {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	// 兜底补别名头：workbuddy 在 bypass 路径下不会设置 displayModel 上下文，
	// doubao 分支虽设了，这里统一保证两种 provider 回包头一致。
	if alias != "" && w.Header().Get("X-Unified-Alias") == "" {
		w.Header().Set("X-Unified-Alias", alias)
	}
	// 响应 body 里的 model 字段统一改写回客户端请求的别名。
	body := rewriteSSEModelAny(c.body.Bytes(), alias)
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// rewriteSSEModelAny 同时处理 SSE 多行与 JSON 单响应里的 model 字段。
// SSE 用逐行 rewriteSSEModel；非流式直接对整段做一次替换。
func rewriteSSEModelAny(body []byte, alias string) []byte {
	if len(alias) == 0 {
		return body
	}
	if bytes.Contains(body, []byte("data: ")) {
		out := make([]byte, 0, len(body)+64)
		start := 0
		for i := 0; i < len(body); i++ {
			if body[i] == '\n' {
				out = append(out, rewriteSSEModel(body[start:i+1], alias)...)
				start = i + 1
			}
		}
		if start < len(body) {
			out = append(out, rewriteSSEModel(body[start:], alias)...)
		}
		return out
	}
	// 非流式：复用 SSE 的单行改写（正则只替换 "model":"..."）。
	return rewriteSSEModel(append([]byte("data: "), body...), alias)[6:]
}

// callProvider 把一个候选 provider 调用写入 captureWriter。
// 返回 nil 表示调用链未接线（如 doubao provider 未启用）。
func (h *Handler) callProvider(ctx context.Context, r *http.Request, provider string, rewrittenBody []byte, capture http.ResponseWriter, aliasName string) error {
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(rewrittenBody))
	r2.ContentLength = int64(len(rewrittenBody))
	r2.Header = r.Header.Clone()

	switch provider {
	case "doubao":
		if h.cfg.DoubaoChat == nil {
			writeOpenAIError(capture, http.StatusServiceUnavailable, "provider_unavailable",
				"doubao provider 未接线：检查 providers.doubao.enabled 与启动日志")
			return nil
		}
		r2.Header.Set("X-Unified-Alias", aliasName)
		// 稳定会话标识：与单 provider 路径同口径，故障转移换 provider 不改会话，
		// 避免同一轮对话在豆包侧被拆成两个会话。
		r2.Header.Set(HeaderSessionID, doubaoSessionID(r))
		h.cfg.DoubaoChat(capture, r2)
		return nil
	case "trae":
		if h.cfg.TraeChat == nil {
			writeOpenAIError(capture, http.StatusServiceUnavailable, "provider_unavailable",
				"trae provider 未接线：检查 providers.trae.enabled 与 Trae2api 是否在线")
			return nil
		}
		// 模型名已在 dispatchUnifiedAlias 里按别名重写成上游名；这里只负责转发。
		r2.Header.Set("X-Unified-Alias", aliasName)
		h.cfg.TraeChat(capture, r2)
		return nil
	case "loomy":
		if h.cfg.LoomyChat == nil {
			writeOpenAIError(capture, http.StatusServiceUnavailable, "provider_unavailable",
				"loomy provider 未接线：检查启动日志与 loomy/accounts.json")
			return nil
		}
		r2.Header.Set("X-Unified-Alias", aliasName)
		r2.Header.Set(HeaderSessionID, doubaoSessionID(r))
		h.cfg.LoomyChat(capture, r2)
		return nil
	case "workbuddy", "":
		// 绕过别名分发，直接进原聊天处理；用 bypass 标记防止递归。
		r2 = r2.WithContext(context.WithValue(r2.Context(), ctxUnifiedBypass{}, true))
		chat := h.cfg.WorkbuddyChat
		if chat == nil {
			chat = h.chatCompletions
		}
		chat(capture, r2)
		return nil
	default:
		writeOpenAIError(capture, http.StatusNotImplemented, "provider_unavailable",
			"provider "+provider+" 尚未接入统一网关")
		return nil
	}
}
