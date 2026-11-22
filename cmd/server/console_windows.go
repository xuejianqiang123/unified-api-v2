//go:build windows

package main

import "syscall"

var modKernel32 = syscall.NewLazyDLL("kernel32.dll")

// detachConsole 让当前进程脱离控制台。
//
// 为什么需要：计划任务以交互方式启动控制台程序时，Windows 会给它分配一个
// 窗口，于是每次启动/看护拉起都会留下一个黑窗口（旧版包装脚本还会因为
// GBK 解析 UTF-8 注释而刷出乱码）。调用 FreeConsole 后进程继续运行但不再
// 拥有控制台，窗口随即消失——只会在启动瞬间闪一下。
//
// 代价：stdout/stderr 不再可用，日志必须显式写文件（见 redirectServiceLogs）。
func detachConsole() {
	proc := modKernel32.NewProc("FreeConsole")
	_, _, _ = proc.Call()
}
