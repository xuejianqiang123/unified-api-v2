//go:build !windows

package main

// detachConsole 在非 Windows 平台为空实现（无控制台窗口概念）。
func detachConsole() {}
