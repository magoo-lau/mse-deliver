//go:build windows

package main

// 本机终端命令的进程组控制（与 deliver-agent/spawn_windows.go 同构，双模块各自维护）
import (
	"os/exec"
	"strconv"
	"syscall"
)

// createNoWindow CREATE_NO_WINDOW（syscall 包未导出，值取 winbase.h；taskkill 不闪控制台窗）
const createNoWindow = 0x08000000

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
