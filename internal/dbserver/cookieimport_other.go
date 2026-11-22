//go:build !windows

package dbserver

import "errors"

// LocalProfileCookie 非 Windows 平台占位（DPAPI 是 Windows 专有机制）。
type LocalProfileCookie struct {
	Profile string
	Name    string
	UID     string
	Cookie  string
	Keys    int
}

// DoubaoLocalCookies 非 Windows 平台占位。
type DoubaoLocalCookies struct {
	UserData string
	Accounts []LocalProfileCookie
	Skipped  []string
}

// ImportDoubaoLocalCookies 非 Windows 平台不支持从本机客户端导入。
func ImportDoubaoLocalCookies() (*DoubaoLocalCookies, error) {
	return nil, errors.New("从本机豆包客户端导入仅在 Windows 支持（依赖 DPAPI）")
}
