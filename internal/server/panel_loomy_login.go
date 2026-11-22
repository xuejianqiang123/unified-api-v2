package server

import (
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"unified-api-v2/internal/browsercookie"
)

// panelLoomyLogin POST /api/panel/loomy/login
//
// 把「跑脚本 → 登录 → 关窗口 → 再点导入」压成一步：打开独立 Edge 登录窗口，
// 同时后台轮询该 profile 的 cookie，读到 loomy_web_session 就自动落盘。
//
// 为什么不用「抓请求」那套：Loomy 的会话就是一个 HttpOnly Cookie，抓包反而
// 可能漏掉它；直接读 profile 的 Cookies 库是官方登录的自然结果，也更稳。
func (h *Handler) panelLoomyLogin(w http.ResponseWriter, r *http.Request) {
	script := filepath.Join("tools", "loomy-login.ps1")
	if _, err := os.Stat(script); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "loomy_login_missing",
			"未找到 tools\\loomy-login.ps1")
		return
	}
	// 非阻塞启动：脚本会弹一个独立 Edge 窗口，用户在里面登录。
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script)
	if err := cmd.Start(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "loomy_login_failed",
			"打开登录窗口失败: "+err.Error())
		return
	}

	// 后台轮询：窗口关闭后 cookie 库才可读，所以最多等 5 分钟。
	go func() {
		dir := loomyProfileDir()
		for i := 0; i < 100; i++ {
			time.Sleep(3 * time.Second)
			values, err := browsercookie.Read(dir, "Default", "xunfei.cn")
			if err != nil {
				continue // 窗口还开着（库被锁）或还没登录
			}
			sess := values[loomyCookieName]
			if sess == "" {
				continue
			}
			cookie := loomyCookieName + "=" + sess
			loomyFileMu.Lock()
			f := loadLoomyAccounts()
			found := false
			for j := range f.Accounts {
				if f.Accounts[j].Name == "local" {
					f.Accounts[j].Cookie = cookie
					f.Accounts[j].Note = "Web 登录导入"
					f.Accounts[j].CreatedAt = time.Now().Unix()
					found = true
					break
				}
			}
			if !found {
				f.Accounts = append(f.Accounts, loomyAccount{
					Name: "local", Cookie: cookie, Note: "Web 登录导入", CreatedAt: time.Now().Unix(),
				})
			}
			err = saveLoomyAccounts(f)
			loomyFileMu.Unlock()
			if err != nil {
				log.Printf("loomy Web 登录：保存失败 %v", err)
				return
			}
			log.Printf("loomy Web 登录：账号已导入（cookie %d 字节）", len(sess))
			return
		}
		log.Printf("loomy Web 登录：5 分钟内没读到 cookie，放弃（可手动点「导入本机 Loomy」）")
	}()

	panelJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"hint": "登录窗口已打开：用手机号 + 验证码登录，然后关闭窗口，账号会自动导入",
	})
}
