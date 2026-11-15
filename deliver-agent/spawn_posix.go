//go:build !windows

package main

// spawnDetached（POSIX）：setsid 分离进程——父进程退出后孤儿存活，不被终端挂断波及
import (
	"os/exec"
	"syscall"
)

func spawnDetached(exe string, args ...string) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}

// setPgroup cmd 独立进程组：终端中断可整组终止（sh 与其子进程如 docker logs -f，防孤儿续跑）
func setPgroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killGroup 杀整个进程组（尽力而为；进程已退出时错误忽略）
func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
