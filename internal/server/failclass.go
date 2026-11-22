// failclass.go 统一的失败分类：决定一次上游失败「要不要切下一家」。
//
// 为什么需要它：现在所有非 2xx 一视同仁地切换，于是一个**请求参数错误**会让
// 3 家候选各跑一遍 —— 慢、费钱、还可能触发风控。反过来，有些失败（模型未开通）
// 恰恰最该切，但被混在通用 4xx 里。
//
// 分类依据分两层，优先级从高到低：
//  1. OpenAI 错误体里的 `error.code` —— 各 provider 的 adapter 已经把自己的业务码
//     翻译成统一字符串（如 codearts 的 TM.000010051 → model_not_enabled），
//     所以这里不需要认识五家的私有码；
//  2. HTTP 状态码兜底 —— 只对**明确**是「请求本身有问题」的形态判不切换，
//     其余不明原因一律按可切换处理（保守：宁可多试一家，也不要让用户拿不到回复）。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// FailClass 是一次上游失败的语义分类。
type FailClass string

const (
	FailOK FailClass = "ok"
	// FailTransient 5xx / 超时 / 网络抖动：可重试、可切换。
	FailTransient FailClass = "transient"
	// FailAuth 认证失效：应刷新凭据或换号；换 provider 也可能有用。
	FailAuth FailClass = "auth"
	// FailQuota 额度耗尽：换号 → 换 provider。
	FailQuota FailClass = "quota"
	// FailRateLimit 限流：换号 → 换 provider。
	FailRateLimit FailClass = "rate_limit"
	// FailModelUnavailable 模型未开通/不存在/无权限：**应该切**，换一家可能真有。
	FailModelUnavailable FailClass = "model_unavailable"
	// FailUnsupportedInput 图片/工具等输入不被支持：可切到支持的那家。
	FailUnsupportedInput FailClass = "unsupported_input"
	// FailBadRequest 请求本身有问题：**不切换**（换到哪家都一样错）。
	FailBadRequest FailClass = "bad_request"
	// FailRefused 内容安全拒答：**不切换**（换一家同样拒，且可能触发风控）。
	FailRefused FailClass = "refused"
	// FailEmpty 2xx 但没有有效结果（空 choices / 空 body）：**应该切**。
	FailEmpty FailClass = "empty"
)

// Retryable 表示这次失败值不值得换下一家/换号。
//
// 只有 bad_request 与 refused 返回 false —— 这两类换到哪都一样。
func (f FailClass) Retryable() bool {
	switch f {
	case FailBadRequest, FailRefused:
		return false
	default:
		return true
	}
}

// explain 给日志/面板用的一句话说明（不面向终端用户）。
func (f FailClass) explain() string {
	switch f {
	case FailTransient:
		return "上游暂时不可用"
	case FailAuth:
		return "认证失效"
	case FailQuota:
		return "额度耗尽"
	case FailRateLimit:
		return "限流"
	case FailModelUnavailable:
		return "模型未开通/不存在"
	case FailUnsupportedInput:
		return "不支持该输入类型"
	case FailBadRequest:
		return "请求参数有误"
	case FailRefused:
		return "内容安全拒答"
	case FailEmpty:
		return "上游返回空结果"
	default:
		return string(f)
	}
}

// classifyFailure 按错误体 + 状态码给出分类。
//
// 有 body 时优先看 error.code（adapter 已归一化的业务码），没有再看状态码。
func classifyFailure(status int, body []byte) FailClass {
	if status >= 200 && status < 300 {
		// 2xx 但走到这里，说明是语义失败（空壳），由 successful() 判负。
		return FailEmpty
	}

	if code, msg, typ := openAIErrorFields(body); code != "" || typ != "" || msg != "" {
		if cls, ok := classFromErrorCode(code); ok {
			return cls
		}
		if cls, ok := classFromErrorText(typ + " " + msg); ok {
			return cls
		}
	}

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return FailAuth
	case status == http.StatusNotFound:
		return FailModelUnavailable
	case status == http.StatusTooManyRequests:
		return FailRateLimit
	case status >= 500:
		return FailTransient
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity:
		// 400 里混着两件事：真的参数错（不该切）与上游用 400 表达的其他失败
		// （该切）。上面按 error.code/text 已经尽力分辨；到这里仍分辨不出的，
		// **保守按可切换**处理 —— 宁可多试一家，也不要让用户拿不到回复。
		return FailTransient
	}
	return FailTransient
}

// classFromErrorCode 把 adapter 归一化后的 error.code 映射成分类。
//
// 这些字符串来自各家 adapter 的翻译层（如 codearts 的 translateCodeArtsError），
// 所以本函数不需要认识任何一家的私有业务码。
func classFromErrorCode(code string) (FailClass, bool) {
	switch code {
	case "":
		return "", false
	case "invalid_request", "invalid_request_error", "bad_request",
		"upstream_waf_blocked":
		return FailBadRequest, true
	case "model_not_found", "model_not_enabled", "no_benefit_package":
		return FailModelUnavailable, true
	case "upstream_concurrency_limit", "upstream_rate_limited", "rate_limit_exceeded":
		return FailRateLimit, true
	case "quota_exceeded", "insufficient_quota", "credits_exhausted":
		return FailQuota, true
	case "upstream_credential_expired", "upstream_unauthorized", "invalid_api_key",
		"upstream_signature_invalid":
		return FailAuth, true
	case "image_not_supported", "unsupported_input":
		return FailUnsupportedInput, true
	case "content_filter", "content_policy_violation":
		return FailRefused, true
	case "upstream_error":
		return FailTransient, true
	}
	return "", false
}

// classFromErrorText 兜底：有些 provider 只在 message/type 里表达语义。
//
// 只匹配**足够specific**的短语，避免把普通报错误判成不切换。
func classFromErrorText(text string) (FailClass, bool) {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "invalid_request_error"):
		return FailBadRequest, true
	case strings.Contains(t, "context_length_exceeded"),
		strings.Contains(t, "maximum context length"),
		strings.Contains(t, "prompt is too long"):
		// 上下文超长：换到窗口更大的那家是有意义的，所以按可切换处理。
		return FailTransient, true
	case strings.Contains(t, "content_filter"), strings.Contains(t, "content policy"):
		return FailRefused, true
	case strings.Contains(t, "rate limit"), strings.Contains(t, "too many requests"):
		return FailRateLimit, true
	}
	return "", false
}

// openAIErrorFields 读 OpenAI 风格错误体的三个关键字段。
func openAIErrorFields(body []byte) (code, message, typ string) {
	if len(bytes.TrimSpace(body)) == 0 {
		return "", "", ""
	}
	var probe struct {
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return "", "", ""
	}
	if s, ok := probe.Error.Code.(string); ok {
		code = strings.TrimSpace(s)
	}
	return code, strings.TrimSpace(probe.Error.Message), strings.TrimSpace(probe.Error.Type)
}
