package server

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// quotaFetchTimeout 单个豆包账号额度查询的上游超时。
//
// /admin/quota 会真打豆包 commerce 的 quota/summary，跨公网；面板不能被动等它。
// 取 8s：实测该接口通常 1-3s 返回，8s 足够覆盖抖动；超时的账号如实标记 error，
// 不阻塞其它账号，也不影响号池页其它列。
const quotaFetchTimeout = 8 * time.Second

// panelQuota 采集豆包账号额度，供号池页「积分」列使用。
//
// 口径差异（必须在 UI 上区分，不能混为一谈）：
//   - WorkBuddy 的 credits 是账号积分余额，由 /pool 直接返回；
//   - 豆包的额度来自 commerce/sale/subscription/quota/summary 的 window_limits 累加，
//     字段是 total / used / remaining（额度），不是「积分」。
//
// 返回 {"enabled":bool,"accounts":[...],"fetched_at":unix}；
// 单个账号失败时该项带 error 字段，整体仍 200——面板照常显示其它账号。
func (h *Handler) panelQuota(w http.ResponseWriter, r *http.Request) {
	if h.cfg.DoubaoAdmin == nil {
		panelJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "accounts": []any{},
			"note": "豆包 provider 未启用",
		})
		return
	}

	list, st := h.doubaoAdmin(http.MethodGet, "/admin/accounts", nil)
	if st != http.StatusOK {
		panelJSON(w, st, map[string]any{
			"enabled": true, "accounts": []any{},
			"error": "账号列表不可用", "detail": list["error"],
		})
		return
	}
	raw, _ := list["accounts"].([]any)
	names := make([]string, 0, len(raw))
	for _, it := range raw {
		if m, ok := it.(map[string]any); ok {
			if n, _ := m["name"].(string); n != "" {
				names = append(names, n)
			}
		}
	}

	type item struct {
		Name      string           `json:"name"`
		Total     float64          `json:"total"`
		Used      float64          `json:"used"`
		Remaining float64          `json:"remaining"`
		Percent   float64          `json:"percent"`
		Groups    []map[string]any `json:"groups,omitempty"`
		Error     string           `json:"error,omitempty"`
	}
	out := make([]item, len(names))

	// 并发查询：账号数很少（当前 1 个），但串行会让面板随账号线性变慢。
	// 每个 goroutine 用自己的 ch 与 captureWriter（doubaoAdminCtx 内部新建），无共享写。
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			out[i] = item{Name: name}
			// 取消传播：客户端断开时不再继续等上游。
			ctx, cancel := context.WithTimeout(r.Context(), quotaFetchTimeout)
			defer cancel()
			q, status := h.doubaoAdminCtx(ctx, http.MethodGet, "/admin/quota?name="+url.QueryEscape(name), nil)
			if status != http.StatusOK {
				out[i].Error = "provider 返回 " + http.StatusText(status)
				if e, ok := q["error"].(string); ok && e != "" {
					out[i].Error = e
				}
				return
			}
			if e, ok := q["error"].(string); ok && e != "" {
				// 上游有明确错误（cookie 失效、额度接口变更等），如实透出。
				out[i].Error = e
			}
			out[i].Total = numOfAny(q["total"])
			out[i].Used = numOfAny(q["used"])
			out[i].Remaining = numOfAny(q["remaining"])
			out[i].Percent = numOfAny(q["percent"])
			if gs, ok := q["groups"].([]any); ok {
				for _, g := range gs {
					if m, ok := g.(map[string]any); ok {
						out[i].Groups = append(out[i].Groups, m)
					}
				}
			}
		}(i, name)
	}
	wg.Wait()

	panelJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "accounts": out, "fetched_at": time.Now().Unix(),
		"note": "豆包为额度（total/used/remaining），与 WorkBuddy 积分口径不同",
	})
}

// numOfAny JSON 数字统一按 float64 处理（标准库 json.Unmarshal 到 map[string]any 的默认类型）。
func numOfAny(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	return 0
}
