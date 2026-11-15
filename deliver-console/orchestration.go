// 编排导出/导入（跨环境分发部署架构）：会话 → mse-deliver-orchestration v1 JSON 文件。
//
// 文件不含镜像介质，只引用包身份（app+version+pkgSha，导入库须有同名包）；主机槽位化
// （slot id = h{原主机ID}，带名称与能力快照）——导入时由目标 console 重选设备映射；
// 敏感变量（VarDecl.Sensitive + 组件凭据占位符）值不导出（键保留置空，导入后重填/留空自动重生成），
// redacted 列出被置空项。DAG 节点 key 内嵌主机号（load-h3 等）在文件中保持原样——导入端按
// 槽位映射重写 h{旧}→h{新} 后 key 与目标环境自动计划对齐（引擎 overlayDag 复用物化载荷）。
package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"
)

const (
	orchKind   = "mse-deliver-orchestration"
	orchFormat = 1
)

type orchFile struct {
	Kind          string        `json:"kind"`
	Format        int           `json:"format"`
	ExportedAt    string        `json:"exportedAt"`
	Package       orchPkgRef    `json:"package"`
	DockerInstall bool          `json:"dockerInstall,omitempty"`
	Slots         []orchSlot    `json:"slots"`
	Placement     orchPlacement `json:"placement"`
	Dag           []orchDagNode `json:"dag"`
	Redacted      []string      `json:"redacted,omitempty"` // 敏感置空的变量名（导入后需重填/自动重生成）
}

type orchPkgRef struct {
	App     string `json:"app"`
	Version string `json:"version"`
	PkgSha  string `json:"pkgSha,omitempty"`
}

type orchSlot struct {
	ID      string `json:"id"` // h{原主机ID}——placement.host 与 dag key/host 以此引用
	Name    string `json:"name"`
	OS      string `json:"os,omitempty"`
	Arch    string `json:"arch,omitempty"`
	Docker  bool   `json:"docker,omitempty"`
	Systemd bool   `json:"systemd,omitempty"`
	JavaVer string `json:"javaVersion,omitempty"`
	Labels  string `json:"labels,omitempty"`
}

type orchPlacement struct {
	Mode map[string]string `json:"mode"`
	Host map[string]string `json:"host"` // svc → 槽位 id
	Vars map[string]string `json:"vars"` // 敏感置空
	Ext  map[string]string `json:"ext"`
	Skip []string          `json:"skip,omitempty"`    // 跳过的组件编码（不装不复用）
	Instances []InstanceSpec     `json:"instances,omitempty"` // console 组态器：组件实例化拓扑（ADR-11；实例主机随 host 槽位化）
	Resources map[string]ResourceSpec `json:"resources,omitempty"` // svc → 资源限额
}

type orchDagNode struct {
	ID     string   `json:"id"`
	Kind   string   `json:"kind"`
	Title  string   `json:"title,omitempty"`
	Host   string   `json:"host,omitempty"` // 槽位 id（gate 无主机）
	Script string   `json:"script,omitempty"`
	Note   string   `json:"note,omitempty"`
	Mode   string   `json:"mode,omitempty"`
	Deps   []string `json:"deps"`
}

// handleSessionExport GET /api/sessions/{id}/export：从会话快照权威导出（分配 + 计划 DAG）
func (s *Server) handleSessionExport(w http.ResponseWriter, r *http.Request, id int64) {
	ss, err := s.store.SessionByID(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "会话不存在")
		return
	}
	var pl Placement
	if ss.Placement == "" || json.Unmarshal([]byte(ss.Placement), &pl) != nil || len(pl.Host) == 0 {
		writeErr(w, http.StatusBadRequest, "本会话无分配快照（Docker 引导会话或旧版会话），无可导出编排")
		return
	}
	hosts := map[int64]*hostRow{}
	list, _ := s.store.ListHosts()
	for _, h := range list {
		hosts[h.ID] = h
	}
	snap := LoadPlanSnapshot(ss.Plan)
	out := orchFile{Kind: orchKind, Format: orchFormat,
		ExportedAt:    time.Now().Format(time.RFC3339),
		Package:       orchPkgRef{App: ss.App, Version: ss.Version, PkgSha: ss.PkgSha},
		DockerInstall: pl.DockerInstall,
		Placement: orchPlacement{Mode: pl.Mode, Host: map[string]string{},
			Vars: map[string]string{}, Ext: pl.Ext, Skip: pl.Skip,
			Instances: pl.Instances, Resources: pl.Resources},
		Dag: []orchDagNode{}}
	// 槽位：分配 + DAG 节点引用到的主机（升序编号 h{ID}）
	slotIDs := map[int64]string{}
	addSlot := func(hostID int64) string {
		if hostID == 0 {
			return ""
		}
		if id, ok := slotIDs[hostID]; ok {
			return id
		}
		id := "h" + strconv.FormatInt(hostID, 10)
		slotIDs[hostID] = id
		slot := orchSlot{ID: id}
		if h := hosts[hostID]; h != nil {
			slot.Name, slot.OS, slot.Arch = h.Name, h.Info.OS, h.Info.Arch
			slot.Docker, slot.Systemd, slot.JavaVer, slot.Labels = h.Info.Docker, h.Info.Systemd, h.Info.JavaVer, h.Labels
		} else {
			slot.Name = "（已删除主机 #" + strconv.FormatInt(hostID, 10) + "）"
		}
		out.Slots = append(out.Slots, slot)
		return id
	}
	for _, svc := range sortedServiceNames(pl.Host) {
		out.Placement.Host[svc] = addSlot(pl.Host[svc])
	}
	for _, n := range snap.Nodes {
		dn := orchDagNode{ID: n.Key, Kind: n.Kind, Title: n.Title, Host: addSlot(n.Host), Deps: n.Deps}
		if n.Deps == nil {
			dn.Deps = []string{}
		}
		switch n.Kind { // 载荷里的自定义内容回填为声明字段
		case "cmd":
			var p CmdPayload
			if json.Unmarshal(n.Payload, &p) == nil {
				dn.Script = p.Script
			}
		case "plugin":
			var p PluginPayload
			if json.Unmarshal(n.Payload, &p) == nil && p.Op == "apply" {
				dn.Mode = p.Mode
			}
		}
		out.Dag = append(out.Dag, dn)
	}
	// 敏感变量置空：包已删（无法判定敏感面）→ 全部置空
	vars := map[string]string{}
	_ = json.Unmarshal([]byte(ss.Vars), &vars)
	sensitive := s.sensitiveVarSet(ss.PackageID)
	for k := range vars {
		if sensitive == nil || sensitive[k] {
			out.Redacted = append(out.Redacted, k)
			vars[k] = ""
		}
	}
	sort.Strings(out.Redacted)
	out.Placement.Vars = vars
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+sanitizeSeg(ss.App)+"-"+sanitizeSeg(ss.Version)+`-orchestration.json"`)
	writeJSON(w, out)
}

// sensitiveVarSet 敏感变量集合：VarDecl.Sensitive + 组件凭据占位符；包缺失返回 nil（=全部敏感）
func (s *Server) sensitiveVarSet(pkgID int64) map[string]bool {
	row, err := s.store.PackageByID(pkgID)
	if err != nil {
		return nil
	}
	pp, err := parsePackageDir(row.Dir)
	if err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, v := range pp.Decl.Variables {
		if v.Sensitive {
			set[v.Name] = true
		}
	}
	for _, c := range pp.Decl.Components {
		for _, cred := range c.Credentials {
			set[cred.Placeholder] = true
		}
	}
	return set
}

func sortedServiceNames(host map[string]int64) []string {
	var out []string
	for svc := range host {
		out = append(out, svc)
	}
	sort.Strings(out)
	return out
}
