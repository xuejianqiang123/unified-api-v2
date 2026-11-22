package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"unified-api-v2/internal/traecn"
)

// Trae 面板代理：把统一面板的操作转给 Trae2api 的管理端点。
//
// 设计边界（用户 2026-11-22 选择「完整接管账号管理」）：
//   - 接管：账号列表、模型请求启停、删除账号、签到（全部 / 单账号）、积分与签到状态展示。
//   - 不接管：新增账号。Trae 的登录是浏览器 OAuth（/web/login → /authorize → /api/web-auth），
//     必须在 Trae2api 自己的页面完成，网关无法也不应代劳；面板给出跳转入口。
//   - 不透传任意路径：TraeClient.Admin 只放通白名单（trae.go 的 traeAdminAllow）。

// traeReady 统一的未接线判断。
func (h *Handler) traeReady(w http.ResponseWriter) bool {
	if h.cfg.TraeAdmin == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable",
			"trae provider 未启用：检查 providers.trae.enabled")
		return false
	}
	return true
}

// panelTraeAccounts GET /api/panel/trae/accounts
//
// 数据源有两个且字段互补（实测）：
//   - /api/checkin/accounts：有 checked_in、credits、account_credits，**没有** model_enabled
//   - /api/accounts：有 model_enabled，checked_in 为 null
//
// 面板的「启停」按钮依赖 model_enabled，所以这里合并成一份：以 checkin 为主，
// 用 /api/accounts 回填缺失的 model_enabled。任一接口失败都不影响另一方的字段。
func (h *Handler) panelTraeAccounts(w http.ResponseWriter, r *http.Request) {
	if !h.traeReady(w) {
		return
	}
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodGet, "/api/checkin/accounts", nil)
	if status == http.StatusOK {
		if extra, st2 := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodGet, "/api/accounts", nil); st2 == http.StatusOK {
			mergeTraeModelEnabled(out, extra)
		}
	}
	out["base_url"] = h.cfg.TraeAdmin.BaseURL()
	panelJSON(w, status, out)
}

// mergeTraeModelEnabled 把 /api/accounts 的 model_enabled 按账号 ID 回填到 checkin 结果。
func mergeTraeModelEnabled(dst, src map[string]any) {
	byID := map[string]map[string]any{}
	if list, ok := src["accounts"].([]any); ok {
		for _, it := range list {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := m["id"].(string); id != "" {
				byID[id] = m
			}
		}
	}
	if list, ok := dst["accounts"].([]any); ok {
		for _, it := range list {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			id, _ := m["id"].(string)
			s, ok := byID[id]
			if !ok {
				continue
			}
			if _, exists := m["model_enabled"]; !exists {
				if v, has := s["model_enabled"]; has {
					m["model_enabled"] = v
				}
			}
		}
	}
}

// panelTraeOverview GET /api/panel/trae/overview
// 代理 Trae2api 的 /api/overview：今日请求/tokens/积分与账号池概况。
func (h *Handler) panelTraeOverview(w http.ResponseWriter, r *http.Request) {
	if !h.traeReady(w) {
		return
	}
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodGet, "/api/overview", nil)
	panelJSON(w, status, out)
}

// decodeTraeBody 读取并校验请求体（限 64KB，必须是 JSON 对象）。
func decodeTraeBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_json", "参数错误: "+err.Error())
		return nil, false
	}
	return body, true
}

// accountIDOf 取出并校验账号 ID（只允许字母数字、下划线、短横线）。
func accountIDOf(body map[string]any) string {
	s, _ := body["account_id"].(string)
	if s == "" {
		s, _ = body["id"].(string)
	}
	s = strings.TrimSpace(s)
	if !traeSafeIDSegment(s) {
		return ""
	}
	return s
}

// panelTraeModelEnabled POST /api/panel/trae/accounts/model-enabled
// body {"account_id":"...","enabled":bool}：只影响是否接收新模型请求，不动签到。
func (h *Handler) panelTraeModelEnabled(w http.ResponseWriter, r *http.Request) {
	if !h.traeReady(w) {
		return
	}
	body, ok := decodeTraeBody(w, r)
	if !ok {
		return
	}
	id := accountIDOf(body)
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "account_id 非法或缺失")
		return
	}
	enabled, isBool := body["enabled"].(bool)
	if !isBool {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "enabled 必须是布尔值")
		return
	}
	payload, _ := json.Marshal(map[string]any{"account_id": id, "enabled": enabled})
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodPost, "/api/accounts/model-enabled", payload)
	panelJSON(w, status, out)
}

// panelTraeRemove POST /api/panel/trae/accounts/remove
// body {"account_id":"..."}：从 Trae2api 的账号存储中删除该账号。
func (h *Handler) panelTraeRemove(w http.ResponseWriter, r *http.Request) {
	if !h.traeReady(w) {
		return
	}
	body, ok := decodeTraeBody(w, r)
	if !ok {
		return
	}
	id := accountIDOf(body)
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "account_id 非法或缺失")
		return
	}
	payload, _ := json.Marshal(map[string]any{"account_id": id})
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodPost, "/api/accounts/remove", payload)
	panelJSON(w, status, out)
}

// panelTraeCheckinAll POST /api/panel/trae/checkin
// 触发 Trae2api 的 /api/checkin/claim-all（按存储顺序逐账号签到）。
func (h *Handler) panelTraeCheckinAll(w http.ResponseWriter, r *http.Request) {
	if !h.traeReady(w) {
		return
	}
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodPost, "/api/checkin/claim-all", nil)
	panelJSON(w, status, out)
}

// panelTraeCheckinOne POST /api/panel/trae/checkin/{account_id}
func (h *Handler) panelTraeCheckinOne(w http.ResponseWriter, r *http.Request, accountID string) {
	if !h.traeReady(w) {
		return
	}
	accountID = strings.TrimSuffix(accountID, "/")
	if !traeSafeIDSegment(accountID) {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "account_id 非法")
		return
	}
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodPost, "/api/checkin/account/"+accountID, nil)
	panelJSON(w, status, out)
}

// panelTraeReorder POST /api/panel/trae/accounts/reorder
// body {"order":["id1","id2"]}：调整 Trae 账号的轮询顺序。
//
// 为什么需要：Trae2api 按 accounts.json 里字典插入顺序取号，原版没有排序接口，
// 面板只能显示「无法管理」。这里转发给它新增的 /api/accounts/reorder（见
// trae2api/src/auth.py 末尾的 reorder_accounts），使 Trae 与另两家一样可拖拽排序。
func (h *Handler) panelTraeReorder(w http.ResponseWriter, r *http.Request) {
	if !h.traeReady(w) {
		return
	}
	body, ok := decodeTraeBody(w, r)
	if !ok {
		return
	}
	rawList, isList := body["order"].([]any)
	if !isList || len(rawList) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "order 必须是非空数组")
		return
	}
	order := make([]string, 0, len(rawList))
	for _, v := range rawList {
		s, _ := v.(string)
		if !traeSafeIDSegment(s) {
			writeOpenAIError(w, http.StatusBadRequest, "bad_request", "order 含非法账号 ID: "+s)
			return
		}
		order = append(order, s)
	}
	payload, _ := json.Marshal(map[string]any{"order": order})
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodPost, "/api/accounts/reorder", payload)
	panelJSON(w, status, out)
}

// panelTraeImportLocal POST /api/panel/trae/import-local
//
// 从本机已登录的 Trae 客户端读取凭证，直接写入 Trae2api 账号池。
//
// 为什么这样做：Trae 的官方登录是浏览器 OAuth，只能在 Trae2api 的页面完成，
// 统一面板因此一直只能「跳转外部控制台」——用户明确不接受。本机装了 Trae CN
// 且已登录时，凭证就在 storage.json 里（Go 侧已有解密实现），
// 解出来 POST /api/web-auth 即可入池，全程不出面板、不需要抓包。
func (h *Handler) panelTraeImportLocal(w http.ResponseWriter, r *http.Request) {
	if !h.traeReady(w) {
		return
	}
	cred, edition, err := traecn.AutoDiscover()
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "trae_local_auth_failed",
			"未读到本机 Trae 登录态（需先在本机 Trae 客户端登录）："+err.Error())
		return
	}
	payload := map[string]any{
		"token":            cred.Token,
		"refreshToken":     cred.RefreshToken,
		"userId":           cred.UserID,
		"expiredAt":        cred.ExpiredAt,
		"refreshExpiredAt": cred.RefreshExpiredAt,
		"host":             cred.Host,
		"label":            "本机 " + edition + " · " + cred.UserID,
	}
	raw, _ := json.Marshal(payload)
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodPost, "/api/web-auth", raw)
	out["edition"] = edition
	// add_account 按 user_id 整条覆盖写入，会把账号记录里的权益缓存
	// （account_credits / 更新时间）一并清掉，面板随即把积分读成 0。
	// 这里立刻触发一次权益刷新，避免「导入成功后积分归零」这种假故障。
	if status == http.StatusOK {
		if _, st2 := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodGet, "/api/checkin/credits/accounts", nil); st2 == http.StatusOK {
			out["credits_refresh"] = "ok"
		} else {
			out["credits_refresh"] = "failed"
		}
	}
	panelJSON(w, status, out)
}

// panelTraeAddAccount POST /api/panel/trae/accounts/add
// body {"token":"<Cloud-IDE-JWT>","label":"...","refresh_token":"...","user_id":"..."}
//
// 手动粘贴凭证的兜底路径：本机未装 Trae 或装了未登录时使用。
// 只接受 token 必填，其余字段可选（与 Trae2api /api/web-auth 的字段别名兼容）。
func (h *Handler) panelTraeAddAccount(w http.ResponseWriter, r *http.Request) {
	if !h.traeReady(w) {
		return
	}
	body, ok := decodeTraeBody(w, r)
	if !ok {
		return
	}
	token, _ := body["token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "token 不能为空")
		return
	}
	payload := map[string]any{"token": token}
	for _, k := range []string{"label", "refreshToken", "refresh_token", "userId", "user_id",
		"expiredAt", "expired_at", "refreshExpiredAt", "refresh_expired_at", "host"} {
		if v, has := body[k]; has {
			payload[k] = v
		}
	}
	raw, _ := json.Marshal(payload)
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodPost, "/api/web-auth", raw)
	if status == http.StatusOK {
		_, _ = h.cfg.TraeAdmin.Admin(r.Context(), http.MethodGet, "/api/checkin/credits/accounts", nil)
	}
	panelJSON(w, status, out)
}

// panelTraeCheckinStatus GET /api/panel/trae/checkin-status
// 代理 Trae2api 的 /api/checkin/status：返回每个账号最近一次签到结果
// （name/status/message/reward/days/ts），供概览的统一签到表合并展示。
func (h *Handler) panelTraeCheckinStatus(w http.ResponseWriter, r *http.Request) {
	if !h.traeReady(w) {
		return
	}
	out, status := h.cfg.TraeAdmin.Admin(r.Context(), http.MethodGet, "/api/checkin/status", nil)
	panelJSON(w, status, out)
}
