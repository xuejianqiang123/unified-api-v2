//go:build windows

package dbserver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LocalProfileCookie 某个 Electron profile 读出的一个豆包登录态。
type LocalProfileCookie struct {
	Profile string // Default / Profile 1 ...
	Name    string // 账号名：local-<uid 前 8 位>，同一账号跨 profile 稳定
	UID     string // uid_tt（账号级标识，用于去重与命名）
	Cookie  string // 已拼成 "k=v; k=v"
	Keys    int
}

// DoubaoLocalCookies 本机所有 profile 的导入结果。
type DoubaoLocalCookies struct {
	UserData string
	Accounts []LocalProfileCookie
	Skipped  []string // 跳过的 profile 及原因（无 cookie / 未登录）
}

// doubaoUserDataDir 定位豆包桌面客户端的 Electron User Data 目录。
func doubaoUserDataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return ""
	}
	p := filepath.Join(base, "Doubao", "User Data")
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	return ""
}

// listProfiles 返回 User Data 下的所有 profile 目录（Default、Profile N）。
func listProfiles(userDataDir string) []string {
	entries, err := os.ReadDir(userDataDir)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n := e.Name()
		if n == "Default" || (strings.HasPrefix(n, "Profile") && len(n) > 7) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// ImportDoubaoLocalCookies 遍历本机豆包客户端的每个 profile，读出各自的登录态。
//
// 为什么必须遍历：客户端按 profile 隔离登录，同一个客户端可以同时登录多个豆包
// 账号（实测 Default 与 Profile 1 的 sessionid 完全不同）。只读 Default 会漏号。
//
// 账号名用 uid_tt 前 8 位生成（local-xxxxxxxx）：同一账号在不同 profile 下
// 命名一致，天然去重；note 里保留 profile 名便于排查。
func ImportDoubaoLocalCookies() (*DoubaoLocalCookies, error) {
	dir := doubaoUserDataDir()
	if dir == "" {
		return nil, errors.New("未找到豆包客户端数据目录（%LOCALAPPDATA%\\Doubao\\User Data），本机可能未安装")
	}
	key, err := loadChromiumAESKey(dir)
	if err != nil {
		return nil, err
	}
	res := &DoubaoLocalCookies{UserData: dir}
	for _, prof := range listProfiles(dir) {
		cookiePath := filepath.Join(dir, prof, "Network", "Cookies")
		if _, err := os.Stat(cookiePath); err != nil {
			continue
		}
		values, err := chromiumCookieValues(cookiePath, key, "doubao.com")
		if err != nil {
			res.Skipped = append(res.Skipped, prof+"："+err.Error())
			continue
		}
		if len(values) == 0 {
			res.Skipped = append(res.Skipped, prof+"：无豆包 cookie（未登录）")
			continue
		}
		// 关键项校验：缺 sessionid 说明不是有效登录态。
		missing := []string{}
		for _, must := range []string{"sessionid", "sid_guard"} {
			if strings.TrimSpace(values[must]) == "" {
				missing = append(missing, must)
			}
		}
		if len(missing) > 0 {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s：缺少 %s（未登录或已失效）",
				prof, strings.Join(missing, "/")))
			continue
		}
		uid := strings.TrimSpace(values["uid_tt"])
		if uid == "" {
			uid = strings.TrimSpace(values["__tea__ug__uid"])
		}
		name := "local"
		if len(uid) >= 8 {
			name = "local-" + uid[:8]
		} else if uid != "" {
			name = "local-" + uid
		}
		header := joinCookieHeader(values, []string{
			"LARK_SUITE_DPOP", "__tea__ug__uid", "_csrf_token", "bd_sso_hi3jfd", "d_ticket",
			"odin_tt", "passport_csrf_token", "passport_csrf_token_default", "s_v_web_id",
			"session_tlb_tag", "sessionid", "sessionid_ss", "sid_guard", "sid_tt",
			"sid_ucp_v1", "ssid_ucp_v1", "ttwid", "uid_tt", "uid_tt_ss",
		})
		res.Accounts = append(res.Accounts, LocalProfileCookie{
			Profile: prof, Name: name, UID: uid, Cookie: header, Keys: len(values),
		})
	}
	if len(res.Accounts) == 0 {
		msg := "没有读到可用的豆包登录态"
		if len(res.Skipped) > 0 {
			msg += "（" + strings.Join(res.Skipped, "；") + "）"
		}
		return nil, errors.New(msg)
	}
	return res, nil
}
