// activity.go 豆包 provider 侧的请求记录环形缓冲。
//
// 为什么放在 dbserver：豆包请求在 dbserver 内就写完响应（统一层的 dispatchUnifiedAlias
// 直接 return），统一层拿不到它的状态码与所选账号；而账号名与模型在此处天然可得。
// 记录经 loopback 管理端点 /admin/activity 暴露，仅统一面板读取，不对外。
//
// 只存元数据，不落请求体 / Cookie / api key。
package dbserver

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

const dbActivityCap = 200

type dbActivityEntry struct {
	At      time.Time `json:"at"`
	Account string    `json:"account"`
	Model   string    `json:"model"`
	// ModelName 是 Model 内部 key 的可读名（例：auto 的 key "9" → "auto"）。
	// 面板「实际所用模型」以它为准，避免把内部数字 key 当模型名展示。
	ModelName string `json:"model_name,omitempty"`
	Alias     string `json:"alias,omitempty"`
	Status    int    `json:"status"`
	MS        int64  `json:"ms"`
	Stream    bool   `json:"stream"`
	Tokens    int    `json:"tokens"` // -1 = 未采集（豆包侧不回 usage 时保持 -1）
}

type dbActivityStore struct {
	mu    sync.Mutex
	items []dbActivityEntry
	next  int
}

var dbActivity = &dbActivityStore{items: make([]dbActivityEntry, dbActivityCap)}

func recordDBActivity(e dbActivityEntry) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	dbActivity.mu.Lock()
	defer dbActivity.mu.Unlock()
	dbActivity.items[dbActivity.next] = e
	dbActivity.next = (dbActivity.next + 1) % len(dbActivity.items)
}

// snapshot 最近 n 条（新的在前）；未写满时遇到零值槽位即止，不伪造历史。
func (s *dbActivityStore) snapshot(n int) []dbActivityEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.items) {
		n = len(s.items)
	}
	out := make([]dbActivityEntry, 0, n)
	for i := 1; i <= len(s.items) && len(out) < n; i++ {
		e := s.items[(s.next-i+len(s.items))%len(s.items)]
		if e.At.IsZero() {
			break
		}
		out = append(out, e)
	}
	return out
}

// adminActivity GET /admin/activity?limit=N 最近请求记录（loopback 面板专用）。
func (s *Server) adminActivity(w http.ResponseWriter, r *http.Request) {
	limit := dbActivityCap
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": dbActivity.snapshot(limit), "capacity": dbActivityCap})
}
