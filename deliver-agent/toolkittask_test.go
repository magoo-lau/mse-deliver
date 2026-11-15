package main

// toolkit 任务单测：载荷契约（与 console 侧 protocol.go 镜像）、未知工具拒绝、幂等快路径
//（PATH 假 docker shim 同时充当 compose 插件）、在线下载脚本构造。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ToolkitPayload JSON 契约（字段改名在此暴露，console 侧 protocol.go 镜像）
func TestToolkitPayloadContract(t *testing.T) {
	raw := `{"app":"compose","tool":"compose","mediaName":"docker-compose","mediaSha256":"abc123",` +
		`"mediaSize":60000000,"mediaVer":"v2.30.3","onlineUrl":"https://example.com/dc","onlineSha256":"ff",` +
		`"force":true,"timeoutSec":600}`
	var p ToolkitPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if p.App != "compose" || p.Tool != "compose" || p.MediaName != "docker-compose" || p.MediaSHA256 != "abc123" ||
		p.MediaSize != 60000000 || p.MediaVer != "v2.30.3" || p.OnlineURL != "https://example.com/dc" ||
		p.OnlineSHA256 != "ff" || !p.Force || p.TimeoutSec != 600 {
		t.Fatalf("载荷解析异常: %+v", p)
	}
}

// 未知工具在平台/root 校验之前即拒绝
func TestExecToolkitUnknownTool(t *testing.T) {
	a := &agent{server: "http://127.0.0.1:1", state: &agentState{}, api: &http.Client{Timeout: 200 * time.Millisecond}, dl: &http.Client{}}
	rep := newReporter(a, &Task{SessionID: "s", NodeID: "n"})
	err := a.execToolkit(ToolkitPayload{Tool: "podman"}, rep)
	if err == nil || !strings.Contains(err.Error(), "未知工具") {
		t.Fatalf("未知工具应拒绝: %v", err)
	}
}

// dockerComposeShim 构造 PATH 假 docker：对任意参数输出固定版本号（probeDocker 与 probeCompose 共用）
func dockerComposeShim(t *testing.T, ver string) {
	t.Helper()
	shimDir := t.TempDir()
	var name string
	if runtime.GOOS == "windows" {
		name = "docker.bat"
		if err := os.WriteFile(filepath.Join(shimDir, name), []byte("@echo off\r\necho "+ver+"\r\n"), 0644); err != nil {
			t.Fatal(err)
		}
	} else {
		name = "docker"
		if err := os.WriteFile(filepath.Join(shimDir, name), []byte("#!/bin/sh\necho "+ver+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// 幂等快路径：compose 已装且（无目标版本或==目标，前导 v 归一）→ 直接 ok，不进入安装分支
func TestExecToolkitIdempotent(t *testing.T) {
	dockerComposeShim(t, "2.30.3")
	a := &agent{server: "http://127.0.0.1:1", state: &agentState{}, api: &http.Client{Timeout: 200 * time.Millisecond}, dl: &http.Client{}}
	rep := newReporter(a, &Task{SessionID: "s", NodeID: "n"})
	if err := a.execToolkit(ToolkitPayload{Tool: "compose"}, rep); err != nil {
		t.Fatal("无目标版本且已装应走快路径:", err)
	}
	if err := a.execToolkit(ToolkitPayload{Tool: "compose", MediaVer: "v2.30.3"}, rep); err != nil {
		t.Fatal("已是目标版本应走快路径:", err)
	}
}

// 在线下载脚本：curl 优先退化 wget，路径/URL 单引号包裹（URL 含 & 等字符防 shell 切词）
func TestComposeOnlineScript(t *testing.T) {
	got := composeOnlineScript("/usr/local/lib/docker/cli-plugins/docker-compose.part", "https://x.example.com/dl?a=1&b=2")
	if !strings.Contains(got, `curl -fL -o '/usr/local/lib/docker/cli-plugins/docker-compose.part' 'https://x.example.com/dl?a=1&b=2'`) {
		t.Fatalf("curl 段构造异常: %s", got)
	}
	if !strings.Contains(got, `wget -qO '/usr/local/lib/docker/cli-plugins/docker-compose.part' 'https://x.example.com/dl?a=1&b=2'`) {
		t.Fatalf("wget 段构造异常: %s", got)
	}
}
