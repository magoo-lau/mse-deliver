package main

// precheckPorts 建会话端口硬校验单测：异源冲突拒绝 / 本项目复用放行 / 档案缺失降级。

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func mkPrecheckBuilder(hosts map[int64]*hostRow) *builder {
	// pp 提供应用名（同包历史项目前缀判定 sanitizeSeg(app)+"-h" 用）
	return &builder{hosts: hosts, pp: &ParsedPackage{Decl: &ComponentsDecl{Name: "mse-app"}}}
}

func mkComposeNode(host int64, project string, ports ...string) planNode {
	return planNode{Key: fmt.Sprintf("compose-h%d", host), Kind: "compose", Host: host,
		Payload: mustJSON(ComposePayload{App: "app", Project: project, Ports: ports})}
}

func TestPrecheckPortsForeignConflict(t *testing.T) {
	b := mkPrecheckBuilder(map[int64]*hostRow{
		8: {ID: 8, Name: "node2", Info: HostInfo{Docker: true, Containers: []ContainerPort{
			{Name: "other-mysql", Image: "mysql:8", HostPorts: []string{"3306"}},
		}}},
	})
	err := b.precheckPorts([]planNode{mkComposeNode(8, "mse-app-h8", "3306", "6379")})
	if err == nil {
		t.Fatal("异源占用应拒绝建会话")
	}
	if !strings.Contains(err.Error(), "other-mysql") || !strings.Contains(err.Error(), "3306") {
		t.Errorf("冲突明细应带占用者归因: %v", err)
	}
}

func TestPrecheckPortsLegacyProject(t *testing.T) {
	// 同包历史遗留（旧主机分配 demo-h9 容器留在本机，当前项目 mse-app-h8）：拒绝但文案区分档位
	b := mkPrecheckBuilder(map[int64]*hostRow{
		8: {ID: 8, Name: "node2", Info: HostInfo{Docker: true, Containers: []ContainerPort{
			{Name: "mse-app-h9-mysql-1", Image: "mysql:8.4", HostPorts: []string{"3306"}, Project: "mse-app-h9"},
		}}},
	})
	err := b.precheckPorts([]planNode{mkComposeNode(8, "mse-app-h8", "3306")})
	if err == nil {
		t.Fatal("同包历史遗留占用应拒绝建会话")
	}
	if !strings.Contains(err.Error(), "本应用历史部署") || !strings.Contains(err.Error(), "mse-app-h9") {
		t.Errorf("遗留档应带项目归因与清理指引: %v", err)
	}
	// 前缀不误判：别家应用 mse-app2 的容器按异源口径（无「历史部署」字样）
	b2 := mkPrecheckBuilder(map[int64]*hostRow{
		8: {ID: 8, Name: "node2", Info: HostInfo{Docker: true, Containers: []ContainerPort{
			{Name: "mse-app2-h8-mysql-1", Image: "mysql:8", HostPorts: []string{"3306"}, Project: "mse-app2-h8"},
		}}},
	})
	err2 := b2.precheckPorts([]planNode{mkComposeNode(8, "mse-app-h8", "3306")})
	if err2 == nil || strings.Contains(err2.Error(), "本应用历史部署") {
		t.Errorf("mse-app2 不属于本包，应按异源口径: %v", err2)
	}
}

func TestPrecheckPortsOwnProjectReuse(t *testing.T) {
	b := mkPrecheckBuilder(map[int64]*hostRow{
		8: {ID: 8, Name: "node2", Info: HostInfo{Docker: true, Containers: []ContainerPort{
			{Name: "mse-app-h8-mysql-1", Image: "mysql:8", HostPorts: []string{"3306"}, Project: "mse-app-h8"},
		}}},
	})
	if err := b.precheckPorts([]planNode{mkComposeNode(8, "mse-app-h8", "3306")}); err != nil {
		t.Fatalf("本项目容器占用应放行（升级接管）: %v", err)
	}
	if len(b.warnings) != 1 || !strings.Contains(b.warnings[0], "原地升级") {
		t.Errorf("升级接管应出信息提示: %v", b.warnings)
	}
}

func TestPrecheckPortsNoDataDegrade(t *testing.T) {
	// agent 版本较旧/心跳未达：无档案不阻断（执行期 agent 自检兜底）
	b := mkPrecheckBuilder(map[int64]*hostRow{
		8: {ID: 8, Name: "node2", Info: HostInfo{Docker: true}}, // Containers=nil
	})
	if err := b.precheckPorts([]planNode{mkComposeNode(8, "mse-app-h8", "3306")}); err != nil {
		t.Fatalf("档案缺失应降级放行: %v", err)
	}
	if len(b.warnings) != 1 || !strings.Contains(b.warnings[0], "跳过端口预检") {
		t.Errorf("降级应出提示: %v", b.warnings)
	}
	// 无 docker 主机（DockerInstall 装完才有容器）：无从预检，静默放行
	b2 := mkPrecheckBuilder(map[int64]*hostRow{
		8: {ID: 8, Name: "node2", Info: HostInfo{}},
	})
	if err := b2.precheckPorts([]planNode{mkComposeNode(8, "mse-app-h8", "3306")}); err != nil {
		t.Fatalf("无 docker 主机应放行: %v", err)
	}
	if len(b2.warnings) != 0 {
		t.Errorf("无 docker 主机不应出提示: %v", b2.warnings)
	}
}

func TestPrecheckPortsPayloadCarriesPorts(t *testing.T) {
	// compose 载荷应带宿主端口（agent 执行前自检的输入；本测锁定序列化字段名）
	p := ComposePayload{App: "app", Project: "p", Ports: []string{"3306"}}
	raw, _ := json.Marshal(p)
	if !strings.Contains(string(raw), `"ports":["3306"]`) {
		t.Fatalf("Ports 序列化字段缺失: %s", raw)
	}
}
