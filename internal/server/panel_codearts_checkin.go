package server

import "net/http"

// panelCodeArtsCheckin POST /api/panel/codearts/checkin
//
// 与 workbuddy 的 /v1/checkin 同源（都走 scheduler 的 ExtraCheckins），
// 这里只是给面板一个「立即签到」入口 —— 后端能力早就有，缺的是按钮。
func (h *Handler) panelCodeArtsCheckin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CodeArtsCheckin == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable",
			"CodeArts 未接线：检查 providers.codearts.enabled")
		return
	}
	kind, credit, msg, err := h.cfg.CodeArtsCheckin(r.Context())
	panelJSON(w, http.StatusOK, map[string]any{
		"ok":      err == nil && kind != "failed",
		"kind":    kind,
		"credit":  credit,
		"message": msg,
		"err":     errText(err),
	})
}
