// providerorder.go 「Provider 路由优先序」运行时状态 + 落盘。
//
// 语义：决定自动路由别名（多 provider + failover）在**同一次请求**里先试哪一侧。
// 与 config.json 里每个别名的 providers 数组同源，但这里是一处**全局**偏好：
// 改一次对所有自动路由别名同时生效，不用逐个别名改配置。
//
// 为什么单独落一个文件而不是回写 config.json：
//   - config.json 是用户手写的主配置，面板写回会重排字段与缩进（pool/mode 也因
//     这个原因选择只改内存）。
//   - 该偏好属于「运维现场决定」，与请求路径无关，放在数据目录（同 state.json
//     所在目录）的独立小文件即可，损坏/缺失时静默回落到 config 的原始顺序。
package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// validProviders 允许出现在优先序里的 provider 名（与 UnifiedAlias 的取值一致）。
var validProviders = map[string]bool{"workbuddy": true, "doubao": true, "trae": true}

// providerOrderPref 优先序的内存视图 + 持久化路径。
// path 为空 = 不落盘（测试或未配置），只在本进程生效。
type providerOrderPref struct {
	mu    sync.RWMutex
	order []string
	path  string
}

// loadProviderOrder 从文件读优先序；文件缺失/损坏/含非法项时返回 nil（= 按配置原序）。
func loadProviderOrder(path string) []string {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f struct {
		Order []string `json:"order"`
	}
	if json.Unmarshal(raw, &f) != nil {
		return nil
	}
	if _, err := normalizeProviderOrder(f.Order); err != nil {
		return nil
	}
	return f.Order
}

// normalizeProviderOrder 校验并规整优先序：只接受已知 provider、去重、保序。
// 返回错误时调用方必须原样拒绝，不允许「部分生效」。
func normalizeProviderOrder(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, os.ErrInvalid
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if !validProviders[p] {
			return nil, os.ErrInvalid
		}
		if seen[p] {
			return nil, os.ErrInvalid
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

// enabledProviders 返回当前已接线的 provider（固定顺序，供优先序展示与补齐用）。
// workbuddy 的本地池总是存在，所以恒在列表首位。
func (h *Handler) enabledProviders() []string {
	out := []string{"workbuddy"}
	if h.cfg.DoubaoChat != nil || h.cfg.DoubaoAdmin != nil {
		out = append(out, "doubao")
	}
	if h.cfg.TraeChat != nil || h.cfg.TraeAdmin != nil {
		out = append(out, "trae")
	}
	return out
}

// ProviderOrder 返回当前优先序的副本；nil 表示未设置（调用方按配置原序处理）。
//
// 注意：这里**不做**「补齐已接线 provider」。orderCandidates 用它重排真实请求，
// 补齐会让未设置偏好时也产生一个非空顺序，从而覆盖别名自身的配置顺序。
// 展示层需要补齐请用 fillProviders。
func (h *Handler) ProviderOrder() []string {
	if h.provOrder == nil {
		return nil
	}
	h.provOrder.mu.RLock()
	defer h.provOrder.mu.RUnlock()
	if len(h.provOrder.order) == 0 {
		return nil
	}
	out := make([]string, len(h.provOrder.order))
	copy(out, h.provOrder.order)
	return out
}

// fillProviders 在给定顺序末尾补齐「已接线但未列出」的 provider。
//
// 场景：provider_order.json 是加入 trae 之前写的（只有 doubao/workbuddy），
// 若原样展示，面板就少一行，表现为「新接的 Trae 看不见」。补齐只影响展示与
// 面板上的顺序编辑，真实路由顺序仍以别名配置 + orderCandidates 为准。
func (h *Handler) fillProviders(base []string) []string {
	out := append([]string{}, base...)
	for _, p := range h.enabledProviders() {
		found := false
		for _, q := range out {
			if q == p {
				found = true
				break
			}
		}
		if !found {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SetProviderOrder 设置优先序：先校验，再改内存，最后尽力落盘。
// 落盘失败不回滚内存——内存已生效，如实返回错误让面板提示「重启后会丢」，
// 比让用户以为操作没发生要好。
func (h *Handler) SetProviderOrder(in []string) (persisted bool, err error) {
	order, nerr := normalizeProviderOrder(in)
	if nerr != nil {
		return false, nerr
	}
	if h.provOrder == nil {
		h.provOrder = &providerOrderPref{}
	}
	h.provOrder.mu.Lock()
	h.provOrder.order = order
	path := h.provOrder.path
	h.provOrder.mu.Unlock()

	if path == "" {
		return false, nil
	}
	raw, merr := json.MarshalIndent(map[string]any{"order": order}, "", "  ")
	if merr != nil {
		return false, merr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	return true, nil
}
