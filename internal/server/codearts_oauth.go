// codearts_oauth.go CodeArts 的 Web 登录（授权码 + PKCE + DPoP）。
//
// 为什么要它：此前拿凭据的唯一途径是「从本机 IDE 的 secretStorage 里读」，
// 于是网关被绑死在 IDE 上 —— IDE 不登录，网关就只能是「只读模式」。
// 现在网关自己走一遍授权码流程，账号完全由网关持有，与 IDE 无关。
//
// 协议来源（IDE 真实登录链接）：
//
//	https://codearts.huaweicloud.com/portal/authorize?client_id=codearts&port=...
//	  &code_challenge=...&code_challenge_method=SHA-256&uri_scheme=codearts
//	  → 登录后回调 http://127.0.0.1:<port>/oauth/callback?code=...
package server

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"unified-api-v2/internal/codearts"
)

// pendingLogin 是一次进行中的授权（华为的回调只带 code、不带 state，
// 所以同时只保留最近一次，5 分钟过期 —— 单用户面板场景足够）。
type pendingLogin struct {
	mu        sync.Mutex
	verifier  string
	jwk       *codearts.DpopJWK
	port      int
	createdAt time.Time
}

var codeArtsPending pendingLogin

// panelCodeArtsLogin GET /api/panel/codearts/login
//
// 返回授权页 URL，前端 window.open 打开；用户在华为登录后浏览器会回调
// /oauth/callback，那边完成换码与落盘。
func (h *Handler) panelCodeArtsLogin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CodeArtsLoginPort == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable",
			"未配置 codearts 登录回调端口")
		return
	}
	pkce, err := codearts.NewPkcePair()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	jwk, err := codearts.GenerateDpopKey()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}

	codeArtsPending.mu.Lock()
	codeArtsPending.verifier = pkce.Verifier
	codeArtsPending.jwk = jwk
	codeArtsPending.port = h.cfg.CodeArtsLoginPort
	codeArtsPending.createdAt = time.Now()
	codeArtsPending.mu.Unlock()

	panelJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"url":          codearts.AuthorizeURL(pkce.Challenge, h.cfg.CodeArtsLoginPort),
		"redirect_uri": codearts.RedirectURI(h.cfg.CodeArtsLoginPort),
		"expires_in":   300,
	})
}

// panelCodeArtsCallback GET /oauth/callback?code=...
//
// 浏览器跳转过来的落点，**不能挂鉴权**（浏览器不会带 Authorization 头）。
// 只认最近 5 分钟内发起的那次授权，避免被随手请求触发换码。
func (h *Handler) panelCodeArtsCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		writeOpenAIHTML(w, http.StatusBadRequest, "登录失败", "回调里没有 code："+r.URL.RawQuery)
		return
	}
	codeArtsPending.mu.Lock()
	verifier, jwk, port, at := codeArtsPending.verifier, codeArtsPending.jwk, codeArtsPending.port, codeArtsPending.createdAt
	codeArtsPending.mu.Unlock()

	if verifier == "" || jwk == nil || time.Since(at) > 5*time.Minute {
		writeOpenAIHTML(w, http.StatusBadRequest, "登录已过期",
			"这次授权超过 5 分钟或没有发起过。请回到面板重新点「Web 登录」。")
		return
	}
	if h.cfg.CodeArtsExchange == nil {
		writeOpenAIHTML(w, http.StatusServiceUnavailable, "未接线", "CodeArts 未启用")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	name, expires, err := h.cfg.CodeArtsExchange(ctx, code, verifier, port, jwk)
	if err != nil {
		log.Printf("codearts Web 登录失败: %v", err)
		writeOpenAIHTML(w, http.StatusBadGateway, "登录失败", err.Error())
		return
	}
	// 用掉即失效：一次授权只能换一次码。
	codeArtsPending.mu.Lock()
	codeArtsPending.verifier = ""
	codeArtsPending.jwk = nil
	codeArtsPending.mu.Unlock()

	log.Printf("codearts Web 登录成功：account=%s expires=%s", name, expires)
	writeOpenAIHTML(w, http.StatusOK, "登录成功",
		"账号 "+name+" 已导入，STS 到期 "+expires+"。可以关闭这个页面，回到面板。")
}

// writeOpenAIHTML 极简结果页（登录回调不走 JSON，用户要看的是浏览器页面）。
func writeOpenAIHTML(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>` + title +
		`</title><div style="font:15px/1.7 system-ui;max-width:560px;margin:80px auto;padding:0 20px">` +
		`<h2 style="font-size:20px;margin:0 0 12px">` + title + `</h2><p style="color:#555">` + msg + `</p></div>`))
}
