// desired.go —— 期望态（服务级）与统一 diff（K8s apply 模型：render → diff(desired, inventory) → plan）。
//
// 配置从「主机级单文件」重构为「服务级文件集」：format:3 单模板等价于同机每个单元服务各持一份
// data/conf/application.yaml（内容同主机渲染，行为兼容）；format:4 单元 ConfigFiles 与组件角色 Files
// 按「服务各自消费哪些文件」建模。配置哈希（conf-hash label 与 inventory confHash）随之落到服务级——
// 改一个服务的配置只 recreate 该容器，不再连带同主机其他服务（旧口径为主机级整串哈希）。
//
// 注意：哈希口径变更意味着存量 inventory detail 首次比对必然不等——升级后第一次增量安装会把全部
// 服务重算一遍（compose up 幂等，conf-hash label 变化触发一次性滚动重建，数据卷保留）。
package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// assignConfBySvc 物化服务级配置文件集（materialize 末段调用）：
//  1. format:3 缺省路径——config-template.yaml 按「消费主机」渲染（端点三态视角），同机每个单元
//     服务各得一份（docker 形态=data/conf/application.yaml，systemd=conf/application.yaml）；
//  2. format:4 单元 ConfigFiles——逐单元逐主机渲染，路径按声明（packager 责任，两种形态同路径）；
//  3. format:4 组件角色 Files——逐角色服务渲染（值集=变量+端点(消费主机视角)+角色变量覆盖）。
//
// 端点跨机引用（confCrossDeps）收集口径同步扩大到全部渲染模板（角色文件/多文件模板里引用的
// 跨机端点同样生成 wait 门控——与单模板口径一致）。
func (b *builder) assignConfBySvc() error {
	pp := b.pp
	b.confBySvc = map[string]map[string]string{}

	hasWork := func(hostID int64) bool {
		return len(b.dockerByHost[hostID]) > 0 || len(b.sysByHost[hostID]) > 0
	}
	unitBySvc := map[string]string{}
	for _, u := range pp.Decl.Units {
		for _, s := range unitServices(u) {
			unitBySvc[s] = u.Code
		}
	}

	// 端点解析（每工作主机一次；外部端点缺失即时报错——与单模板时代口径一致）
	epValsByHost := map[int64]map[string]string{}
	for hostID := range b.hosts {
		if !hasWork(hostID) {
			continue
		}
		values := map[string]string{}
		for k, v := range b.vars {
			values[k] = v
		}
		for _, c := range pp.Decl.Components {
			if b.skipComp[c.Code] {
				continue
			}
			for _, ep := range c.Endpoints {
				v, err := b.endpointValueFor(ep, hostID)
				if err != nil {
					return err
				}
				values[ep.Placeholder] = v
			}
		}
		epValsByHost[hostID] = values
	}

	// 跨机端点引用登记（占位符被本主机任一渲染模板实际引用且端点服务落机在别机）
	noteCross := func(hostID int64, files []FileDecl, extra map[string]string) {
		for _, f := range files {
			for _, m := range placeholderRe.FindAllStringSubmatch(f.Template, -1) {
				ph := m[1]
				if _, isRole := extra[ph]; isRole {
					continue
				}
				for _, c := range pp.Decl.Components {
					if b.skipComp[c.Code] {
						continue
					}
					for _, ep := range c.Endpoints {
						if ep.Placeholder != ph {
							continue
						}
						if depHost, placed := b.svcHost[ep.Service]; placed && depHost != hostID &&
							!containsStr(b.confCrossDeps[hostID], ep.Service) {
							b.confCrossDeps[hostID] = append(b.confCrossDeps[hostID], ep.Service)
						}
					}
				}
			}
		}
	}
	renderSet := func(files []FileDecl, values map[string]string, extra map[string]string) (map[string]string, error) {
		out := map[string]string{}
		vs := map[string]string{}
		for k, v := range values {
			vs[k] = v
		}
		for k, v := range extra {
			vs[k] = v
		}
		for _, f := range files {
			rendered, unknown := renderPlaceholders(f.Template, vs)
			if len(unknown) > 0 {
				return nil, fmt.Errorf("配置文件 %s 存在未知占位符 %v（变量/端点/凭据/角色变量均未覆盖）", f.Path, unknown)
			}
			out[f.Path] = rendered
		}
		return out, nil
	}

	// format:3 缺省模板：逐主机渲染一份，挂到该主机全部单元服务（保持旧挂载路径口径）
	var tpl string
	if data, err := readFileOrEmpty(filepath.Join(pp.PkgDir, templateFile)); err == nil {
		tpl = data
	}
	tpl = stripSkippedSections(tpl, b.skipComp)
	for hostID := range b.hosts {
		if !hasWork(hostID) || tpl == "" {
			continue
		}
		rendered, unknown := renderPlaceholders(tpl, epValsByHost[hostID])
		if len(unknown) > 0 {
			return fmt.Errorf("配置模板存在未知占位符 %v（变量/端点/凭据均未覆盖）", unknown)
		}
		// 跨机端点引用收集（单模板段）
		for _, m := range placeholderRe.FindAllStringSubmatch(tpl, -1) {
			ph := m[1]
			for _, c := range pp.Decl.Components {
				if b.skipComp[c.Code] {
					continue
				}
				for _, ep := range c.Endpoints {
					if ep.Placeholder == ph {
						if depHost, placed := b.svcHost[ep.Service]; placed && depHost != hostID &&
							!containsStr(b.confCrossDeps[hostID], ep.Service) {
							b.confCrossDeps[hostID] = append(b.confCrossDeps[hostID], ep.Service)
						}
					}
				}
			}
		}
		for _, svc := range append(append([]string{}, b.dockerByHost[hostID]...), b.sysByHost[hostID]...) {
			if unitBySvc[svc] == "" {
				continue // 组件服务不消费应用配置
			}
			if b.confBySvc[svc] == nil {
				b.confBySvc[svc] = map[string]string{}
			}
			if b.svcMode[svc] == "systemd" {
				b.confBySvc[svc]["conf/application.yaml"] = rendered
			} else {
				b.confBySvc[svc]["data/conf/application.yaml"] = rendered
			}
		}
	}

	// format:4 单元多配置文件：逐单元逐主机渲染（声明路径两形态一致；覆盖同路径旧条目）
	for _, u := range pp.Decl.Units {
		if len(u.ConfigFiles) == 0 {
			continue
		}
		for _, svc := range unitServices(u) {
			hostID, placed := b.svcHost[svc]
			if !placed {
				continue
			}
			files, err := renderSet(u.ConfigFiles, epValsByHost[hostID], nil)
			if err != nil {
				return fmt.Errorf("单元 %s（%s）：%w", u.Code, svc, err)
			}
			noteCross(hostID, u.ConfigFiles, nil)
			if b.confBySvc[svc] == nil {
				b.confBySvc[svc] = map[string]string{}
			}
			for k, v := range files {
				b.confBySvc[svc][k] = v
			}
		}
	}

	// format:4 组件角色文件：逐角色服务渲染（角色变量覆盖端点/凭据同名占位符）
	for _, c := range pp.Decl.Components {
		if b.skipComp[c.Code] {
			continue
		}
		for _, r := range c.Roles {
			if len(r.Files) == 0 {
				continue
			}
			for _, svc := range r.Services {
				hostID, placed := b.svcHost[svc]
				if !placed {
					continue
				}
				files, err := renderSet(r.Files, epValsByHost[hostID], r.Vars)
				if err != nil {
					return fmt.Errorf("组件 %s 角色 %s（%s）：%w", c.Code, r.Name, svc, err)
				}
				noteCross(hostID, r.Files, r.Vars)
				b.confBySvc[svc] = files
			}
		}
	}
	return nil
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// renderRoleScript 渲染角色脚本（exec 探针/初始化步骤共用）：值集=变量+端点（消费主机=角色服务
// 所在主机视角）+角色变量覆盖——与角色配置文件同源
func (b *builder) renderRoleScript(svc, script string) (string, error) {
	hostID, placed := b.svcHost[svc]
	if !placed {
		return "", fmt.Errorf("服务 %s 未落机，无法渲染角色脚本", svc)
	}
	values := map[string]string{}
	for k, v := range b.vars {
		values[k] = v
	}
	for _, c := range b.pp.Decl.Components {
		if b.skipComp[c.Code] {
			continue
		}
		for _, ep := range c.Endpoints {
			v, err := b.endpointValueFor(ep, hostID)
			if err != nil {
				return "", err
			}
			values[ep.Placeholder] = v
		}
	}
	if role := b.roleBySvc[svc]; role != nil {
		for k, v := range role.Vars {
			values[k] = v
		}
	}
	rendered, unknown := renderPlaceholders(script, values)
	if len(unknown) > 0 {
		return "", fmt.Errorf("服务 %s 角色脚本存在未知占位符 %v", svc, unknown)
	}
	return rendered, nil
}

// confHashOf 服务级配置指纹（排序路径拼接内容；空集=空串——无配置服务不注入 conf-hash label）
func confHashOf(files map[string]string) string {
	if len(files) == 0 {
		return ""
	}
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString("\x00")
		sb.WriteString(files[k])
		sb.WriteString("\x00")
	}
	return hashStr(sb.String())
}

// svcEnvHash 服务级环境指纹：只取该服务块实际引用的 ${VAR} 子集（改一个变量不再全量重建——
// 旧口径为 .env 整串哈希）
func (b *builder) svcEnvHash(svc string) string {
	meta := b.pp.Meta[svc]
	if meta == nil {
		return hashStr("")
	}
	names := map[string]bool{}
	for _, m := range envRefRe.FindAllStringSubmatch(meta.Block, -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		return hashStr("")
	}
	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString("=");
		sb.WriteString(b.vars[k])
		sb.WriteString("\n")
	}
	return hashStr(sb.String())
}

// ---- 统一 diff（UI 预览与引擎执行共用同一结果——单一口径） ----

// svcChange 服务级变更（K8s apply diff 对应物）：create=新增 / update=变更（含迁移）/
// remove=计划中不存在（增量 up --remove-orphans 清理）/ nochange=零触碰
type svcChange struct {
	Service string   `json:"service"`
	HostID  int64    `json:"hostId"`
	Host    string   `json:"host"`
	Action  string   `json:"action"`
	Reasons []string `json:"reasons,omitempty"`
}

// changed 是否需要纳入计划
func (c svcChange) changed() bool { return c.Action == "create" || c.Action == "update" || c.Action == "remove" }

// computeChanges 期望态（计划节点携带的 InvDetail）对 inventory 的逐服务 diff。
// 与执行计划同一来源（b.buildDag 产物的 InvDetail），预览/执行零口径差。
func (b *builder) computeChanges(nodes []planNode) []svcChange {
	invMap := map[string]invRow{}
	for _, r := range b.inv {
		if _, ok := invMap[r.Service]; !ok {
			invMap[r.Service] = r
		}
	}
	type planned struct {
		host   int64
		detail string
	}
	plannedSvcs := map[string]planned{}
	for _, n := range nodes {
		for svc, detail := range n.InvDetail {
			plannedSvcs[svc] = planned{host: n.Host, detail: detail}
		}
	}
	hostName := func(id int64) string {
		if h := b.hosts[id]; h != nil {
			return h.Name
		}
		return fmt.Sprintf("h%d", id)
	}
	var out []svcChange
	emit := func(svc string, hostID int64, action string, reasons ...string) {
		c := svcChange{Service: svc, HostID: hostID, Host: hostName(hostID), Action: action}
		if len(reasons) > 0 {
			c.Reasons = reasons
		}
		out = append(out, c)
	}
	for _, svc := range b.pp.Order {
		p, ok := plannedSvcs[svc]
		if !ok {
			continue
		}
		row, installed := invMap[svc]
		switch {
		case !installed:
			emit(svc, p.host, "create", "新增服务")
		case p.host != row.HostID:
			emit(svc, p.host, "update", fmt.Sprintf("迁移主机：%s → %s", hostName(row.HostID), hostName(p.host)))
		case row.Detail != p.detail:
			emit(svc, p.host, "update", detailDiff(row.Detail, p.detail)...)
		default:
			emit(svc, p.host, "nochange")
		}
	}
	// inventory 有、计划无：remove（旧主机 up --remove-orphans 收敛）
	var removed []string
	for svc, row := range invMap {
		if _, ok := plannedSvcs[svc]; !ok {
			removed = append(removed, svc)
			_ = row
		}
	}
	sort.Strings(removed)
	for _, svc := range removed {
		emit(svc, invMap[svc].HostID, "remove", "服务已不在编排中（清理容器）")
	}
	return out
}

// detailDiff 逐字段比对两个 detail JSON，产出人读变更原因
func detailDiff(oldRaw, newRaw string) []string {
	var oldM, newM map[string]any
	_ = json.Unmarshal([]byte(oldRaw), &oldM)
	_ = json.Unmarshal([]byte(newRaw), &newM)
	labels := map[string]string{
		"blockHash":   "编排块变更",
		"image":       "镜像变更",
		"envHash":     "变量取值变更",
		"confHash":    "配置内容变更",
		"resHash":     "资源限额变更",
		"unitHash":    "unit 定义变更",
		"artifactSha": "制品更新",
	}
	var reasons []string
	seen := map[string]bool{}
	for k, label := range labels {
		seen[k] = true
		if fmt.Sprint(oldM[k]) != fmt.Sprint(newM[k]) {
			reasons = append(reasons, label)
		}
	}
	var keys []string
	for k := range newM {
		if !seen[k] && k != "kind" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if fmt.Sprint(oldM[k]) != fmt.Sprint(newM[k]) {
			reasons = append(reasons, "定义变更（"+k+"）")
		}
	}
	if len(reasons) == 0 {
		reasons = []string{"定义变更"}
	}
	return reasons
}

// shellQuote POSIX 单引号安全包裹（docker exec sh -c '<script>'）
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
