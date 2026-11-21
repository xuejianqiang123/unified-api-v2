// activity.go 面板「实时使用」与「请求日志」数据源：请求级环形缓冲 + 活跃视图聚合。
//
// 设计约束：
//   - 只保留请求元数据（时间 / provider / 模型 / 别名 / 账号 / 状态 / 耗时 / token），
//     绝不落请求体、Cookie、密钥——面板因此不新增任何敏感面。
//   - WorkBuddy 的写入点唯一：chatStat.done()（全路径唯一出口，与 metrics 同一纪律）。
//     豆包请求在 dbserver 内部就写完了响应，统一层拿不到它的响应码，故由 dbserver 自己
//     记环形缓冲，经 loopback 管理端点 /admin/activity 取回后在此合流。两边各持一份、
//     不共享状态，避免跨包共享可变全局。
//   - 有界内存：容量固定，写满即覆盖最旧一条；进程重启清零（面板页明确标注窗口范围）。
//   - token 缺失记 -1（与日志行、metrics 的「缺失≠0」纪律一致），前端显示 "-"。
package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"unified-api-v2/internal/logfmt"
)

// activityCap 环形缓冲容量。500 条足够覆盖单机网关近数小时的排查窗口，
// 内存占用约 500×~120B ≈ 60KB，可忽略。
const activityCap = 500

// activityEntry 单次请求的展示元数据。
type activityEntry struct {
	At       time.Time `json:"at"`
	Provider string    `json:"provider"`
	Model    string    `json:"model"` // 上游实际模型 key（豆包为内部 modelKey，可能是数字）
	// ModelName 是 Model 的可读名（豆包内部 key "9" → "auto"）。
	// 面板「实际所用模型」以它为准；为空则回退 Model。
	ModelName string `json:"model_name,omitempty"`
	Alias     string `json:"alias,omitempty"`   // 客户端请求的别名；空 = 直连模型名
	Account   string `json:"account,omitempty"` // 展示用账号标签（"昵称(uid8)" 或豆包账号名）
	Status    int    `json:"status"`
	MS        int64  `json:"ms"`
	Stream    bool   `json:"stream"`
	Tokens    int    `json:"tokens"` // -1 = usage 缺失
}

// displayModel 面板展示用的模型名：可读名优先，缺失回退内部 key。
func (e activityEntry) displayModel() string {
	if e.ModelName != "" {
		return e.ModelName
	}
	return e.Model
}

type activityStore struct {
	mu    sync.Mutex
	items []activityEntry
	next  int
}

func newActivityStore(cap int) *activityStore {
	return &activityStore{items: make([]activityEntry, cap)}
}

// add 写入一条记录；写满后覆盖最旧位置。At 为零值时补当前时间。
func (s *activityStore) add(e activityEntry) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[s.next] = e
	s.next = (s.next + 1) % len(s.items)
}

// snapshot 返回最近 n 条（新的在前）。n<=0 或超过容量时取满容量。
// 未写满的槽位是零值，At 为零即视为空槽跳过——不伪造历史记录。
func (s *activityStore) snapshot(n int) []activityEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n <= 0 || n > len(s.items) {
		n = len(s.items)
	}
	out := make([]activityEntry, 0, n)
	for i := 1; i <= len(s.items) && len(out) < n; i++ {
		e := s.items[(s.next-i+len(s.items))%len(s.items)]
		if e.At.IsZero() {
			break // 环形缓冲从当前位置往前必然全是空槽
		}
		out = append(out, e)
	}
	return out
}

// wbActivity WorkBuddy 侧的请求记录（豆包侧在 dbserver 内部另记一份）。
var wbActivity = newActivityStore(activityCap)

// recordWBActivity 由 chatStat.done() 调用；不在其它位置重复埋点，避免双计。
func recordWBActivity(e activityEntry) { wbActivity.add(e) }

// displayAccount 账号展示标签：有昵称用 "昵称(uid8)"，否则只给 uid8（不泄露完整 uid）。
func displayAccount(uid, nick string) string {
	if uid == "" && nick == "" {
		return ""
	}
	return logfmt.Label(uid, nick)
}

// dbActivityItem 豆包 /admin/activity 的返回条目（字段与 dbserver 侧保持一致）。
type dbActivityItem struct {
	At        time.Time `json:"at"`
	Account   string    `json:"account"`
	Model     string    `json:"model"`
	ModelName string    `json:"model_name"`
	Alias     string    `json:"alias"`
	Status    int       `json:"status"`
	MS        int64     `json:"ms"`
	Stream    bool      `json:"stream"`
	Tokens    int       `json:"tokens"`
}

// dbAccountStat 豆包 /admin/status 的账号状态条目（只取面板需要的字段）。
type dbAccountStat struct {
	Name     string `json:"name"`
	Priority int    `json:"priority"`
	InFlight int    `json:"in_flight"`
	Disabled bool   `json:"disabled"`
	Cooling  bool   `json:"cooling"`
	Fails    int    `json:"fails"`
	Reason   string `json:"reason"`
}

// panelActivity GET /api/panel/activity 实时使用视图：
// 最近请求 + 按模型汇总 + 按账号汇总（含实时在途），供概览与请求日志两页共用。
//
// 任何 provider 不可用时对应段留空并给出 ok=false，不伪造数据。
func (h *Handler) panelActivity(w http.ResponseWriter, _ *http.Request) {
	recent := activitySnapshotMerged(h)

	// 账号实时视图：WorkBuddy 来自 Pool.List（在途/停用/冷却/优先级），
	// 豆包来自 provider 的 /admin/status（同上口径，字段名不同但语义一致）。
	byAccount := map[string]*accountAgg{}
	inFlight := map[string]int{"workbuddy": 0, "doubao": 0}
	for _, st := range h.cfg.Pool.List() {
		key := "workbuddy|" + displayAccount(st.UID, st.Nickname)
		a := byAccount[key]
		if a == nil {
			a = &accountAgg{Provider: "workbuddy", Account: displayAccount(st.UID, st.Nickname),
				Priority: st.Priority, Disabled: st.Disabled || st.ManualDisabled, Cooling: st.Cooling}
			byAccount[key] = a
		}
		a.InFlight = st.InFlight
		inFlight["workbuddy"] += st.InFlight
	}
	doubaoOK := h.cfg.DoubaoAdmin != nil
	if doubaoOK {
		if out, status := h.doubaoAdmin(http.MethodGet, "/admin/status", nil); status == http.StatusOK {
			var stats []dbAccountStat
			if decodeAny(out["accounts"], &stats) {
				for _, st := range stats {
					a := &accountAgg{Provider: "doubao", Account: st.Name, Priority: st.Priority,
						InFlight: st.InFlight, Disabled: st.Disabled, Cooling: st.Cooling}
					byAccount["doubao|"+st.Name] = a
					inFlight["doubao"] += st.InFlight
				}
			}
		}
	}

	// 请求级汇总：一条记录同时累加到模型维与账号维。
	byModel := map[string]*modelAgg{}
	for _, e := range recent {
		// 按「可读模型名」分组：同一次请求上下游可能给内部 key，与别名重复计成两组。
		mk := e.Provider + "|" + e.displayModel()
		m := byModel[mk]
		if m == nil {
			m = &modelAgg{Provider: e.Provider, Model: e.displayModel(), Alias: e.Alias}
			byModel[mk] = m
		}
		m.add(e)
		ak := e.Provider + "|" + e.Account
		a := byAccount[ak]
		if a == nil {
			a = &accountAgg{Provider: e.Provider, Account: e.Account}
			byAccount[ak] = a
		}
		a.add(e)
	}

	models := make([]modelAgg, 0, len(byModel))
	for _, m := range byModel {
		models = append(models, *m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Requests > models[j].Requests })

	accounts := make([]accountAgg, 0, len(byAccount))
	for _, a := range byAccount {
		accounts = append(accounts, *a)
	}
	// 排序：有在途的置顶 → 最近活动时间倒序 → 请求数倒序。运维先看「谁在干活」。
	sort.Slice(accounts, func(i, j int) bool {
		if (accounts[i].InFlight > 0) != (accounts[j].InFlight > 0) {
			return accounts[i].InFlight > 0
		}
		if !accounts[i].LastAt.Equal(accounts[j].LastAt) {
			return accounts[i].LastAt.After(accounts[j].LastAt)
		}
		return accounts[i].Requests > accounts[j].Requests
	})

	last5 := lastNMinutes(recent, 5)
	panelJSON(w, http.StatusOK, map[string]any{
		"selection_mode": h.cfg.Pool.SelectionMode(),
		"window":         map[string]any{"capacity": activityCap, "count": len(recent)},
		"in_flight":      inFlight,
		"recent":         recent,
		"models":         models,
		"accounts":       accounts,
		"last5m":         last5,
		"doubao_ok":      doubaoOK,
		"checked_at":     time.Now().Unix(),
	})
}

// activitySnapshotMerged 合并两侧记录并按时间倒序截断到容量上限。
// 豆包段取不到时静默降级为仅有 WorkBuddy 记录（面板另以 doubao_ok 标注）。
func activitySnapshotMerged(h *Handler) []activityEntry {
	recent := wbActivity.snapshot(activityCap)
	if h.cfg.DoubaoAdmin != nil {
		if out, status := h.doubaoAdmin(http.MethodGet, "/admin/activity?limit=200", nil); status == http.StatusOK {
			var items []dbActivityItem
			if decodeAny(out["entries"], &items) {
				for _, it := range items {
					recent = append(recent, activityEntry{
						At: it.At, Provider: "doubao", Model: it.Model, ModelName: it.ModelName, Alias: it.Alias,
						Account: it.Account, Status: it.Status, MS: it.MS, Stream: it.Stream, Tokens: it.Tokens,
					})
				}
			}
		}
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].At.After(recent[j].At) })
	if len(recent) > activityCap {
		recent = recent[:activityCap]
	}
	return recent
}

// modelAgg 单模型（provider+上游模型名）的请求汇总。
type modelAgg struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Alias    string    `json:"alias,omitempty"`
	Requests int       `json:"requests"`
	OK       int       `json:"ok"`
	Fail     int       `json:"fail"`
	Tokens   int64     `json:"tokens"`
	DurSumMS int64     `json:"-"`
	LastAt   time.Time `json:"last_at"`
	AvgMS    int64     `json:"avg_ms"`
}

func (m *modelAgg) add(e activityEntry) {
	m.Requests++
	if e.Status >= 200 && e.Status < 400 {
		m.OK++
	} else {
		m.Fail++
	}
	if e.Tokens > 0 {
		m.Tokens += int64(e.Tokens)
	}
	m.DurSumMS += e.MS
	m.AvgMS = m.DurSumMS / int64(m.Requests)
	if e.At.After(m.LastAt) {
		m.LastAt = e.At
	}
	// 别名以最近一次为准（同一别名可能映射到不同上游模型，展示当前生效的那个）。
	if e.Alias != "" {
		m.Alias = e.Alias
	}
}

// accountAgg 单账号汇总 + 实时状态（Provider 内账号名唯一）。
type accountAgg struct {
	Provider  string    `json:"provider"`
	Account   string    `json:"account"`
	Priority  int       `json:"priority"`
	Disabled  bool      `json:"disabled"`
	Cooling   bool      `json:"cooling"`
	InFlight  int       `json:"in_flight"`
	Requests  int       `json:"requests"`
	OK        int       `json:"ok"`
	Fail      int       `json:"fail"`
	Tokens    int64     `json:"tokens"`
	DurSumMS  int64     `json:"-"`
	LastAt    time.Time `json:"last_at"`
	LastModel string    `json:"last_model,omitempty"`
	AvgMS     int64     `json:"avg_ms"`
}

func (a *accountAgg) add(e activityEntry) {
	a.Requests++
	if e.Status >= 200 && e.Status < 400 {
		a.OK++
	} else {
		a.Fail++
	}
	if e.Tokens > 0 {
		a.Tokens += int64(e.Tokens)
	}
	a.DurSumMS += e.MS
	a.AvgMS = a.DurSumMS / int64(a.Requests)
	if e.At.After(a.LastAt) {
		a.LastAt = e.At
		a.LastModel = e.Model
		if a.Account == "" {
			a.Account = e.Account
		}
	}
}

// decodeAny 把 doubaoAdmin 返回的 any（已解析的 JSON）转成具体结构：
// 其中的数组是 []any，无法直接类型断言，走一次 marshal/unmarshal 最省事
// （也顺带保证字段名口径与 provider 侧完全一致）。
func decodeAny(v any, dst any) bool {
	raw, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// lastNMinutes 统计窗口内的请求数与成功率（两个 provider 分开，便于一眼看谁在扛流量）。
func lastNMinutes(recent []activityEntry, mins int) map[string]any {
	cut := time.Now().Add(-time.Duration(mins) * time.Minute)
	type c struct{ req, ok int }
	per := map[string]*c{"workbuddy": {}, "doubao": {}}
	okAll := 0
	for _, e := range recent {
		if e.At.Before(cut) {
			continue
		}
		p := per[e.Provider]
		if p == nil {
			p = &c{}
			per[e.Provider] = p
		}
		p.req++
		if e.Status >= 200 && e.Status < 400 {
			p.ok++
			okAll++
		}
	}
	out := map[string]any{"minutes": mins, "ok": okAll}
	for p, v := range per {
		out[p] = map[string]any{"requests": v.req, "ok": v.ok}
	}
	return out
}
