// stream_capture.go 故障转移路径的流式感知响应捕获器。
//
// 与 panel.go 的 captureWriter（攒整段再回放）不同，本类型实现「首字节前可切、
// 首字节后立即透传」：
//   - provider 写入 2xx 首字节时，立刻把响应头 + 已攒字节写到下游并标记 committed；
//   - committed 之后所有 Write 直接透传 + Flush，实现真正的实时流式；
//   - committed 之后不再切换 provider（HTTP 无法撤回已发字节）；
//   - 4xx/5xx 响应不 committed，攒着等调用方决定是否切下一候选。
//
// 代价：首字节之后的上游失败只能透传错误帧，不能重放。这是 HTTP 语义限制，
// 也是用户 2026-11-21 选定的「首字节后透传」方案。
package server

import (
	"bytes"
	"net/http"
)

type streamCaptureWriter struct {
	downstream http.ResponseWriter
	alias      string

	hdr       http.Header
	status    int
	buf       bytes.Buffer
	committed bool
	wroteHdr  bool
}

func newStreamCaptureWriter(w http.ResponseWriter, alias string) *streamCaptureWriter {
	return &streamCaptureWriter{
		downstream: w,
		alias:      alias,
		hdr:        make(http.Header),
		status:     0,
	}
}

func (c *streamCaptureWriter) Header() http.Header {
	if c.hdr == nil {
		c.hdr = make(http.Header)
	}
	return c.hdr
}

// commit 把当前攒下的头与数据一次性写到下游，之后进入透传模式。
func (c *streamCaptureWriter) commit() {
	if c.committed {
		return
	}
	c.committed = true
	for k, vals := range c.hdr {
		for _, v := range vals {
			c.downstream.Header().Add(k, v)
		}
	}
	if c.alias != "" && c.downstream.Header().Get("X-Unified-Alias") == "" {
		c.downstream.Header().Set("X-Unified-Alias", c.alias)
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	c.downstream.WriteHeader(status)
	c.wroteHdr = true
	if c.buf.Len() > 0 {
		body := rewriteSSEModelAny(c.buf.Bytes(), c.alias)
		_, _ = c.downstream.Write(body)
		c.buf.Reset()
	}
	if f, ok := c.downstream.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *streamCaptureWriter) WriteHeader(status int) {
	if c.committed {
		return // 已提交后不能再改状态
	}
	c.status = status
	// 2xx：收到状态即视为成功，立即提交（头先发出去，数据随后续 Write 实时透传）。
	// 4xx/5xx：不提交，攒着让调用方决定切换。
	if status >= 200 && status < 300 {
		c.commit()
	}
}

func (c *streamCaptureWriter) Write(p []byte) (int, error) {
	if c.committed {
		// 透传模式：逐帧改写 model 字段后直接写出，立即 Flush（实时流式）。
		out := rewriteSSEModelAny(p, c.alias)
		_, err := c.downstream.Write(out)
		if f, ok := c.downstream.(http.Flusher); ok && len(out) > 0 {
			f.Flush()
		}
		return len(p), err // 返回原始长度（out 可能被改写变长）
	}
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.buf.Write(p)
	// 没显式 WriteHeader 的 provider（如直接 Write 的 mock/部分实现）：
	// 首字节即成功，提交。
	if c.status >= 200 && c.status < 300 {
		c.commit()
	}
	return len(p), nil
}

// Flush 满足 http.Flusher。已提交时把下游 Flush；未提交时提交（若有数据）。
func (c *streamCaptureWriter) Flush() {
	if !c.committed {
		c.commit()
		return
	}
	if f, ok := c.downstream.(http.Flusher); ok {
		f.Flush()
	}
}

// successful 判定是否可以作为最终结果。
// committed 的一定成功（已写下游）；未提交时看状态码。
func (c *streamCaptureWriter) successful() bool {
	if c.committed {
		return true
	}
	return c.status >= 200 && c.status < 300
}

// fallbackReplay 把未提交的失败响应攒段写回下游（所有候选都失败时用）。
func (c *streamCaptureWriter) fallbackReplay() {
	if c.committed {
		return
	}
	for k, vals := range c.hdr {
		for _, v := range vals {
			c.downstream.Header().Add(k, v)
		}
	}
	if c.alias != "" && c.downstream.Header().Get("X-Unified-Alias") == "" {
		c.downstream.Header().Set("X-Unified-Alias", c.alias)
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	c.downstream.WriteHeader(status)
	_, _ = c.downstream.Write(rewriteSSEModelAny(c.buf.Bytes(), c.alias))
	c.committed = true
	c.wroteHdr = true
}
