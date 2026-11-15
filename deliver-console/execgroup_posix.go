//go:build !windows

package main

// 本机终端命令的进程组控制（与 deliver-agent/spawn_posix.go 同构，双模块各自维护）
import (
	"os/exec"
	"syscall"
)

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
