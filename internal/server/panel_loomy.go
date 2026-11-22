package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"unified-api-v2/internal/browsercookie"
)

// loomy 的账号凭证由网关自己管理（没有外部进程），存在 loomy/accounts.json。
//
// 凭证只有一种：Cookie `loomy_web_session`。Loomy 不签发 refresh token，
// 失效后只能重新走一次浏览器登录——所以这里不做过期刷新，只存原值。

const loomyCookieName = "loomy_web_session"

var loomyFileMu sync.Mutex

type loomyAccount struct {
	Name      string `json:"name"`
	Cookie    string `json:"cookie"` // 完整 "loomy_web_session=..."，永不对外下发
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`
}

type loomyAccountsFile struct {
	Accounts []loomyAccount `json:"accounts"`
}

func loomyDataDir() string      { return "loomy" }
func loomyAccountsPath() string { return filepath.Join(loomyDataDir(), "accounts.json") }

// loomyProfileDir 专用登录浏览器（Edge 独立 user-data-dir）的位置，
// 与 tools/loomy-login.ps1 里保持一致。
func loomyProfileDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "unified-api-v2", "loomy-profile")
}

func loadLoomyAccounts() loomyAccountsFile {
	var f loomyAccountsFile
	raw, err := os.ReadFile(loomyAccountsPath())
	if err != nil {
		return f
	}
	_ = json.Unmarshal(raw, &f)
	return f
}

func saveLoomyAccounts(f loomyAccountsFile) error {
	if err := os.MkdirAll(loomyDataDir(), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(loomyAccountsPath(), raw, 0o600)
}

// loomyPoints 是 points-summary 的额度快照。
type loomyPoints struct {
	Permanent int64  `json:"permanent"`
	Daily     int64  `json:"daily"`
	Nickname  string `json:"nickname"`
	Err       string `json:"err,omitempty"`
}

// fetchLoomyPoints 查一个账号的额度与昵称。
//
// 上游（读 loomy2api 的 credit.py 确认）：GET /web/api/auth/points-summary
// 返回 {permanent, daily}；GET /web/api/auth/profile 返回 {nickname}。
// 只认 loomy_web_session cookie，无 bearer、无 CSRF 头。
//
// 超时压到 3 秒：面板加载不能被额度查询拖住，慢就显示"—"。
func fetchLoomyPoints(cookie string) loomyPoints {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://loomy.xunfei.cn"+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Cookie", cookie)
		req.Header.Set("Referer", "https://loomy.xunfei.cn/web")
		req.Header.Set("Origin", "https://loomy.xunfei.cn")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("http %d", resp.StatusCode)
		}
		return json.Unmarshal(raw, out)
	}

	var pts struct {
		Data struct {
			Permanent int64 `json:"permanent"`
			Daily     int64 `json:"daily"`
		} `json:"data"`
	}
	res := loomyPoints{}
	if err := get("/web/api/auth/points-summary", &pts); err != nil {
		res.Err = err.Error()
		return res
	}
	res.Permanent = pts.Data.Permanent
	res.Daily = pts.Data.Daily

	// 昵称是装饰项：失败不影响额度结果。
	var prof struct {
		Data struct {
			Nickname string `json:"nickname"`
		} `json:"data"`
	}
	if err := get("/web/api/auth/profile", &prof); err == nil {
		res.Nickname = strings.TrimSpace(prof.Data.Nickname)
	}
	return res
}

// panelLoomyAccounts GET /api/panel/loomy/accounts
// 只下发账号名与元信息，cookie 明文永不出网关（与豆包/Trae 的口径一致）。
func (h *Handler) panelLoomyAccounts(w http.ResponseWriter, _ *http.Request) {
	f := loadLoomyAccounts()
	type row struct {
		Name      string `json:"name"`
		Note      string `json:"note"`
		CreatedAt int64  `json:"created_at"`
		CookieLen int    `json:"cookie_len"`
		Permanent int64  `json:"permanent"`
		Daily     int64  `json:"daily"`
		Nickname  string `json:"nickname,omitempty"`
		CreditErr string `json:"credit_err,omitempty"`
	}
	out := make([]row, 0, len(f.Accounts))
	for _, a := range f.Accounts {
		pts := fetchLoomyPoints(a.Cookie)
		out = append(out, row{
			Name: a.Name, Note: a.Note, CreatedAt: a.CreatedAt, CookieLen: len(a.Cookie),
			Permanent: pts.Permanent, Daily: pts.Daily, Nickname: pts.Nickname, CreditErr: pts.Err,
		})
	}
	panelJSON(w, http.StatusOK, map[string]any{
		"accounts":     out,
		"total":        len(out),
		"profile_dir":  loomyProfileDir(),
		"login_script": filepath.ToSlash(filepath.Join("tools", "loomy-login.ps1")),
	})
}

// panelLoomyImportLocal POST /api/panel/loomy/import-local
//
// 从专用 Edge profile 里读出 loomy_web_session 并落盘。
// 前提：用户已运行 tools/loomy-login.ps1 登录并**关闭该窗口**（Windows 文件锁）。
func (h *Handler) panelLoomyImportLocal(w http.ResponseWriter, r *http.Request) {
	dir := loomyProfileDir()
	if _, err := os.Stat(dir); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "loomy_profile_missing",
			"未找到登录 profile。请先运行 tools\\loomy-login.ps1 打开登录窗口完成登录")
		return
	}
	values, err := browsercookie.Read(dir, "Default", "xunfei.cn")
	if err != nil {
		// 最常见的是「窗口还开着」或「没登录成功」，原文已可操作，直接透出。
		writeOpenAIError(w, http.StatusBadRequest, "loomy_profile_unreadable", err.Error())
		return
	}
	sess := values[loomyCookieName]
	if sess == "" {
		writeOpenAIError(w, http.StatusBadRequest, "loomy_not_logged_in",
			"profile 里没有 "+loomyCookieName+"：请确认已在登录窗口完成登录（不是只打开了页面），然后关闭窗口再试")
		return
	}
	cookie := loomyCookieName + "=" + sess

	loomyFileMu.Lock()
	f := loadLoomyAccounts()
	replaced := false
	for i := range f.Accounts {
		if f.Accounts[i].Name == "local" {
			f.Accounts[i].Cookie = cookie
			f.Accounts[i].Note = "本机 Edge 导入"
			f.Accounts[i].CreatedAt = time.Now().Unix()
			replaced = true
			break
		}
	}
	if !replaced {
		f.Accounts = append(f.Accounts, loomyAccount{
			Name: "local", Cookie: cookie, Note: "本机 Edge 导入", CreatedAt: time.Now().Unix(),
		})
	}
	err = saveLoomyAccounts(f)
	loomyTotal := len(f.Accounts)
	loomyFileMu.Unlock()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "loomy_save_failed", "保存账号失败: "+err.Error())
		return
	}

	panelJSON(w, http.StatusOK, map[string]any{
		"ok": true, "name": "local", "added": !replaced,
		"cookie_len": len(cookie), "total": loomyTotal,
		"other_cookies": len(values) - 1,
	})
}

// panelLoomyRemove POST /api/panel/loomy/remove
// body {"name":"..."}：从 loomy/accounts.json 彻底删除该账号。
//
// 与豆包的删除语义一致：连记录一起删，不是"停用"。删完该账号的
// conversationId 记录仍留在 conversations.json（按 TTL 自然过期），
// 不影响其它账号。
func (h *Handler) panelLoomyRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_json", "参数错误")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "name 不能为空")
		return
	}

	loomyFileMu.Lock()
	f := loadLoomyAccounts()
	kept := make([]loomyAccount, 0, len(f.Accounts))
	found := false
	for _, a := range f.Accounts {
		if a.Name == name {
			found = true
			continue
		}
		kept = append(kept, a)
	}
	if !found {
		loomyFileMu.Unlock()
		writeOpenAIError(w, http.StatusNotFound, "not_found", "账号不存在: "+name)
		return
	}
	f.Accounts = kept
	err := saveLoomyAccounts(f)
	total := len(kept)
	loomyFileMu.Unlock()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "loomy_save_failed", "保存账号失败: "+err.Error())
		return
	}
	panelJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": name, "total": total})
}
