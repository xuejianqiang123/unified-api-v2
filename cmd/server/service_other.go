//go:build !windows

package main

import "errors"

// 非 Windows 平台没有 SCM，服务相关能力统一返回明确错误。

const serviceName = "UnifiedAPIV2"

func isWindowsService() bool { return false }

func runAsService() {}

func redirectServiceLogs() {}

func installService() error {
	return errors.New("服务安装仅在 Windows 支持")
}

func uninstallService() error {
	return errors.New("服务卸载仅在 Windows 支持")
}

func startService() error { return errors.New("仅 Windows 支持") }
func stopService() error  { return errors.New("仅 Windows 支持") }
