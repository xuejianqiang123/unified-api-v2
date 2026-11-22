package server

import (
	"context"
	"log"
	"net/http"
	"time"

	codeartspkg "unified-api-v2/internal/codearts"
)

// CodeArtsPanelInfo 是面板要展示的 CodeArts 账号快照。
type CodeArtsPanelInfo struct {
	Name      string         // 账号显示名
	ExpiresAt string         // STS 到期（RFC3339，空=未知）
	Quota     map[string]any // statistics/plugin 原样响应
	Err       error          // 查询失败原因（非致命，前端显示「—」）
}

// panelCodeArtsAccounts GET /api/panel/codearts/accounts
//
// 账号与额度同源：CodeArts 只有一个侧（网关自己拿凭据换的 STS），没有外部进程
// 轮询，所以额度必须在这里现查。
//
// 失败**不**让整页报错：返回 err 字段由前端显示「—」，而不是 500 ——
// 一个上游抖动不该让账号表格整块消失。
func (h *Handler) panelCodeArtsAccounts(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Name      string `json:"name"`
		Credits   int64  `json:"credits"`
		Basic     int64  `json:"basic"`
		Bonus     int64  `json:"bonus"`
		OnDemand  int64  `json:"on_demand"`
		Expiring  int64  `json:"expiring"`
		ExpKnown  bool   `json:"expiring_known"`
		Package   string `json:"package,omitempty"`
		ExpiresAt string `json:"expires_at,omitempty"`
		Err       string `json:"err,omitempty"`
	}

	if h.cfg.CodeArtsPanel == nil {
		panelJSON(w, http.StatusOK, map[string]any{
			"accounts": []row{}, "enabled": false, "err": "CodeArts 未接线",
		})
		return
	}

	// 上游偶发慢时不要拖住整页；与 loomy 的 3s 同口径。
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()

	info := h.cfg.CodeArtsPanel(ctx)
	out := row{Name: info.Name, ExpiresAt: info.ExpiresAt}
	if out.Name == "" {
		out.Name = "codearts"
	}
	if info.Err != nil {
		out.Err = info.Err.Error()
		panelJSON(w, http.StatusOK, map[string]any{"accounts": []row{out}, "enabled": true})
		return
	}
	sum := codeartspkg.Summarize(info.Quota)
	out.Credits = sum.Total
	out.Basic = sum.Basic
	out.Bonus = sum.Bonus
	out.OnDemand = sum.OnDemand
	out.Expiring = sum.Expiring
	out.ExpKnown = sum.ExpiringKnown
	out.Package = sum.PackageName
	panelJSON(w, http.StatusOK, map[string]any{"accounts": []row{out}, "enabled": true})
}

// panelCodeArtsImport POST /api/panel/codearts/import
//
// codearts 的凭据不是「用户名 + 密码」，而是本机 IDE 里的一套 STS 材料
// （AK/SK/security_token + refresh_token + DPoP 私钥）。所以「添加账号」
// 的正确形态是「从 IDE 重新读一次」，而不是弹一个表单让人填。
//
// 导入成功后上游客户端下一次请求就会读到新文件（Client 每次重读），
// 不需要重启网关。
func (h *Handler) panelCodeArtsImport(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CodeArtsImport == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable",
			"CodeArts 未接线：检查 providers.codearts.enabled")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	name, expiresAt, err := h.cfg.CodeArtsImport(ctx)
	if err != nil {
		panelJSON(w, http.StatusOK, map[string]any{
			"ok": false, "err": err.Error(),
			"hint": "确认码道 IDE 已登录（导入读的是它的凭据库）；接管模式下也可用 codearts-import 命令",
		})
		return
	}
	log.Printf("面板导入 codearts 凭据：account=%s expires=%s", name, expiresAt)
	panelJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": name, "expires_at": expiresAt,
	})
}
