//go:build windows

// service_windows.go 把网关注册成 Windows 服务。
//
// 为什么需要：用计划任务 + `cmd /c run.cmd` 托管时，子进程与 cmd 共享控制台，
// 会话结束或外壳收到关闭事件时 exe 会被一起带走，而任务状态仍显示 Running
// （2026-11-22 实测：日志打出 `bye` 后 8900 拒连，任务却是 Running）。
// 注册成服务后由 SCM 托管，并可用 `sc failure` 配置失败自动重启。
package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows/svc"
)

const serviceName = "UnifiedAPIV2"

type serviceHandler struct {
	run func()
}

// Execute 是 SCM 的回调：启动业务逻辑，并把 Stop/Shutdown 转成状态变更。
func (h *serviceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}
	go h.run()
	status <- svc.Status{State: svc.Running, Accepts: accepted}
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			status <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

// isWindowsService 判断当前进程是否由 SCM 启动。
func isWindowsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

func runAsService() {
	if err := svc.Run(serviceName, &serviceHandler{run: runServer}); err != nil {
		log.Printf("服务运行失败: %v", err)
	}
}

// redirectServiceLogs 把日志显式追加到 gateway.err.log。
//
// 只写文件，不再 tee 到 stderr：服务模式没有控制台；-detach 模式调了
// FreeConsole 之后 os.Stderr 已失效，MultiWriter 会在第一处 Write 失败并
// 吞掉后续日志（实测：进程在跑，但 gateway.err.log 一行不写）。
func redirectServiceLogs() {
	f, err := os.OpenFile("gateway.err.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(f)
}

func exePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Abs(exe)
}

// installService 注册服务并配置失败自动重启（restart 5s / 10s / 30s）。
func installService() error {
	exe, err := exePath()
	if err != nil {
		return err
	}
	// 同名旧服务先清掉，保证幂等；失败不代表有旧服务，忽略即可。
	_ = exec.Command("sc", "stop", serviceName).Run()
	_ = exec.Command("sc", "delete", serviceName).Run()

	out, err := exec.Command("sc", "create", serviceName,
		"binPath=", exe, "start=", "auto",
		"DisplayName=", "Unified API v2").CombinedOutput()
	if err != nil {
		return fmt.Errorf("sc create 失败: %v\n%s", err, out)
	}
	// 失败自动重启：这是相对计划任务的核心改进。
	if out, err := exec.Command("sc", "failure", serviceName,
		"reset=", "86400",
		"actions=", "restart/5000/restart/10000/restart/30000").CombinedOutput(); err != nil {
		return fmt.Errorf("sc failure 配置失败: %v\n%s", err, out)
	}
	return nil
}

func uninstallService() error {
	_ = exec.Command("sc", "stop", serviceName).Run()
	out, err := exec.Command("sc", "delete", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sc delete 失败: %v\n%s", err, out)
	}
	return nil
}

// startService / stopService 供自动化脚本调用（也可直接用 sc）。
func startService() error {
	out, err := exec.Command("sc", "start", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sc start 失败: %v\n%s", err, out)
	}
	return nil
}

func stopService() error {
	out, err := exec.Command("sc", "stop", serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("sc stop 失败: %v\n%s", err, out)
	}
	return nil
}
