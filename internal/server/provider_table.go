// provider_table.go 把「某家 provider 该怎么接」收敛到一张注册表。
//
// 为什么需要它：此前加一家 provider 要同时改四处 —— 单 provider 分发 switch、
// failover 分发 switch、validProviders 白名单、enabledProviders 的 if 链。
// 四处手工同步，漏一处就静默降级（实测踩过：概览页少两行、别名校验报未知）。
//
// 现在：Config 的扁平字段仍是**装配输入**（测试与 config 都按老样子填），
// NewHandler 一次性装成表，之后所有分发与展示逻辑只查表。
package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// ProviderEntry 是一个 provider 的接线描述。
//
// 字段刻意只描述「怎么调用」，不描述「能力」——能力在 internal/providercap。
type ProviderEntry struct {
	Name string
	// Chat 实际处理聊天的函数。workbuddy 允许为 nil，表示走网关自己的
	// chatCompletions 原生路径（由 NewHandler 兜底绑定）。
	Chat http.HandlerFunc
	// AliasHeader 把客户端请求的别名写进 X-Unified-Alias，供上游回显。
	AliasHeader bool
	// SessionID 注入会话粘性头（doubao / loomy 需要，同一轮对话不能被拆成两个会话）。
	SessionID bool
	// Bypass 绕过别名分发，直接进原生聊天（workbuddy），用上下文标记防递归。
	Bypass bool
	// Order 决定 /v1/models 与面板里的展示顺序，数值小在前。
	// 加一家 provider 只需在这里给一个号，不必再去改排序函数。
	Order int
	// Healthy 返回 (健康数, 总数)；nil 表示该家没有健康概念（进程内单实例）。
	Healthy func() (int, int)
	// Checkin 可选的签到入口（面板「立即签到」用）。
	Checkin func(ctx context.Context) (kind string, credit int64, message string, err error)
	// Panel 可选的面板信息（账号/额度快照）。
	Panel func(ctx context.Context) CodeArtsPanelInfo
	// Reason 未接线时给用户的提示（指向该检查什么）。
	Reason string
}

// buildProviderTable 把 Config 里的扁平字段装配成注册表。
//
// 这是**唯一**知道「哪家怎么接」的地方：以后加 provider 只需要在这里加一个分支，
// 分发、白名单、展示顺序都会自动跟上。
func (c Config) buildProviderTable() map[string]*ProviderEntry {
	t := make(map[string]*ProviderEntry, 5)

	// workbuddy 恒在（进程内原生路径），Chat 由 NewHandler 兜底绑定。
	t["workbuddy"] = &ProviderEntry{
		Name:   "workbuddy",
		Bypass: true,
		Order:  10,
	}

	// 注意：**已知 provider 全部入表**，用 Chat==nil 表示「未接线」。
	// 这样优先序校验能接受历史配置里尚未启用的名字（旧行为兼容），
	// 而 enabledProviders 只返回真正接线的那些。
	t["doubao"] = &ProviderEntry{
		Name:        "doubao",
		Chat:        c.DoubaoChat,
		AliasHeader: true,
		SessionID:   true,
		Order:       20,
		Healthy:     c.DoubaoHealth,
		Reason:      "检查 providers.doubao.enabled 与启动日志",
	}
	t["trae"] = &ProviderEntry{
		Name:        "trae",
		Chat:        c.TraeChat,
		AliasHeader: true,
		Order:       30,
		Reason:      "检查 providers.trae.enabled 与 Trae2api 是否在线",
	}
	t["loomy"] = &ProviderEntry{
		Name:        "loomy",
		Chat:        c.LoomyChat,
		AliasHeader: true,
		SessionID:   true,
		Order:       40,
		Reason:      "检查启动日志与 loomy/accounts.json",
	}
	t["codearts"] = &ProviderEntry{
		Name:        "codearts",
		Chat:        c.CodeArtsChat,
		AliasHeader: true,
		Order:       50,
		Healthy:     c.CodeArtsHealth,
		Checkin:     c.CodeArtsCheckin,
		Panel:       c.CodeArtsPanel,
		Reason:      "检查 providers.codearts.enabled 与 codearts/accounts.json",
	}

	return t
}

// provider 查表；未接线返回 nil。
func (h *Handler) provider(name string) *ProviderEntry {
	if h.providers == nil {
		return nil
	}
	return h.providers[name]
}

// invokeProvider 是**唯一**知道「怎么调用一家 provider」的函数。
//
// 单 provider 路径与 failover 路径都走它，所以 session id / 别名头 / bypass
// 三处差异只写一遍 —— 这正是此前两条 switch 各自维护时最容易漏的地方。
func (h *Handler) invokeProvider(ctx context.Context, w http.ResponseWriter, r *http.Request,
	e *ProviderEntry, rewrittenBody []byte, aliasName string) {

	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(rewrittenBody))
	r2.ContentLength = int64(len(rewrittenBody))
	r2.Header = r.Header.Clone()

	if e.AliasHeader {
		r2.Header.Set("X-Unified-Alias", aliasName)
	}
	if e.SessionID {
		r2.Header.Set(HeaderSessionID, doubaoSessionID(r))
	}
	if e.Bypass {
		r2 = r2.WithContext(context.WithValue(r2.Context(), ctxUnifiedBypass{}, true))
	}
	e.Chat(w, r2)
}
