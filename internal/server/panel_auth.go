// panel_auth.go 统一面板的自定义登录（替代浏览器 Basic Auth 原生弹窗）。
//
// 设计：
//   - 无状态签名 cookie（HMAC-SHA256，密钥派生自 PanelAuth），不引入 session 存储。
//   - 不再发送 WWW-Authenticate 头——那是浏览器弹出原生用户名/密码框的根源。
//   - 保留 Basic Auth 兼容：脚本/curl 仍可用 -u user:pass 调 /api/panel/*。
//   - 未登录访问页面 → 302 到 /login；访问 API → 401 JSON（前端据此跳登录页）。
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const panelCookieName = "wb2api_panel"

// panelSessionToken 生成无状态会话令牌：HMAC(secret, "panel-session-v1")。
// PanelAuth 变化（改密码）会使旧 cookie 自动失效，无需额外吊销机制。
func (h *Handler) panelSessionToken() string {
	mac := hmac.New(sha256.New, []byte(h.cfg.PanelAuth))
	mac.Write([]byte("panel-session-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *Handler) panelCookieValid(r *http.Request) bool {
	if h.cfg.PanelAuth == "" {
		return true
	}
	c, err := r.Cookie(panelCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	want := h.panelSessionToken()
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
}

// withPanelAuth 面板鉴权闸（替代 Basic Auth 弹窗）。
func (h *Handler) withPanelAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.PanelAuth == "" {
			next(w, r)
			return
		}
		if h.panelCookieValid(r) {
			next(w, r)
			return
		}
		// Basic Auth 兼容：脚本/curl 的 -u user:pass 仍然有效（不触发浏览器弹窗，
		// 因为不发 WWW-Authenticate 头；只有客户端主动带 Authorization 时才生效）。
		if u, p, ok := r.BasicAuth(); ok {
			want := h.cfg.PanelAuth
			if subtle.ConstantTimeCompare([]byte(u+":"+p), []byte(want)) == 1 {
				next(w, r)
				return
			}
		}
		// 未登录：API 返回 401 JSON，页面重定向到登录页。均不发 WWW-Authenticate。
		if strings.HasPrefix(r.URL.Path, "/api/panel/") {
			writeOpenAIError(w, http.StatusUnauthorized, "panel_unauthorized", "需要面板登录")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// panelLoginPage 登录页（不鉴权）。
func (h *Handler) panelLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/login" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(panelLoginHTML))
}

// panelLogin 校验密码并设置会话 cookie。
func (h *Handler) panelLogin(w http.ResponseWriter, r *http.Request) {
	if h.cfg.PanelAuth == "" {
		panelJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSONLimited(r, &body, 1<<20); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_json", "参数错误: "+err.Error())
		return
	}
	want := h.cfg.PanelAuth
	// 面板密码格式 "user:pass"：允许只输密码（取冒号后段）或完整 "user:pass"。
	passOK := subtle.ConstantTimeCompare([]byte(body.Password), []byte(want)) == 1
	if !passOK {
		if i := strings.Index(want, ":"); i >= 0 {
			passOK = subtle.ConstantTimeCompare([]byte(body.Password), []byte(want[i+1:])) == 1
		}
	}
	if !passOK {
		writeOpenAIError(w, http.StatusUnauthorized, "bad_password", "密码错误")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     panelCookieName,
		Value:    h.panelSessionToken(),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(7 * 24 * time.Hour),
	})
	panelJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// panelLogout 清除会话 cookie。
func (h *Handler) panelLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     panelCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	panelJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// decodeJSONLimited 限制 body 大小的 JSON 解码（避免复用时漏掉限制）。
func decodeJSONLimited(r *http.Request, v any, limit int64) error {
	return json.NewDecoder(io.LimitReader(r.Body, limit)).Decode(v)
}

// panelLoginHTML 自定义登录页，风格与主面板一致（深色卡片 + 绿色主色）。
const panelLoginHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>统一控制台 · 登录</title>
<style>
:root{--bg:#0b0e13;--card:#141a23;--line:#1f2833;--fg:#e6edf3;--dim:#8b949e;--accent:#3fb950;--bad:#f85149}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;background:var(--bg);color:var(--fg);display:flex;align-items:center;justify-content:center;
 font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC",sans-serif;padding:20px}
.card{background:var(--card);border:1px solid var(--line);border-radius:14px;width:100%;max-width:380px;padding:28px;
 box-shadow:0 20px 60px rgba(0,0,0,.5)}
.brand{display:flex;align-items:center;gap:10px;margin-bottom:6px}
.brand .dot{width:10px;height:10px;border-radius:50%;background:var(--accent);box-shadow:0 0 12px var(--accent)}
.brand h1{font-size:17px;margin:0;letter-spacing:.4px}
.sub{color:var(--dim);font-size:12px;margin:0 0 22px}
label{display:block;font-size:12px;color:var(--dim);margin-bottom:6px}
input{width:100%;background:#0d131a;border:1px solid var(--line);color:var(--fg);border-radius:8px;
 padding:10px 12px;font:inherit;margin-bottom:16px}
input:focus{outline:none;border-color:var(--accent)}
button{width:100%;background:var(--accent);color:#04140a;border:0;border-radius:8px;padding:10px 14px;
 font-weight:600;font-size:14px;cursor:pointer}
button:disabled{opacity:.6;cursor:not-allowed}
.err{color:var(--bad);font-size:12px;min-height:18px;margin-bottom:8px}
.foot{margin-top:18px;text-align:center;color:var(--dim);font-size:11px}
</style></head><body>
<div class="card">
 <div class="brand"><span class="dot"></span><h1>统一控制台</h1></div>
 <p class="sub">Unified API · 输入面板密码登录</p>
 <form id="f">
  <label for="p">密码</label>
  <input id="p" name="p" type="password" autocomplete="current-password" placeholder="面板登录密码" autofocus>
  <div class="err" id="err"></div>
  <button type="submit" id="btn">登录</button>
 </form>
 <div class="foot">仅本机可访问 · 会话有效期 7 天</div>
</div>
<script>
document.getElementById('f').addEventListener('submit',async function(e){
 e.preventDefault();
 const btn=document.getElementById('btn'),err=document.getElementById('err');
 const password=document.getElementById('p').value;
 if(!password){err.textContent='请输入密码';return;}
 btn.disabled=true;err.textContent='';
 try{
  const r=await fetch('/api/panel/login',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({password:password})});
  const d=await r.json();
  if(r.ok&&d.ok){location.href='/';}else{err.textContent=(d.error&&d.error.message)||'登录失败';}
 }catch(ex){err.textContent='网络错误：'+ex.message;}
 finally{btn.disabled=false;}
});
</script></body></html>`
