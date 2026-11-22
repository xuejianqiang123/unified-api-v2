package server

import "net/http"

// panelTraeLoginURL GET /api/panel/trae/login-url
//
// trae2api 自带登录页（/web/login），账号与 OAuth 都归它。
// 网关只提供一个入口，避免在前端硬编码地址。
func (h *Handler) panelTraeLoginURL(w http.ResponseWriter, r *http.Request) {
	if h.cfg.TraeAdmin == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable", "trae 未接线")
		return
	}
	panelJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"url":  h.cfg.TraeBaseURL + "/web/login",
		"hint": "在打开的页面完成登录；账号由 trae2api 自己保存，登录后回面板刷新即可",
	})
}
