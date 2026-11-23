package server

import (
	"log"
	"net/http"

	"unified-api-v2/internal/browsercookie"
)

// panelDoubaoImportProfile POST /api/panel/doubao/import-profile
//
// 只读「已登录过的独立 profile」，不打开浏览器。
//
// 存在的意义：Web 登录那次是「开窗口 + 轮询」的组合，如果用户手动关晚了、
// 或轮询超时，就得从头再来一遍。这个端点让「窗口已关」时能补一次读取，
// 不必重复登录。
func (h *Handler) panelDoubaoImportProfile(w http.ResponseWriter, r *http.Request) {
	values, err := browsercookie.Read(doubaoProfileDir(), "Default", "doubao.com")
	if err != nil {
		// 最常见：Edge 窗口还开着（Cookies 库被锁）或从未登录。
		writeOpenAIError(w, http.StatusBadRequest, "doubao_profile_unreadable", err.Error())
		return
	}
	if values["sessionid"] == "" || values["msToken"] == "" {
		writeOpenAIError(w, http.StatusBadRequest, "doubao_profile_incomplete",
			"profile 里缺 sessionid 或 msToken：请确认已登录（不是只打开了页面），然后关闭窗口再试")
		return
	}
	cookie := joinCookie(values)
	if err := h.upsertDoubaoAccount(cookie, values["web_id"], values["device_id"]); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "doubao_save_failed", err.Error())
		return
	}
	if _, st := h.doubaoAdmin(http.MethodPost, "/admin/reload", nil); st >= 300 {
		log.Printf("豆包 profile 导入：reload 返回 %d", st)
	}
	log.Printf("豆包 profile 导入成功：cookie %d 字节，含 msToken", len(cookie))
	panelJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"cookie_len":    len(cookie),
		"has_mstoken":   true,
		"other_cookies": len(values),
	})
}
