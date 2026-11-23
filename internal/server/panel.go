// Package server 统一控制面板 API。
//
// 设计约束：
//   - 面板前端不接触 API Key：所有后端调用在进程内完成，只由 Basic Auth 保护。
//   - 面板由统一进程在单端口提供（Config.PanelEnabled 开启时注册），不另起服务。
//   - 任何 provider 未启用时返回明确的 503，不静默伪造数据。
package server

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// panelIndexHTML 由统一进程直接托管，单端口提供面板与 API。
//
//go:embed panelweb/index.html
var panelIndexHTML []byte

// panelBuildStamp 面板构建标识，注入到页面顶栏。
//
// 用途：面板反复出现「改了但页面还是旧样子」的沟通成本，根因往往是浏览器/客户端
// 加载的是旧页面。顶栏显示构建号后，用户一眼就能判断是否已加载新版本；每次整改
// 面板就递增一次 hi 标记（r28 = 第 28 轮）。
const panelBuildStamp = "面板 r31 · 2026-11-22"

func (h *Handler) panelRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(bytes.ReplaceAll(panelIndexHTML, []byte("{{PANEL_BUILD}}"), []byte(panelBuildStamp)))
}

// captureWriter 进程内 HTTP 调用的最小 ResponseWriter（不引入 httptest 到生产代码）。
type captureWriter struct {
	hdr    http.Header
	status int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header {
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}
func (c *captureWriter) WriteHeader(s int) {
	if c.status == 0 {
		c.status = s
	}
}
func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(p)
}
func (c *captureWriter) Flush() {}

// doubaoAdmin 进程内调用豆包 provider 的管理端点，返回解析后的 JSON 与 HTTP 状态。
func (h *Handler) doubaoAdmin(method, path string, body []byte) (map[string]any, int) {
	return h.doubaoAdminCtx(context.Background(), method, path, body)
}

// doubaoAdminCtx 同 doubaoAdmin，但允许调用方用 ctx 控制上游耗时。
// 用于打上游外网的端点（如 /admin/quota 会请求豆包 commerce），避免面板被上游拖住。
func (h *Handler) doubaoAdminCtx(ctx context.Context, method, path string, body []byte) (map[string]any, int) {
	if h.cfg.DoubaoAdmin == nil {
		return map[string]any{"error": "doubao provider 未启用"}, http.StatusServiceUnavailable
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1"+path, rd)
	if err != nil {
		return map[string]any{"error": err.Error()}, http.StatusBadRequest
	}
	req.RemoteAddr = "127.0.0.1:0" // 通过 dbserver 的 loopback 管理闸
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := &captureWriter{}
	h.cfg.DoubaoAdmin.ServeHTTP(rec, req)
	out := map[string]any{}
	if err := json.Unmarshal(rec.body.Bytes(), &out); err != nil {
		return map[string]any{"error": "provider 响应不是 JSON"}, rec.status
	}
	return out, rec.status
}

func panelJSON(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, v)
}

// panelOverview 聚合两个 provider 的健康快照。
func (h *Handler) panelOverview(w http.ResponseWriter, _ *http.Request) {
	total, healthy, cooling, disabled, inFlight := h.cfg.Pool.CountsDetailed()
	db, dbStatus := h.doubaoAdmin(http.MethodGet, "/admin/status", nil)

	// 账号异常摘要（主页一屏看全，不搬整张表）：从 Pool.List() 只读快照计算。
	manualStopped, needsRelogin := 0, 0
	for _, st := range h.cfg.Pool.List() {
		if st.ManualDisabled {
			manualStopped++
		}
		if st.TokenExpired || st.NeedsRelogin {
			needsRelogin++
		}
	}
	// 别名统计：自动切换（多 provider + failover）与独立别名各多少。
	autoAliases, singleAliases := 0, 0
	for _, a := range h.Aliases() {
		if a.Failover && len(a.Candidates()) > 1 {
			autoAliases++
		} else {
			singleAliases++
		}
	}

	panelJSON(w, http.StatusOK, map[string]any{
		"service":         "unified-api-v2",
		"aliases":         len(h.Aliases()),
		"selection_mode":  h.cfg.Pool.SelectionMode(),
		"providers":       h.providerStatus(),
		"workbuddy":       map[string]any{"total": total, "healthy": healthy, "cooling": cooling, "disabled": disabled, "manual_stopped": manualStopped, "needs_relogin": needsRelogin, "in_flight_full": inFlight, "ok": true},
		"doubao":          map[string]any{"data": db, "ok": dbStatus == http.StatusOK},
		"alias_breakdown": map[string]any{"auto": autoAliases, "single": singleAliases},
		"provider_order":  h.providerOrderView(),
		// 用量：今日/累计（持久化在 data/usage.json，重启不清零）。
		"usage":      usageGlobal.Snapshot(),
		"checked_at": time.Now().Unix(),
	})
}

// panelAccounts 统一账号视图：两个 provider 的账号分列，不下发任何 Cookie/Token。
func (h *Handler) panelAccounts(w http.ResponseWriter, _ *http.Request) {
	wb := h.cfg.Pool.List()
	db, dbStatus := h.doubaoAdmin(http.MethodGet, "/admin/accounts", nil)
	panelJSON(w, http.StatusOK, map[string]any{
		"workbuddy":  wb,
		"doubao":     db,
		"doubao_ok":  dbStatus == http.StatusOK,
		"checked_at": time.Now().Unix(),
	})
}

// panelPool 号池状态 + 签到历史。
func (h *Handler) panelPool(w http.ResponseWriter, _ *http.Request) {
	var checkins []CheckinRecord
	if h.cfg.CheckinHistory != nil {
		checkins = h.cfg.CheckinHistory.Snapshot()
	}
	db, dbStatus := h.doubaoAdmin(http.MethodGet, "/admin/checkin", nil)
	panelJSON(w, http.StatusOK, map[string]any{
		"workbuddy": map[string]any{
			"accounts": h.cfg.Pool.List(),
			"checkins": checkins,
		},
		"doubao":     db,
		"doubao_ok":  dbStatus == http.StatusOK,
		"checked_at": time.Now().Unix(),
	})
}

// panelKeys 客户端接入信息：只暴露统一入口的 API 地址与唯一密钥（明文，
// 供一键复制）。不再展示 WorkBuddy / 豆包各自的内部密钥——客户端永远只用
// 统一层的 api_key，provider 密钥属于内部实现，暴露只会造成误用。
//
// 明文下发的前提：面板仅绑定本机 + cookie/Basic Auth 双保护（见 withPanelAuth），
// 且登录页需密码。若将来面板暴露到非本机地址，必须改回掩码或去掉明文。
func (h *Handler) panelKeys(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1:8900"
	}
	apiKey := h.cfg.APIKey
	// AuthKeys 非空时，主密钥以 name=master 或无分组的密钥为准；保持与 withAuth
	// 的口径一致——客户端用 config.api_key 即可，这里优先给 APIKey。
	if apiKey == "" && len(h.cfg.AuthKeys) > 0 {
		for _, k := range h.cfg.AuthKeys {
			if k.Name == "master" || len(k.Groups) == 0 {
				apiKey = k.Key
				break
			}
		}
	}
	panelJSON(w, http.StatusOK, map[string]any{
		"api_url": scheme + "://" + host + "/v1",
		"api_key": apiKey,
		"models":  h.aliasNames(),
	})
}

func maskPanelKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return k[:2] + "***"
	}
	return k[:4] + "***" + k[len(k)-4:]
}

// panelModels 全局别名表（模型页直接可编辑的配置来源）。
func (h *Handler) panelModels(w http.ResponseWriter, _ *http.Request) {
	all := h.aliasNames()
	// 面板展示顺序：具备自动切换（多 provider + failover）的公共别名排前，
	// 各侧独立别名排后；组内按别名稳定排序。/v1/models 仍用 aliasNames() 字母序，
	// 协议输出不受影响（只改面板展示，零回归）。
	autoNames := make([]string, 0, len(all))
	singleNames := make([]string, 0, len(all))
	for _, name := range all {
		a := h.Aliases()[name]
		if a.Failover && len(a.Candidates()) > 1 {
			autoNames = append(autoNames, name)
		} else {
			singleNames = append(singleNames, name)
		}
	}
	ordered := append(autoNames, singleNames...)
	aliases := make([]map[string]any, 0, len(ordered))
	for i, name := range ordered {
		a := h.Aliases()[name]
		// providers 用 Candidates()（含多候选）；provider 保留单值兼容旧前端。
		// 公共模型（两侧都提供）会以多候选形式返回，前端据此显示 workbuddy|doubao。
		aliases = append(aliases, map[string]any{
			"alias": name, "provider": a.Provider, "providers": a.Candidates(),
			"failover": a.Failover, "model": a.Model, "auto_order": i + 1,
			"name": a.Name, "context_length": a.ContextLength,
		})
	}
	panelJSON(w, http.StatusOK, map[string]any{"aliases": aliases, "total": len(aliases)})
}

// panelLogs 聚合可见的运维记录：签到历史 + 豆包用量摘要。
// 不暴露 Cookie / Token / 完整请求体。
func (h *Handler) panelLogs(w http.ResponseWriter, _ *http.Request) {
	var checkins []CheckinRecord
	if h.cfg.CheckinHistory != nil {
		checkins = h.cfg.CheckinHistory.Snapshot()
	}
	db, dbStatus := h.doubaoAdmin(http.MethodGet, "/admin/usage?days=7", nil)
	panelJSON(w, http.StatusOK, map[string]any{
		"checkins": checkins, "doubao_usage": db, "doubao_ok": dbStatus == http.StatusOK,
		"usage": usageGlobal.Snapshot(),
	})
}

// panelUsage 用量：WorkBuddy 当日预算 + 豆包按密钥统计。
func (h *Handler) panelUsage(w http.ResponseWriter, _ *http.Request) {
	used, limit, rejected := h.budget.snapshot()
	db, dbStatus := h.doubaoAdmin(http.MethodGet, "/admin/usage?days=7", nil)
	panelJSON(w, http.StatusOK, map[string]any{
		"workbuddy": map[string]any{"used": used, "limit": limit, "rejected": rejected, "day": cstDay(time.Now())},
		"doubao":    db, "doubao_ok": dbStatus == http.StatusOK,
	})
}

// panelCheckinGet 签到状态：WorkBuddy 下次时点 + 豆包最近一次结果。
func (h *Handler) panelCheckinGet(w http.ResponseWriter, _ *http.Request) {
	var next any
	if h.cfg.CheckinSchedule != nil {
		next = h.cfg.CheckinSchedule.NextCheckinAt(time.Now())
	}
	db, dbStatus := h.doubaoAdmin(http.MethodGet, "/admin/checkin", nil)
	// 豆包签到历史：内存快照（db.results）只留每账号最新一条且重启清零，
	// 真正的历史在 usage_checkin.jsonl，由 /admin/checkin/log 倒序读出。
	dbLog, dbLogStatus := h.doubaoAdmin(http.MethodGet, "/admin/checkin/log?limit=50", nil)
	panelJSON(w, http.StatusOK, map[string]any{
		"workbuddy":     map[string]any{"next": next},
		"doubao":        db,
		"doubao_ok":     dbStatus == http.StatusOK,
		"doubao_log":    dbLog,
		"doubao_log_ok": dbLogStatus == http.StatusOK,
	})
}

// panelCheckinPost 立即签到：两个 provider 各跑一次。WorkBuddy 走进程内 CheckinFn，
// 豆包走自己的 /admin/checkin；任一失败不影响另一方的执行与结果展示。
func (h *Handler) panelCheckinPost(w http.ResponseWriter, r *http.Request) {
	var wb any
	if h.cfg.CheckinFn != nil {
		rep, busy, err := h.cfg.CheckinFn()
		wb = map[string]any{"report": rep, "busy": busy, "err": errText(err), "ok": err == nil && !busy}
	} else {
		wb = map[string]any{"ok": false, "err": "签到未接线"}
	}
	db, dbStatus := h.doubaoAdmin(http.MethodPost, "/admin/checkin", nil)
	// Trae：外部进程，接线时一并签到。单家失败只影响自己的 ok，不让整体报错。
	var tr any
	if h.cfg.TraeAdmin != nil {
		out, st := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodPost, "/api/checkin/claim-all", nil)
		tr = map[string]any{"ok": st >= 200 && st < 300, "status": st, "data": out}
	} else {
		tr = map[string]any{"ok": false, "err": "Trae 未接线"}
	}
	// CodeArts：进程内 provider，签到与 workbuddy 同源（都由 scheduler 的
	// ExtraCheckins 承担），这里给面板一个即时入口。
	var ca any
	if h.cfg.CodeArtsCheckin != nil {
		kind, credit, msg, err := h.cfg.CodeArtsCheckin(r.Context())
		ca = map[string]any{
			"ok": err == nil && kind != "failed", "kind": kind,
			"credit": credit, "message": msg, "err": errText(err),
		}
	} else {
		ca = map[string]any{"ok": false, "err": "CodeArts 未接线"}
	}
	panelJSON(w, http.StatusOK, map[string]any{
		"workbuddy": wb, "doubao": db, "doubao_ok": dbStatus == http.StatusOK,
		"trae": tr, "codearts": ca,
	})
}

// panelCheckinProvider 单 provider 签到，供任务页按 provider 分组的独立按钮调用。
func (h *Handler) panelCheckinProvider(w http.ResponseWriter, r *http.Request, provider string) {
	switch provider {
	case "workbuddy":
		if h.cfg.CheckinFn == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "checkin_not_wired", "WorkBuddy 签到未接线")
			return
		}
		rep, busy, err := h.cfg.CheckinFn()
		panelJSON(w, http.StatusOK, map[string]any{
			"provider": "workbuddy", "report": rep, "busy": busy, "err": errText(err),
			"ok": err == nil && !busy,
		})
	case "codearts":
		if h.cfg.CodeArtsCheckin == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "checkin_not_wired", "CodeArts 签到未接线")
			return
		}
		kind, credit, msg, err := h.cfg.CodeArtsCheckin(r.Context())
		panelJSON(w, http.StatusOK, map[string]any{
			"provider": "codearts", "kind": kind, "credit": credit, "message": msg,
			"err": errText(err), "ok": err == nil && kind != "failed",
		})
	case "doubao":
		if h.cfg.DoubaoAdmin == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable", "豆包 provider 未启用")
			return
		}
		db, status := h.doubaoAdmin(http.MethodPost, "/admin/checkin", nil)
		panelJSON(w, status, map[string]any{
			"provider": "doubao", "data": db, "ok": status >= 200 && status < 300,
		})
	default:
		writeOpenAIError(w, http.StatusBadRequest, "bad_provider", "未知 provider: "+provider)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// panelSettings 只读配置快照（不含任何密钥明文）。
func (h *Handler) panelSettings(w http.ResponseWriter, r *http.Request) {
	panelJSON(w, http.StatusOK, map[string]any{
		"service": "unified-api-v2",
		"listen":  r.Host,
		"providers": map[string]any{
			"workbuddy": true,
			"doubao":    h.cfg.DoubaoAdmin != nil,
		},
		"aliases": h.aliasNames(),
		"admin":   h.cfg.AdminEnabled,
		"metrics": h.cfg.MetricsEnabled,
	})
}

// panelPoolMode 运行时切换选号模式（priority / weighted）。
//
// 只改内存中的 pool 行为，不回写 config.json：面板写配置文件会重排用户手写的
// 字段与注释，且一旦写坏就没有备份可用——模式持久化仍由配置文件负责（页面已标注）。
func (h *Handler) panelPoolMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_json", "参数错误: "+err.Error())
		return
	}
	mode := strings.ToLower(strings.TrimSpace(body.Mode))
	if mode != "priority" && mode != "weighted" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_mode", `mode 只能是 "priority" 或 "weighted"`)
		return
	}
	h.cfg.Pool.SetSelectionMode(mode)
	panelJSON(w, http.StatusOK, map[string]any{
		"mode":      h.cfg.Pool.SelectionMode(),
		"persisted": false,
		"note":      "本次运行生效；重启后按 config.json 的 pool.selection_mode 恢复",
	})
}

// panelProviderOrder 读取/设置「自动路由的 Provider 优先序」。
//
// GET  → 当前生效顺序（含来源：override=运行时偏好文件 / config=别名配置原序）
// POST → {"first":"doubao"|"workbuddy"}：把该侧排第一，另一侧排第二。
//
// 与 pool/mode 的差异：优先序落在数据目录的独立小文件（provider_order.json），
// 重启后仍然生效；config.json 不动，避免重排用户手写的主配置。
func (h *Handler) panelProviderOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		panelJSON(w, http.StatusOK, h.providerOrderView())
		return
	}
	var body struct {
		First string   `json:"first"`
		Order []string `json:"order"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_json", "参数错误: "+err.Error())
		return
	}
	order := body.Order
	if len(order) == 0 {
		first := strings.ToLower(strings.TrimSpace(body.First))
		if first == "" {
			writeOpenAIError(w, http.StatusBadRequest, "bad_request", `需要 first（"doubao" 或 "workbuddy"）或 order 数组`)
			return
		}
		other := "workbuddy"
		if first == "workbuddy" {
			other = "doubao"
		} else if first != "doubao" {
			writeOpenAIError(w, http.StatusBadRequest, "bad_provider", `first 只能是 "doubao" 或 "workbuddy"`)
			return
		}
		order = []string{first, other}
	}
	persisted, err := h.SetProviderOrder(order)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_order", "优先序非法："+err.Error())
		return
	}
	out := h.providerOrderView()
	out["persisted"] = persisted
	if !persisted {
		out["note"] = "已在内存生效；未落盘（ProviderOrderFile 未配置），重启后按 config 恢复"
	}
	panelJSON(w, http.StatusOK, out)
}

// providerOrderView 输出当前优先序与来源，供概览页直接渲染控件。
func (h *Handler) providerOrderView() map[string]any {
	raw := h.ProviderOrder()
	effective := raw
	source := "override"
	if len(effective) == 0 {
		source = "config"
		effective = h.configProviderOrder()
	}
	// 展示层补齐：历史偏好文件里没有 trae 时也要显示该行，否则面板像少了 provider。
	effective = h.fillProviders(effective)
	return map[string]any{
		"order":     effective,
		"source":    source,
		"providers": h.enabledProviders(), // 已接线 provider（含 trae），供面板渲染行
	}
}

// configProviderOrder 取各自动路由别名里出现次数最多的候首次序（用于展示「当前配置
// 决定的默认顺序」）。简化为出现最多的首候选：配置里所有公共别名应保持一致。
func (h *Handler) configProviderOrder() []string {
	counts := map[string]int{}
	for _, a := range h.Aliases() {
		c := a.Candidates()
		if len(c) > 0 {
			counts[c[0]]++
		}
	}
	out := make([]string, 0, len(counts))
	for _, p := range []string{"doubao", "workbuddy"} {
		if counts[p] > 0 {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"doubao", "workbuddy"}
	}
	return out
}

// panelTaskRun 手动触发 WorkBuddy 排程任务（六类白名单）。
func (h *Handler) panelTaskRun(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if h.cfg.Tasks == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "task_not_wired", "任务调度未接线")
		return
	}
	var accepted bool
	switch name {
	case "checkin":
		h.cfg.Tasks.RunCheckinNow()
		accepted = true
	case "activity":
		h.cfg.Tasks.RunActivityNow()
		accepted = true
	case "keepalive":
		h.cfg.Tasks.RunKeepaliveNow()
		accepted = true
	case "travel":
		h.cfg.Tasks.RunTravelNow()
		accepted = true
	case "school":
		h.cfg.Tasks.RunSchoolNow()
		accepted = true
	case "cat":
		h.cfg.Tasks.RunCatNow()
		accepted = true
	default:
		writeOpenAIError(w, http.StatusNotFound, "unknown_task", "未知任务: "+name)
		return
	}
	panelJSON(w, http.StatusAccepted, map[string]any{"task": name, "accepted": accepted})
}

// panelAccountToggle 统一账号启停。provider=workbuddy|doubao。
func (h *Handler) panelAccountToggle(w http.ResponseWriter, r *http.Request) {
	provider, name := r.PathValue("provider"), r.PathValue("name")
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch provider {
	case "workbuddy":
		r2 := r.Clone(r.Context())
		r2.SetPathValue("uid", name)
		raw := []byte(`{}`)
		if body.Disabled != nil {
			b, _ := json.Marshal(map[string]any{"disabled": *body.Disabled})
			raw = b
		}
		r2.Body = io.NopCloser(bytes.NewReader(raw))
		if body.Disabled != nil && *body.Disabled {
			h.adminAccountDisable(w, r2)
		} else {
			h.adminAccountEnable(w, r2)
		}
	case "doubao":
		raw := []byte(`{}`)
		if body.Disabled != nil {
			b, _ := json.Marshal(map[string]any{"disabled": *body.Disabled})
			raw = b
		}
		out, status := h.doubaoAdmin(http.MethodPost, "/admin/accounts/"+name+"/toggle", raw)
		panelJSON(w, status, out)
	default:
		writeOpenAIError(w, http.StatusNotFound, "unknown_provider", "未知 provider: "+provider)
	}
}

// panelAPI 统一面板 API 分发。
func (h *Handler) panelAPI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/panel")
	method := r.Method
	switch {
	case p == "/overview" && method == http.MethodGet:
		h.panelOverview(w, r)
	case p == "/activity" && method == http.MethodGet:
		h.panelActivity(w, r)
	case p == "/pool/mode" && method == http.MethodPost:
		h.panelPoolMode(w, r)
	case p == "/provider-order" && (method == http.MethodGet || method == http.MethodPost):
		h.panelProviderOrder(w, r)
	case p == "/accounts" && method == http.MethodGet:
		h.panelAccounts(w, r)
	case p == "/pool" && method == http.MethodGet:
		h.panelPool(w, r)
	case p == "/quota" && method == http.MethodGet:
		h.panelQuota(w, r)
	// Trae provider 管理代理（外部 Trae2api 进程）。精确路径在前、前缀在后。
	case p == "/trae/accounts" && method == http.MethodGet:
		h.panelTraeAccounts(w, r)
	case p == "/trae/overview" && method == http.MethodGet:
		h.panelTraeOverview(w, r)
	case p == "/trae/checkin-status" && method == http.MethodGet:
		h.panelTraeCheckinStatus(w, r)
	case p == "/trae/accounts/model-enabled" && method == http.MethodPost:
		h.panelTraeModelEnabled(w, r)
	case p == "/trae/accounts/remove" && method == http.MethodPost:
		h.panelTraeRemove(w, r)
	case p == "/trae/accounts/reorder" && method == http.MethodPost:
		h.panelTraeReorder(w, r)
	// Loomy：网关自己管理账号（无外部进程）。凭证来自专用 Edge profile 的登录。
	case p == "/codearts/accounts" && method == http.MethodGet:
		h.panelCodeArtsAccounts(w, r)
	// 导入/刷新：codearts 的凭据来自本机 IDE（或接管模式的 refresh_token），
	// 没有「用户名密码」可填，所以这里的「添加账号」就是「从 IDE 重新读一次」。
	case p == "/codearts/import" && method == http.MethodPost:
		h.panelCodeArtsImport(w, r)
	case p == "/codearts/login" && method == http.MethodGet:
		h.panelCodeArtsLogin(w, r)
	case p == "/codearts/checkin" && method == http.MethodPost:
		h.panelCodeArtsCheckin(w, r)
	case p == "/codearts/login" && method == http.MethodPost:
		h.panelCodeArtsLogin(w, r)
	case p == "/loomy/accounts" && method == http.MethodGet:
		h.panelLoomyAccounts(w, r)
	case p == "/doubao/import-client" && method == http.MethodPost:
		h.panelDoubaoImportClient(w, r)
	case p == "/doubao/import-profile" && method == http.MethodPost:
		h.panelDoubaoImportProfile(w, r)
	case p == "/doubao/login" && method == http.MethodPost:
		h.panelDoubaoLogin(w, r)
	case p == "/loomy/login" && method == http.MethodPost:
		h.panelLoomyLogin(w, r)
	case p == "/loomy/import-local" && method == http.MethodPost:
		h.panelLoomyImportLocal(w, r)
	case p == "/loomy/remove" && method == http.MethodPost:
		h.panelLoomyRemove(w, r)
	// 账号新增：import-local 读本机 Trae 客户端凭证（免跳转），accounts/add 为手动粘贴兜底。
	case p == "/trae/login-url" && method == http.MethodGet:
		h.panelTraeLoginURL(w, r)
	case p == "/trae/import-local" && method == http.MethodPost:
		h.panelTraeImportLocal(w, r)
	case p == "/trae/accounts/add" && method == http.MethodPost:
		h.panelTraeAddAccount(w, r)
	case p == "/trae/checkin" && method == http.MethodPost:
		h.panelTraeCheckinAll(w, r)
	case strings.HasPrefix(p, "/trae/checkin/") && method == http.MethodPost:
		h.panelTraeCheckinOne(w, r, strings.TrimPrefix(p, "/trae/checkin/"))
	case p == "/keys" && method == http.MethodGet:
		h.panelKeys(w, r)
	case p == "/models" && method == http.MethodGet:
		h.panelModels(w, r)
	case p == "/logs" && method == http.MethodGet:
		h.panelLogs(w, r)
	case p == "/usage" && method == http.MethodGet:
		h.panelUsage(w, r)
	case p == "/checkin" && method == http.MethodGet:
		h.panelCheckinGet(w, r)
	case p == "/checkin" && method == http.MethodPost:
		h.panelCheckinPost(w, r)
	case strings.HasPrefix(p, "/checkin/") && method == http.MethodPost:
		h.panelCheckinProvider(w, r, strings.TrimPrefix(p, "/checkin/"))
	case p == "/settings" && method == http.MethodGet:
		h.panelSettings(w, r)
	case strings.HasPrefix(p, "/tasks/") && strings.HasSuffix(p, "/run") && method == http.MethodPost:
		r2 := r.Clone(r.Context())
		r2.SetPathValue("name", strings.TrimSuffix(strings.TrimPrefix(p, "/tasks/"), "/run"))
		h.panelTaskRun(w, r2)
	case p == "/accounts/order" && method == http.MethodPost:
		h.panelAccountOrder(w, r)
	case strings.HasPrefix(p, "/accounts/") && strings.HasSuffix(p, "/toggle") && method == http.MethodPost:
		rest := strings.TrimSuffix(strings.TrimPrefix(p, "/accounts/"), "/toggle")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 {
			writeOpenAIError(w, http.StatusBadRequest, "bad_request", "路径格式为 /accounts/{provider}/{name}/toggle")
			return
		}
		r2 := r.Clone(r.Context())
		r2.SetPathValue("provider", parts[0])
		r2.SetPathValue("name", parts[1])
		h.panelAccountToggle(w, r2)
	case p == "/accounts/wb/import" && method == http.MethodPost:
		h.panelWBImport(w, r)
	case p == "/accounts/wb/oauth/url" && method == http.MethodPost:
		h.panelWBOAuthURL(w, r)
	case p == "/accounts/wb/oauth/poll" && method == http.MethodPost:
		h.panelWBOAuthPoll(w, r)
	case p == "/accounts/db/cookie" && method == http.MethodPost:
		h.panelDBCookie(w, r)
	case p == "/accounts/db/import-local" && method == http.MethodPost:
		h.panelDBImportLocal(w, r)
	case p == "/accounts/db/remove" && method == http.MethodPost:
		h.panelDBRemove(w, r)
	case p == "/test" && method == http.MethodPost:
		// 测试台复用统一聊天入口：别名路由、鉴权、预算与统计全部一致。
		// 固定独立会话标识：测试台的连续提问落进同一个豆包会话，又不与外部
		// 客户端的会话串台。
		r2 := r.Clone(r.Context())
		r2.Header = r.Header.Clone()
		r2.Header.Set(HeaderSessionID, "panel-test")
		h.chatCompletions(w, r2)
	default:
		writeOpenAIError(w, http.StatusNotFound, "panel_not_found", "未知面板端点: "+p)
	}
}
