//go:build !windows

package codearts

import "errors"

// ImportFromIDE 在非 Windows 平台不可用：码道 IDE 的凭据存储走 Windows DPAPI。
func ImportFromIDE() (Credential, error) {
	return Credential{}, errors.New("codearts: 从 IDE 导入凭据仅在 Windows 可用")
}

// IDEUserDataDir 在非 Windows 平台没有对应位置。
func IDEUserDataDir() string { return "" }
