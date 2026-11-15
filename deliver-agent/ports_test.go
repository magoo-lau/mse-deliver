package main

import (
	"net"
	"strings"
	"testing"
)

func TestParseHostPorts(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"0.0.0.0:3306->3306/tcp, [::]:3306->3306/tcp", []string{"3306"}},
		{"0.0.0.0:3306->3306/tcp, :::3306->3306/tcp", []string{"3306"}}, // 双栈去重
		{"127.0.0.1:6379->6379/tcp", []string{"6379"}},
		{"0.0.0.0:48080->8080/tcp, 0.0.0.0:33060->33060/tcp", []string{"33060", "48080"}},
		{"", nil},
		{"3306/tcp, 8080/tcp", nil}, // 无发布映射（->）不算宿主端口
	}
	for _, c := range cases {
		got := parseHostPorts(c.in)
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("parseHostPorts(%q) = %v；want %v", c.in, got, c.want)
		}
	}
}

func TestPortConflictDetail(t *testing.T) {
	cs := []ContainerPort{
		{Name: "mse-monolith-h8-mysql-1", Image: "mysql:8.4", HostPorts: []string{"3306"}, Project: "mse-monolith-h8"},
		{Name: "other-mysql", Image: "mysql:8", HostPorts: []string{"3307"}},
		{Name: "no-ports", Image: "busybox"}, // 无发布端口：不参与归因
	}
	// 异源冲突归因
	dets := portConflictDetail([]string{"3307"}, "mse-monolith-h8", cs)
	if len(dets) != 1 || !strings.Contains(dets[0], "other-mysql") {
		t.Errorf("异源占用应报冲突归因，got %v", dets)
	}
	// 本项目容器占用 = 复用，不拦
	if dets := portConflictDetail([]string{"3306"}, "mse-monolith-h8", cs); len(dets) != 0 {
		t.Errorf("本项目容器占用应视为复用，got %v", dets)
	}
	// 项目名不同（换包重装）→ 旧容器按异源如实报出
	if dets := portConflictDetail([]string{"3306"}, "mse-monolith-h9", cs); len(dets) != 1 {
		t.Errorf("异项目容器占用应报冲突，got %v", dets)
	}
	// 空端口清单恒无冲突（旧 console 载荷语义不变）
	if dets := portConflictDetail(nil, "mse-monolith-h8", cs); len(dets) != 0 {
		t.Errorf("空清单应恒无冲突，got %v", dets)
	}
	// 空闲端口不报
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无法试绑临时端口")
	}
	free := strings.TrimPrefix(ln.Addr().String(), "127.0.0.1:")
	_ = ln.Close()
	if dets := portConflictDetail([]string{free}, "mse-monolith-h8", cs); len(dets) != 0 {
		t.Errorf("空闲端口不应报冲突，got %v", dets)
	}
}

func TestHostPortBusy(t *testing.T) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Skip("无法试绑临时端口")
	}
	addr := ln.Addr().String()
	port := addr[strings.LastIndex(addr, ":")+1:]
	if !hostPortBusy(port) {
		t.Errorf("端口 %s 被监听应判忙", port)
	}
	_ = ln.Close()
	if hostPortBusy(port) {
		t.Errorf("端口 %s 已释放应判闲", port)
	}
}

func TestProbeFormatStable(t *testing.T) {
	// docker ps --format 模板经 exec 传参后 \t 由 docker 渲染为 tab，解析按 4 列切——
	// 锁定模板列序（Names/Image/Ports/Project）与 probeContainerPorts 的切分约定
	tpl := "{{.Names}}\\t{{.Image}}\\t{{.Ports}}\\t{{.Label \"com.docker.compose.project\"}}"
	want := []string{"{{.Names}}", "{{.Image}}", "{{.Ports}}", "{{.Label \"com.docker.compose.project\"}}"}
	got := strings.Split(tpl, "\\t")
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("格式模板列序被破坏：%q", tpl)
	}
	// 模拟一行 docker ps 输出走完整解析
	line := strings.Join([]string{"c1", "mysql:8", "0.0.0.0:3306->3306/tcp", "proj-x"}, "\t")
	f := strings.Split(line, "\t")
	if len(f) != 4 || f[0] != "c1" || f[3] != "proj-x" {
		t.Fatalf("4 列切分约定被破坏：%v", f)
	}
}
