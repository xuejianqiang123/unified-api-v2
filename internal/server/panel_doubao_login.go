package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"unified-api-v2/internal/browsercookie"
)

// panelDoubaoLogin POST /api/panel/doubao/login
//
// 为什么需要浏览器：豆包签到要求 Cookie 里带 msToken，而 msToken 是页面 JS
// 生成的反爬 token（绑定设备指纹与会话），HTTP 客户端既拿不到也续不了。
// 所以这里照 loomy 的路子——打开独立 Edge 登录，再读取该 profile 的 Cookie。
func (h *Handler) panelDoubaoLogin(w http.ResponseWriter, r *http.Request) {
	script := filepath.Join("tools", "doubao-login.ps1")
	if _, err := os.Stat(script); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "doubao_login_missing",
			"未找到 tools\\doubao-login.ps1")
		return
	}
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script)
	if err := cmd.Start(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "doubao_login_failed",
			"打开登录窗口失败: "+err.Error())
		return
	}

	// 后台轮询：要求同时具备 sessionid 与 msToken 才算有效（只有 sessionid 签不了到）。
	go func() {
		dir := doubaoProfileDir()
		for i := 0; i < 120; i++ {
			time.Sleep(3 * time.Second)
			values, err := browsercookie.Read(dir, "Default", "doubao.com")
			if err != nil {
				continue // 窗口还开着（Cookies 库被锁）或尚未登录
			}
			if values["sessionid"] == "" || values["msToken"] == "" {
				continue
			}
			cookie := joinCookie(values)
			login := values["uid_tt"]
			if err := h.upsertDoubaoAccount(cookie, values["web_id"], values["device_id"]); err != nil {
				log.Printf("豆包 Web 登录：写账号失败 %v", err)
				return
			}
			// 让 dbserver 热加载新账号（只增不减，不会误删在途账号）。
			if _, st := h.doubaoAdmin(http.MethodPost, "/admin/reload", nil); st >= 300 {
				log.Printf("豆包 Web 登录：reload 返回 %d（重启 dbserver 后生效）", st)
			}
			log.Printf("豆包 Web 登录：账号已更新（uid=%s，cookie %d 字节，含 msToken）", login, len(cookie))
			return
		}
		log.Printf("豆包 Web 登录：6 分钟内没读到含 msToken 的 Cookie，放弃")
	}()

	panelJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"hint": "登录窗口已打开：完成登录并确认右上角是目标账号后关闭窗口，账号会自动更新",
	})
}

// doubaoProfileDir 与 tools/doubao-login.ps1 里的路径保持一致。
func doubaoProfileDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "AppData", "Local")
	}
	return filepath.Join(base, "unified-api-v2", "doubao-profile")
}

// joinCookie 把 cookie map 拼回 `k=v; k=v` 形式。
func joinCookie(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for k, v := range values {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

// upsertDoubaoAccount 按 uid_tt（登录用户标识）匹配已有账号：
//
//	命中 → 换成新 Cookie（含新的 msToken）
//	未命中 → 追加一个 web-<短 uid> 账号
//
// 直接写 doubao2api/accounts.json：网关与 dbserver 共用这个文件，
// 写完再调 /admin/reload 让它重新装载。
func (h *Handler) upsertDoubaoAccount(cookie, webID, deviceID string) error {
	path := filepath.Join("doubao2api", "accounts.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return err
	}

	// uid_tt 在 cookie 里，用它判断「是不是同一个号」比名字可靠。
	uid := ""
	for _, part := range strings.Split(cookie, "; ") {
		if strings.HasPrefix(part, "uid_tt=") {
			uid = strings.TrimPrefix(part, "uid_tt=")
			break
		}
	}

	matched := false
	for _, acc := range doc.Accounts {
		old, _ := acc["cookie"].(string)
		if uid != "" && strings.Contains(old, "uid_tt="+uid) {
			acc["cookie"] = cookie
			if webID != "" {
				acc["web_id"] = webID
			}
			if deviceID != "" {
				acc["device_id"] = deviceID
			}
			acc["note"] = "Web 登录导入"
			matched = true
			break
		}
	}
	if !matched {
		name := "web"
		if uid != "" {
			if len(uid) > 8 {
				uid = uid[:8]
			}
			name = "web-" + uid
		}
		doc.Accounts = append(doc.Accounts, map[string]any{
			"name": name, "cookie": cookie,
			"web_id": webID, "device_id": deviceID,
			"note": "Web 登录导入",
		})
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}
