package server

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"unified-api-v2/internal/browsercookie"
)

// 豆包桌面客户端（Electron）的数据目录。
//
// 为什么要读它：msToken 只存在于客户端的 Cookies 库里，浏览器页面不下发。
// 直接从客户端取，用户就完全不需要手动复制 Cookie —— 这是「傻瓜式」的关键。
func doubaoClientUserData() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return ""
	}
	return filepath.Join(base, "Doubao", "User Data")
}

// panelDoubaoImportClient POST /api/panel/doubao/import-client
//
// 从「正在运行的豆包客户端」读取 Cookie（含 msToken）并导入账号池。
//
// 技术要点：客户端运行时 Cookies 库是打开的，但 Windows 允许共享读，
// 所以先复制到临时目录再解析（不直接锁库，也不要求用户退出客户端）。
func (h *Handler) panelDoubaoImportClient(w http.ResponseWriter, r *http.Request) {
	userData := doubaoClientUserData()
	if userData == "" {
		writeOpenAIError(w, http.StatusBadRequest, "doubao_client_missing",
			"未找到 %LOCALAPPDATA%，无法定位豆包客户端")
		return
	}
	if _, err := os.Stat(userData); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "doubao_client_missing",
			"未找到豆包客户端数据目录，请确认已安装并登录过客户端")
		return
	}

	// 复制到临时目录：避免与运行中的客户端争锁。
	tmp, err := os.MkdirTemp("", "doubao-ck-*")
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "temp_failed", err.Error())
		return
	}
	defer os.RemoveAll(tmp)

	netDir := filepath.Join(tmp, "Default", "Network")
	if err := os.MkdirAll(netDir, 0o755); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "temp_failed", err.Error())
		return
	}
	// Local State 里是解密 Cookies 用的密钥，必须一起复制。
	pairs := [][2]string{
		{filepath.Join(userData, "Local State"), filepath.Join(tmp, "Local State")},
		{filepath.Join(userData, "Default", "Network", "Cookies"), filepath.Join(netDir, "Cookies")},
		{filepath.Join(userData, "Default", "Network", "Cookies-journal"), filepath.Join(netDir, "Cookies-journal")},
	}
	for _, p := range pairs {
		if err := copyShared(p[0], p[1]); err != nil {
			// Cookies 主库必须成功，其余（journal）缺失不影响解析。
			if filepath.Base(p[0]) == "Cookies" {
				writeOpenAIError(w, http.StatusInternalServerError, "doubao_cookie_copy_failed",
					"复制 Cookies 库失败: "+err.Error())
				return
			}
		}
	}

	values, err := browsercookie.Read(tmp, "Default", "doubao.com")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "doubao_client_unreadable", err.Error())
		return
	}
	if values["sessionid"] == "" {
		writeOpenAIError(w, http.StatusBadRequest, "doubao_not_logged_in",
			"客户端里没有登录态：请先在豆包客户端登录")
		return
	}
	if values["msToken"] == "" {
		writeOpenAIError(w, http.StatusBadRequest, "doubao_no_mstoken",
			"客户端 Cookie 里没有 msToken（请确认客户端已登录且进过对话页）")
		return
	}

	cookie := joinCookie(values)
	if err := h.upsertDoubaoAccount(cookie, values["web_id"], values["device_id"]); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "doubao_save_failed", err.Error())
		return
	}
	if _, st := h.doubaoAdmin(http.MethodPost, "/admin/reload", nil); st >= 300 {
		log.Printf("豆包客户端导入：reload 返回 %d", st)
	}
	log.Printf("豆包客户端导入成功：cookie %d 字节，含 msToken", len(cookie))
	panelJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"cookie_len":  len(cookie),
		"has_mstoken": true,
		"fields":      len(values),
	})
}

// copyShared 以共享读方式复制文件（对方进程可能正打开它）。
func copyShared(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", filepath.Base(src), err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建 %s: %w", filepath.Base(dst), err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("复制 %s: %w", filepath.Base(src), err)
	}
	return nil
}
