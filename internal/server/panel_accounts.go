// panel_accounts.go 面板账号新增端点。
//
// 四类入口：
//   - WorkBuddy JSON 导入：校验凭证字段后写 auths/workbuddy-*.json，靠 pool 目录 watcher 自动入池。
//   - WorkBuddy OAuth：串行会话，起会话拿 authUrl → 浏览器登录 → poll 拿凭证落盘。
//   - 豆包 Cookie：写 accounts.json 落盘 + 进程内 admin/cookie 即时生效。
//   - 豆包扫码：转发 provider 原生 QR 端点（其可用性受 provider 配置限制）。
//
// 安全：所有凭证只在 POST body 内传输（面板受 Basic Auth 保护），不写日志、
// 不回显明文、前端不接触。落盘文件权限 0600。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// oauthSession 一次 WorkBuddy OAuth 会话（进程内，单会话串行）。
type oauthSession struct {
	Realm  string
	At     time.Time
	AuthURL string
}

var (
	oauthMu     sync.Mutex
	oauthSess   *oauthSession
	oauthTTL    = 5 * time.Minute
)

// wbCredential 兼容两类输入：
//   - login.exe poll 的扁平输出：access_token / refresh_token / expires_in / uid / nickname ...
//   - 现成 auth 文件的嵌套结构：auth:{accessToken,...} / account:{uid,...}
//   - 第三方常见格式：expires_at（毫秒或秒）
type wbCredential struct {
	// 扁平形（login 输出 / 第三方）
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAtMS  int64  `json:"expires_at"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	// 嵌套形（现成 auth 文件）
	Auth struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
		Realm        string `json:"realm"`
	} `json:"auth"`
	Account struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
	} `json:"account"`
}

func (c *wbCredential) normalize() error {
	if c.Auth.AccessToken != "" {
		c.AccessToken = c.Auth.AccessToken
		c.RefreshToken = c.Auth.RefreshToken
		if c.Auth.ExpiresAt > 0 {
			c.ExpiresAtMS = c.Auth.ExpiresAt
		}
		if c.Domain == "" {
			c.Domain = c.Auth.Domain
		}
		if c.Realm == "" {
			c.Realm = c.Auth.Realm
		}
	}
	if c.Account.UID != "" && c.UID == "" {
		c.UID = c.Account.UID
	}
	if c.Account.Nickname != "" && c.Nickname == "" {
		c.Nickname = c.Account.Nickname
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return fmt.Errorf("缺少 access_token（或 auth.accessToken）")
	}
	return nil
}

// expiresAtSeconds 归一到 Unix 秒。expires_at 若 >1e12 视为毫秒。
func (c *wbCredential) expiresAtSeconds() int64 {
	v := c.ExpiresAtMS
	if v <= 0 && c.ExpiresIn > 0 {
		return time.Now().Unix() + c.ExpiresIn
	}
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// saveWBCredential 把凭证写成 auths/workbuddy-<uid>.json（嵌套格式，与 SaveAtomic 一致）。
func (h *Handler) saveWBCredential(c *wbCredential) (string, error) {
	if err := c.normalize(); err != nil {
		return "", err
	}
	dir := h.cfg.AuthDir
	if dir == "" {
		dir = "./auths"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	uid := strings.TrimSpace(c.UID)
	if uid == "" {
		// 无 uid 时用 accessToken 前 12 字节派生稳定 id，避免覆盖别人。
		n := len(c.AccessToken)
		if n > 12 {
			n = 12
		}
		uid = "import-" + c.AccessToken[:n]
	}
	realm := c.Realm
	if realm == "" {
		realm = "cn"
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  c.AccessToken,
			"refreshToken": c.RefreshToken,
			"expiresAt":    c.expiresAtSeconds(),
			"domain":       c.Domain,
			"realm":        realm,
		},
		"account": map[string]any{
			"uid":      uid,
			"nickname": c.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	name := "workbuddy-" + uid + ".json"
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return name, nil
}

// panelWBImport POST /api/panel/accounts/wb/import
// body 可为单条凭证或凭证数组，逐个校验落盘；返回成功写入的文件名列表。
func (h *Handler) panelWBImport(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "读取请求失败: "+err.Error())
		return
	}
	trimmed := bytes.TrimSpace(raw)
	var creds []wbCredential
	if len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &creds); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "bad_json", "JSON 数组解析失败: "+err.Error())
			return
		}
	} else {
		var one wbCredential
		if err := json.Unmarshal(trimmed, &one); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "bad_json", "JSON 解析失败: "+err.Error())
			return
		}
		creds = []wbCredential{one}
	}
	if len(creds) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "凭证列表为空")
		return
	}
	added, failed := []string{}, []string{}
	for i := range creds {
		name, err := h.saveWBCredential(&creds[i])
		if err != nil {
			failed = append(failed, fmt.Sprintf("#%d: %s", i+1, err.Error()))
			continue
		}
		added = append(added, name)
	}
	if len(added) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "import_failed", "全部导入失败: "+strings.Join(failed, "; "))
		return
	}
	panelJSON(w, http.StatusOK, map[string]any{
		"added": added, "failed": failed,
		"note": "凭证已写入 auths/，网关每 5 秒自动加载；若无生效请查看网关日志。",
	})
}

// panelWBOAuthURL POST /api/panel/accounts/wb/oauth/url
// body: {"realm":"cn|global"}。起串行 OAuth 会话，返回 authUrl 供浏览器打开。
func (h *Handler) panelWBOAuthURL(w http.ResponseWriter, r *http.Request) {
	if h.cfg.LoginBin == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "oauth_unavailable", "未配置 login.exe 路径")
		return
	}
	if _, err := os.Stat(h.cfg.LoginBin); err != nil {
		// 相对路径：在 cwd 下找
		if _, err2 := os.Stat(filepath.Base(h.cfg.LoginBin)); err2 != nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "oauth_unavailable", "login.exe 不存在: "+h.cfg.LoginBin)
			return
		}
	}
	var body struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	realm := body.Realm
	if realm != "cn" && realm != "global" {
		realm = "cn"
	}
	cmd := exec.Command(h.cfg.LoginBin, "--realm="+realm, "url")
	out, err := cmd.Output()
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "oauth_start_failed", "发起登录失败: "+err.Error())
		return
	}
	authURL := strings.TrimSpace(string(out))
	if !strings.HasPrefix(authURL, "http") {
		writeOpenAIError(w, http.StatusBadGateway, "oauth_start_failed", "登录工具未返回有效 URL")
		return
	}
	oauthMu.Lock()
	oauthSess = &oauthSession{Realm: realm, At: time.Now(), AuthURL: authURL}
	oauthMu.Unlock()
	panelJSON(w, http.StatusOK, map[string]any{"auth_url": authURL, "realm": realm})
}

// panelWBOAuthPoll POST /api/panel/accounts/wb/oauth/poll
// 用户在浏览器完成登录后调用；执行 login poll，成功则落盘凭证。
func (h *Handler) panelWBOAuthPoll(w http.ResponseWriter, r *http.Request) {
	if h.cfg.LoginBin == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "oauth_unavailable", "未配置 login.exe 路径")
		return
	}
	oauthMu.Lock()
	sess := oauthSess
	oauthMu.Unlock()
	if sess == nil || time.Since(sess.At) > oauthTTL {
		writeOpenAIError(w, http.StatusBadRequest, "oauth_no_session", "无有效 OAuth 会话，请先发起登录")
		return
	}
	cmd := exec.Command(h.cfg.LoginBin, "--realm="+sess.Realm, "poll")
	out, err := cmd.Output()
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "oauth_pending", "登录尚未完成或已失败，请在浏览器完成授权后重试")
		return
	}
	var cred wbCredential
	if err := json.Unmarshal(bytes.TrimSpace(out), &cred); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "oauth_parse", "解析登录结果失败: "+err.Error())
		return
	}
	cred.Realm = sess.Realm
	name, err := h.saveWBCredential(&cred)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "oauth_save", "保存凭证失败: "+err.Error())
		return
	}
	oauthMu.Lock()
	oauthSess = nil
	oauthMu.Unlock()
	panelJSON(w, http.StatusOK, map[string]any{"added": name, "uid": cred.UID})
}

// dbAccountFile 豆包 accounts.json 结构（与 dbserver 一致）。
type dbAccountFile struct {
	Accounts []struct {
		Name       string `json:"name"`
		Cookie     string `json:"cookie"`
		Note       string `json:"note,omitempty"`
		LocalAppID string `json:"local_app_id,omitempty"`
	} `json:"accounts"`
}

// panelDBCookie POST /api/panel/accounts/db/cookie
// body: {"name":"账号名","cookie":"...","note":"..."}。落盘 accounts.json + 进程内即时生效。
func (h *Handler) panelDBCookie(w http.ResponseWriter, r *http.Request) {
	if h.cfg.DoubaoAdmin == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable", "doubao provider 未启用")
		return
	}
	var body struct {
		Name   string `json:"name"`
		Cookie string `json:"cookie"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_json", "参数错误: "+err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || strings.TrimSpace(body.Cookie) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "name 与 cookie 不能为空")
		return
	}
	// 1) 进程内即时生效（复用 provider 原生端点，跳过统一鉴权由面板 Basic Auth 保护）。
	payload, _ := json.Marshal(map[string]string{"name": body.Name, "cookie": body.Cookie})
	out, status := h.doubaoAdmin(http.MethodPost, "/admin/cookie", payload)
	if status < 200 || status >= 300 {
		writeOpenAIError(w, http.StatusBadGateway, "cookie_apply_failed", fmt.Sprintf("provider 拒绝 Cookie: %v", out))
		return
	}
	// 2) 落盘 accounts.json（重启不丢号）。
	if h.cfg.DoubaoAccountsFile != "" {
		if err := h.upsertDBCookieFile(body.Name, body.Cookie, body.Note); err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "cookie_persist_failed", "Cookie 已生效但写入 accounts.json 失败: "+err.Error())
			return
		}
	}
	panelJSON(w, http.StatusOK, map[string]any{"ok": true, "name": body.Name, "persisted": h.cfg.DoubaoAccountsFile != ""})
}

func (h *Handler) upsertDBCookieFile(name, cookie, note string) error {
	path := h.cfg.DoubaoAccountsFile
	var f dbAccountFile
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	found := false
	for i := range f.Accounts {
		if f.Accounts[i].Name == name {
			f.Accounts[i].Cookie = cookie
			if note != "" {
				f.Accounts[i].Note = note
			}
			found = true
			break
		}
	}
	if !found {
		f.Accounts = append(f.Accounts, struct {
			Name       string `json:"name"`
			Cookie     string `json:"cookie"`
			Note       string `json:"note,omitempty"`
			LocalAppID string `json:"local_app_id,omitempty"`
		}{Name: name, Cookie: cookie, Note: note})
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// panelDBQR POST/GET /api/panel/accounts/db/qr/*
// 转发 provider 原生扫码端点。注意：provider 未配置 qr 时返回 501/错误，
// 且此前实测确认阶段 expired（provider 已知限制），失败时如实透出。
func (h *Handler) panelDBQR(w http.ResponseWriter, r *http.Request) {
	if h.cfg.DoubaoAdmin == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable", "doubao provider 未启用")
		return
	}
	sub := strings.TrimPrefix(r.URL.Path, "/api/panel/accounts/db/qr")
	target := "/admin/accounts/qr" + sub
	var payload []byte
	if r.Body != nil {
		payload, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
	}
	out, status := h.doubaoAdmin(r.Method, target, payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(mustJSON(out))
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}

// panelAccountOrder 组内账号拖拽排序。provider=workbuddy|doubao。
// 请求体统一 {"provider":"...","names":[...]}（names 为账号 uid 或 name 的完整列表）。
// WorkBuddy 直接调本进程 adminAccountOrder；豆包转发其 provider 原生排序端点。
func (h *Handler) panelAccountOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string   `json:"provider"`
		Names    []string `json:"names"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_json", "参数错误: "+err.Error())
		return
	}
	if len(body.Names) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "names 不能为空")
		return
	}
	switch body.Provider {
	case "workbuddy":
		raw, _ := json.Marshal(map[string]any{"uids": body.Names})
		r2 := r.Clone(r.Context())
		r2.Body = io.NopCloser(bytes.NewReader(raw))
		h.adminAccountOrder(w, r2)
	case "doubao":
		if h.cfg.DoubaoAdmin == nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "provider_unavailable", "豆包 provider 未启用")
			return
		}
		raw, _ := json.Marshal(map[string]any{"names": body.Names})
		out, status := h.doubaoAdmin(http.MethodPost, "/admin/accounts/order", raw)
		panelJSON(w, status, out)
	default:
		writeOpenAIError(w, http.StatusBadRequest, "bad_provider", "未知 provider: "+body.Provider)
	}
}
