//go:build !windows

package browsercookie

import "errors"

// Read 非 Windows 平台不支持：DPAPI 是 Windows 专有机制。
func Read(userDataDir, profile, hostSuffix string) (map[string]string, error) {
	return nil, errors.New("读取本机浏览器 cookie 仅在 Windows 支持（依赖 DPAPI）")
}

// JoinHeader 在非 Windows 下只保留接口一致性（无调用方）。
func JoinHeader(m map[string]string, prefer []string) string { return "" }
