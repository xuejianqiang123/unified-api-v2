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
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

type streamCaptureWriter struct {
	downstream http.ResponseWriter
	alias      string

	hdr       http.Header
	status    int
	buf       bytes.Buffer
	committed bool
	wroteHdr  bool

	// lastUsage 记录本次响应里观测到的 total_tokens（最后一个 usage 帧为准）。
	// 流式响应里 usage 常出现在末尾帧，且字段可能被 Write 边界切开，
	// 所以用一个小尾巴缓冲累积扫描。
	lastUsage int
	scanTail  []byte
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
	// 2xx：默认收到状态即提交（头先发出去，数据随后续 Write 实时透传）。
	// 但**明确是 JSON 的响应**要攒着 —— 有的上游会对不认识的模型回
	// 200 + 空 choices，若立刻提交，failover 会被这段「假成功」短路，
	// 客户端拿到空回复而不会切到下一个候选。
	// 4xx/5xx：不提交，攒着让调用方决定切换。
	if status >= 200 && status < 300 && !c.deferCommit() {
		c.commit()
	}
}

func (c *streamCaptureWriter) Write(p []byte) (int, error) {
	c.scanUsage(p)
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
	// 首字节即成功并提交 —— 除非是 JSON（要等 body 齐了判空壳）。
	if c.status >= 200 && c.status < 300 && !c.deferCommit() {
		c.commit()
	}
	return len(p), nil
}

// Flush 满足 http.Flusher。已提交时把下游 Flush；未提交时提交（若有数据）。
//
// ⚠️ **失败响应不在这里提交**（2026-11-22 实测的真实漏洞）：
// 很多 provider 代理实现写完响应后会顺手 Flush，若 Flush 无条件 commit，
// 一个 4xx 会被标记成「已提交」，而 successful() 对已提交的响应恒返回 true
// ⇒ failover 误判成功、把错误原样返回给客户端、**不再切下一家**。
// trae 的 /v1/chat 正是「写 400 + Flush」这种形态，实测 glm-5.3 的 3 个后备
// 候选因此从未被尝试过。
func (c *streamCaptureWriter) Flush() {
	if !c.committed {
		if c.status != 0 && (c.status < 200 || c.status >= 300) {
			return // 4xx/5xx：攒着，交给调用方决定切不切
		}
		c.commit()
		return
	}
	if f, ok := c.downstream.(http.Flusher); ok {
		f.Flush()
	}
}

// deferCommit 表示这次响应要不要等 body 齐了再提交。
//
// 只对**明确是 application/json** 的响应为 true：
//   - text/event-stream 必须首字节就提交，否则实时流式退化成攒完再发；
//   - 未设置 Content-Type 的保持旧行为（立即提交）—— 不能因为「猜不出」
//     就把流式卡住，那是比空壳更严重的退化。
func (c *streamCaptureWriter) deferCommit() bool {
	ct := strings.ToLower(c.hdr.Get("Content-Type"))
	return strings.Contains(ct, "application/json")
}

// successful 判定是否可以作为最终结果。
//
// 三种情况：
//  1. 已提交（SSE 或非 JSON）→ 一定成功，HTTP 无法撤回已发字节；
//  2. 非 2xx → 失败；
//  3. 2xx 但攒着（JSON）→ 还要看 body 是不是**空壳**。
//
// 空壳的判据（保守）：JSON 里 choices 为空/缺失，且没有 data 字段。
// 正常 chat/completions 响应一定有至少一个 choice，所以 choices 为空 = 上游
// 实际上没给出结果。实测 doubao 对不认识的模型正是这种形态（200 + 空 choices
// + usage 全 0），不判它就会让 failover 静默失效。
func (c *streamCaptureWriter) successful() bool {
	if c.committed {
		return true
	}
	if c.status < 200 || c.status >= 300 {
		return false
	}
	if !c.deferCommit() {
		return true
	}
	return !isEmptyCompletion(c.buf.Bytes())
}

// isEmptyCompletion 判断响应体是不是「没有任何结果的空壳」。
//
// 解析不出 JSON 时返回 false（不判空壳）—— 宁可放过一个可疑响应，
// 也不要把正常的非 JSON 响应误判成失败而触发多余的切换。
func isEmptyCompletion(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	// 用指针区分「字段不存在」与「空数组」：{"data":[]} 是 models 形态，
	// 不能因为数组为空就被当成 chat 的空壳。
	var probe struct {
		Choices *[]json.RawMessage `json:"choices"`
		Data    *json.RawMessage   `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	if probe.Data != nil {
		return false // 有 data 字段 ⇒ 不是 chat 响应，不判空壳
	}
	if probe.Choices == nil {
		return true // 没有 choices 字段 ⇒ 上游没给结果
	}
	return len(*probe.Choices) == 0
}

// usageRE 匹配响应里的 total_tokens（容忍空格）。
var usageRE = regexp.MustCompile(`"total_tokens"\s*:\s*(\d+)`)

// scanUsage 累积扫描 total_tokens，供用量统计使用。
//
// 为什么要尾巴缓冲：SSE 帧可能被 Write 边界切开，`"total_tokens": 1` 的两半
// 分处两次 Write 时，单次匹配会漏。保留最后 8KB 足以覆盖一个 usage 对象。
func (c *streamCaptureWriter) scanUsage(p []byte) {
	c.scanTail = append(c.scanTail, p...)
	if len(c.scanTail) > 8192 {
		c.scanTail = c.scanTail[len(c.scanTail)-8192:]
	}
	if m := usageRE.FindAllSubmatch(c.scanTail, -1); len(m) > 0 {
		if n, err := strconv.Atoi(string(m[len(m)-1][1])); err == nil {
			c.lastUsage = n
		}
	}
}

// UsageTokens 返回本次响应观测到的 total_tokens（0 = 未知）。
func (c *streamCaptureWriter) UsageTokens() int { return c.lastUsage }

// failClass 给出这次响应的失败分类，供分发层决定「要不要切下一家」。
//
// 只在未成功时有意义；已提交（SSE）的响应无法撤回，successful() 恒 true，
// 不会走到需要分类的路径。
func (c *streamCaptureWriter) failClass() FailClass {
	if c.successful() {
		return FailOK
	}
	if c.status >= 200 && c.status < 300 {
		// 2xx 但没内容：空壳（由 successful 判负）。
		return FailEmpty
	}
	return classifyFailure(c.status, c.buf.Bytes())
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
