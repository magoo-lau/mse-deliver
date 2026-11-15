//go:build windows

package main

// spawnDetached（Windows）：DETACHED + 新进程组——脱离控制台与父进程，父（旧 agent）正常
// 退出后作为孤儿存活执行换代（schtasks /End + /Run）
import (
	"os/exec"
	"strconv"
	"syscall"
)

// detachedProcess DETACHED_PROCESS（syscall 包未导出，值取 winbase.h）
const detachedProcess = 0x00000008

// createNoWindow CREATE_NO_WINDOW（同上；taskkill 不闪控制台窗）
const createNoWindow = 0x08000000

func spawnDetached(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess,
	}
	return cmd.Start()
}

// setPgroup Windows 无 POSIX 进程组语义（no-op）：整树终止由 killGroup 的 taskkill /T 承担
func setPgroup(*exec.Cmd) {}

// killGroup 杀整棵进程树（taskkill /T /F）：主进程（sh/powershell）被杀后其子进程（如
// docker logs -f）会成为孤儿并握住 stdout 管道，令任务收尾永久挂起——必须连树一并终止
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	c := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	_ = c.Run()
}
