// topology.go —— 内置拓扑模板（console 组态器，ADR-11）。
//
// 职责划分：studio 打包只出素材（镜像 tar + 组件声明 + 资源缺省）；**拓扑（几份实例、什么角色、
// 怎么互联）在 console 安装时组态**——内置向导把组件（如 mysql 单实例）展开成多实例（primary/
// replica），免为改拓扑回 studio 重打包。代价与决策：console 由此持有常用组件的拓扑知识
//（当前：MySQL 主从 GTID）；新拓扑形态在此扩展（数据驱动，引擎零改动——展开产物喂 format:4
// 角色链既有编译路径），不匹配「零组件知识」的原教旨约束，用户已拍板接受。
//
// 知识内容与 mysql-ha 预设同源：per-role my.cnf（GTID/server-id/read_only）、主库建复制账号 →
// 从库 CHANGE REPLICATION SOURCE（SOURCE_AUTO_POSITION=1，GTID 免 file:pos 步骤间传值）→
// exec 双线程就绪探针（Slave_IO/SQL_Running）、extras initdb 仅主库（从库不预建同名库防复制冲突）。
package main

import (
	"fmt"
	"sort"
	"strings"
)

// topoRole 拓扑角色模板
type topoRole struct {
	Name     string // 角色名（primary|replica）
	Title    string
	Port     string // 缺省宿主端口
	ServerID int    // MySQL server-id（0=不注入角色变量）
	InitDB   bool   // 挂 extras initdb + 预建库 env（仅主库/单机）
	Exec     bool   // exec 就绪探针（replica：SHOW REPLICA STATUS 双线程）；false=tcp 探测
	Join     bool   // 初始化链带 join 步骤（After=主库建号完成）
}

// topoForm 拓扑形态模板
type topoForm struct {
	ID          string
	Title       string
	Replicas    int // 从库数
	SpreadHosts bool
	Primary     topoRole
	Replica     topoRole
}

const (
	tplPrimaryCnf = "[mysqld]\nserver-id={{MYSQL_SERVER_ID}}\ngtid_mode=ON\nenforce_gtid_consistency=ON\nlog-bin=binlog\nbinlog_format=ROW\n"
	tplReplicaCnf = "[mysqld]\nserver-id={{MYSQL_SERVER_ID}}\ngtid_mode=ON\nenforce_gtid_consistency=ON\nread_only=ON\nrelay-log=relay-bin\n"

	tplReplUserScript = "mysql -uroot -p\"{{MYSQL_ROOT_PASSWORD}}\" -e \"CREATE USER IF NOT EXISTS '{{MYSQL_REPL_USER}}'@'%' IDENTIFIED WITH caching_sha2_password BY '{{MYSQL_REPL_PASSWORD}}'; GRANT REPLICATION REPLICA ON *.* TO '{{MYSQL_REPL_USER}}'@'%'; FLUSH PRIVILEGES;\""
	tplJoinScript     = "mysql -uroot -p\"{{MYSQL_ROOT_PASSWORD}}\" -e \"STOP REPLICA;\" || true\nmysql -uroot -p\"{{MYSQL_ROOT_PASSWORD}}\" -e \"CHANGE REPLICATION SOURCE TO SOURCE_HOST='{{DB_HOST}}', SOURCE_PORT={{DB_PORT}}, SOURCE_USER='{{MYSQL_REPL_USER}}', SOURCE_PASSWORD='{{MYSQL_REPL_PASSWORD}}', SOURCE_AUTO_POSITION=1, GET_SOURCE_PUBLIC_KEY=1;\"\nmysql -uroot -p\"{{MYSQL_ROOT_PASSWORD}}\" -e \"START REPLICA;\""
	tplReplicaProbe   = "mysql -uroot -p\"{{MYSQL_ROOT_PASSWORD}}\" -e 'SHOW REPLICA STATUS\\G' | grep -q 'Slave_IO_Running: Yes' && mysql -uroot -p\"{{MYSQL_ROOT_PASSWORD}}\" -e 'SHOW REPLICA STATUS\\G' | grep -q 'Slave_SQL_Running: Yes'"
)

// builtinTopologies 内置拓扑形态清单（MySQL 主从：1主1从 / 1主2从）
var builtinTopologies = []*topoForm{
	{ID: "mysql-ha-1", Title: "MySQL 主从（1主1从·GTID）", Replicas: 1, SpreadHosts: true,
		Primary: topoRole{Name: "primary", Title: "主库", Port: "3306", ServerID: 1, InitDB: true},
		Replica: topoRole{Name: "replica", Title: "从库", Port: "3306", ServerID: 1002, Exec: true, Join: true}},
	{ID: "mysql-ha-2", Title: "MySQL 主从（1主2从·GTID）", Replicas: 2, SpreadHosts: true,
		Primary: topoRole{Name: "primary", Title: "主库", Port: "3306", ServerID: 1, InitDB: true},
		Replica: topoRole{Name: "replica", Title: "从库", Port: "3306", ServerID: 1002, Exec: true, Join: true}},
}

func builtinForm(id string) (*topoForm, error) {
	for _, f := range builtinTopologies {
		if f.ID == id {
			return f, nil
		}
	}
	return nil, fmt.Errorf("未知拓扑形态 %q（内置：%s）", id, topoFormIDs())
}

func topoFormIDs() string {
	var ids []string
	for _, f := range builtinTopologies {
		ids = append(ids, f.ID)
	}
	return strings.Join(ids, "、")
}

// topologyFormsFor 包详情的组态形态清单（向导数据源）：组件匹配内置模板（当前：database 类
// 且镜像 mysql: 前缀）→ 返回可选形态与实例规格预览；不匹配返回 nil（组件卡无「组态」入口）。
func topologyFormsFor(c ComponentDecl) []map[string]any {
	isMysql := false
	for _, img := range c.Images {
		if strings.HasPrefix(img, "mysql:") {
			isMysql = true
			break
		}
	}
	if !isMysql {
		return nil
	}
	var out []map[string]any
	for _, f := range builtinTopologies {
		instances := []map[string]any{
			{"service": c.Code + "-primary", "role": f.Primary.Name, "roleTitle": f.Primary.Title, "port": f.Primary.Port, "serverId": f.Primary.ServerID},
		}
		for i := 0; i < f.Replicas; i++ {
			svc := fmt.Sprintf("%s-replica", c.Code)
			if f.Replicas > 1 {
				svc = fmt.Sprintf("%s-replica-%d", c.Code, i+1)
			}
			serverID := f.Replica.ServerID + i
			instances = append(instances, map[string]any{"service": svc, "role": f.Replica.Name, "roleTitle": f.Replica.Title, "port": f.Replica.Port, "serverId": serverID})
		}
		out = append(out, map[string]any{"id": f.ID, "title": f.Title, "minHosts": 1 + f.Replicas, "spreadHosts": f.SpreadHosts, "instances": instances})
	}
	return out
}

// mysqlInstanceBlock 实例服务块整块生成（不从包内原块做文本变换——模板即权威，引用组件镜像与
// 凭据 env；未列资源限额——统一由 injectResourceLimits 在组态/普通服务两侧同路径注入）
func mysqlInstanceBlock(image, svc, hostPort string, initDB bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "  %s:\n", svc)
	fmt.Fprintf(&sb, "    image: %s\n", image)
	sb.WriteString("    restart: unless-stopped\n")
	sb.WriteString("    environment:\n")
	sb.WriteString("      MYSQL_ROOT_PASSWORD: ${MYSQL_ROOT_PASSWORD:-mse@2026}\n")
	if initDB {
		sb.WriteString("      MYSQL_DATABASE: ${PLATFORM_DB_NAME:-mse_platform}\n")
	}
	sb.WriteString("      TZ: Asia/Shanghai\n")
	sb.WriteString("    volumes:\n")
	fmt.Fprintf(&sb, "      - ./data/%s:/var/lib/mysql\n", svc)
	fmt.Fprintf(&sb, "      - ./data/%s/conf.d:/etc/mysql/conf.d:ro\n", svc)
	if initDB {
		sb.WriteString("      - ./extras:/docker-entrypoint-initdb.d:ro\n")
	}
	sb.WriteString("    ports:\n")
	fmt.Fprintf(&sb, "      - \"%s:3306\"\n", hostPort)
	return sb.String()
}

// roleDeclFor 实例的 RoleDecl（format:4 角色链编译输入）：角色名在组件内唯一（多从库
// replica-1/-2——init 节点键/After 引用不撞）；files 按实例路径；server-id 角色变量；
// 探针/初始化链按模板（join 的 After=主库角色名）。
func roleDeclFor(c ComponentDecl, f *topoForm, svc, roleName string, serverID int, hostPort string, primaryRole string, initDB, exec, join bool) RoleDecl {
	rd := RoleDecl{Name: roleName, Services: []string{svc},
		Vars: map[string]string{"MYSQL_SERVER_ID": fmt.Sprint(serverID)}}
	rd.Files = []FileDecl{{Path: fmt.Sprintf("data/%s/conf.d/my.cnf", svc), Template: tplPrimaryCnf}}
	if !initDB {
		rd.Files[0].Template = tplReplicaCnf
	}
	if exec {
		rd.Readiness = &ProbeDecl{Type: "exec", Script: tplReplicaProbe}
	} else {
		rd.Readiness = &ProbeDecl{Type: "tcp", Port: hostPort}
	}
	if initDB {
		rd.Init = []InitDecl{{ID: "repl-user", Script: tplReplUserScript}}
	} else if join {
		rd.Init = []InitDecl{{ID: "join", After: primaryRole, Script: tplJoinScript}}
	}
	_ = c
	return rd
}

// applyTopology 组件实例化组态展开（BuildSessionPlan 在 resolveVars/validate 之前调用）：
// placement.instances 命中的组件按拓扑模板改写包视图——原服务替换为实例服务（合成 ServiceMeta
// 插入 Order 原位）、组件 Services/端点（→主库实例）/Roles/SpreadHosts 重写、补充模板凭据
// （复制账号，去重）——后续校验/物化/DAG 在改写后视图上**原样运行**（format:4 角色链全套机制
// 零新路径）。未组态组件零改动（format:3 老包完全兼容）。
func (b *builder) applyTopology() error {
	if len(b.pl.Instances) == 0 {
		return nil
	}
	pp := b.pp
	byComp := map[string][]InstanceSpec{}
	for _, ins := range b.pl.Instances {
		if ins.Service == "" || ins.Component == "" {
			return fmt.Errorf("实例声明缺少 component/service：%+v", ins)
		}
		byComp[ins.Component] = append(byComp[ins.Component], ins)
	}
	for _, insList := range byComp {
		form, err := builtinForm(insList[0].Form)
		if err != nil {
			return err
		}
		if len(insList) != 1+form.Replicas {
			return fmt.Errorf("拓扑形态 %s 需要 %d 个实例（1主%d从），实际 %d 个", form.ID, 1+form.Replicas, form.Replicas, len(insList))
		}
		for _, ins := range insList[1:] {
			if ins.Form != insList[0].Form {
				return fmt.Errorf("组件 %s 的实例组态形态不一致（%s 与 %s）", insList[0].Component, insList[0].Form, ins.Form)
			}
		}
	}
	var compCodes []string
	for code := range byComp {
		compCodes = append(compCodes, code)
	}
	sort.Strings(compCodes)
	for _, code := range compCodes {
		insList := byComp[code]
		var comp *ComponentDecl
		for i := range pp.Decl.Components {
			if pp.Decl.Components[i].Code == code {
				comp = &pp.Decl.Components[i]
				break
			}
		}
		if comp == nil {
			return fmt.Errorf("实例组态引用的组件 %s 不在包内", code)
		}
		if b.skipComp[code] {
			return fmt.Errorf("组件 %s 已实例化组态（%d 实例）不能同时跳过——请移除跳过或清空实例", code, len(insList))
		}
		if len(comp.Images) == 0 {
			return fmt.Errorf("组件 %s 无镜像声明，无法生成实例", code)
		}
		form, _ := builtinForm(insList[0].Form)
		// 实例服务名唯一性 + 角色齐备性（1 主 + N 从）
		seen := map[string]bool{}
		primaryCount := 0
		for _, ins := range insList {
			if seen[ins.Service] {
				return fmt.Errorf("实例服务名 %s 重复", ins.Service)
			}
			seen[ins.Service] = true
			if _, ok := pp.Meta[ins.Service]; ok {
				return fmt.Errorf("实例服务名 %s 与包内既有服务冲突", ins.Service)
			}
			if ins.Role == form.Primary.Name {
				primaryCount++
			}
		}
		if primaryCount != 1 {
			return fmt.Errorf("拓扑形态 %s 需要 1 个 %s 实例，实际 %d 个", form.ID, form.Primary.Name, primaryCount)
		}
		// 定位主库实例与原服务（端点重定向基准）
		origSvc := firstServiceOf(*comp)
		primarySvc := ""
		for _, ins := range insList {
			if ins.Role == form.Primary.Name {
				primarySvc = ins.Service
			}
		}
		// 展开实例：合成 ServiceMeta + RoleDecl（primary 在前——diffFilter「先从后主」以 Roles[0] 为主角色）
		newSvcs := make([]string, 0, len(insList))
		var roles []RoleDecl
		ordered := make([]InstanceSpec, 0, len(insList))
		for _, ins := range insList {
			if ins.Role == form.Primary.Name {
				ordered = append(ordered, ins)
			}
		}
		for _, ins := range insList {
			if ins.Role != form.Primary.Name {
				ordered = append(ordered, ins)
			}
		}
		replicaIdx := 0
		for _, ins := range ordered {
			role := form.Primary
			initDB, exec, join := role.InitDB, false, false
			serverID := form.Primary.ServerID
			roleName := form.Primary.Name // 角色名须实例内唯一（init 节点键/跨角色 After 引用）
			if ins.Role != form.Primary.Name {
				role = form.Replica
				initDB, exec, join = false, role.Exec, role.Join
				serverID = form.Replica.ServerID + replicaIdx
				roleName = form.Replica.Name
				if form.Replicas > 1 {
					roleName = fmt.Sprintf("%s-%d", form.Replica.Name, replicaIdx+1)
				}
				replicaIdx++
			}
			hostPort := orStr(ins.Port, role.Port)
			block := mysqlInstanceBlock(comp.Images[0], ins.Service, hostPort, initDB)
			pp.Meta[ins.Service] = &ServiceMeta{Name: ins.Service, Owner: "comp:" + comp.Code, Kind: "comp",
				Image: comp.Images[0], Ports: map[string]string{"3306": hostPort}, Block: block,
				Primary: ins.Role == form.Primary.Name}
			newSvcs = append(newSvcs, ins.Service)
			roles = append(roles, roleDeclFor(*comp, form, ins.Service, roleName, serverID, hostPort, form.Primary.Name, initDB, exec, join))
		}
		// 组件视图改写：Services/Roles/SpreadHosts/端点→主库实例/模板凭据补充
		comp.Services = newSvcs
		comp.Roles = roles
		if form.SpreadHosts {
			comp.SpreadHosts = true
		}
		for j := range comp.Endpoints {
			if comp.Endpoints[j].Service == origSvc {
				comp.Endpoints[j].Service = primarySvc
			}
		}
		hasCred := func(ph string) bool {
			for _, cred := range comp.Credentials {
				if cred.Placeholder == ph {
					return true
				}
			}
			return false
		}
		// 模板凭据兜底：root 口令（缺省 random——实例为全新数据目录，首启设号）+ 复制账号两件。
		// 兼容未按 mse 预设形态声明凭据的 mysql 组件（模板块/脚本统一引 MYSQL_ROOT_PASSWORD）
		for _, cred := range []Credential{
			{Placeholder: "MYSQL_ROOT_PASSWORD", Label: "MySQL root 密码", Env: "MYSQL_ROOT_PASSWORD", Generate: "random"},
			{Placeholder: "MYSQL_REPL_USER", Label: "MySQL 复制账号", Env: "MYSQL_REPL_USER", Generate: "const", Value: "repl"},
			{Placeholder: "MYSQL_REPL_PASSWORD", Label: "MySQL 复制账号密码", Env: "MYSQL_REPL_PASSWORD", Generate: "random"},
		} {
			if !hasCred(cred.Placeholder) {
				comp.Credentials = append(comp.Credentials, cred)
			}
		}
		// 单机→组态迁移边界：inventory 已有原服务记录 = 在位单实例被替换——实例数据目录全新
		//（./data/<实例名>），原单实例数据不自动迁移（需自行 mysqldump 导入）；增量升级时一并明示
		for _, row := range b.inv {
			if row.Service == origSvc {
				b.warnf("组件 %s 由单实例改为组态 %s：实例数据目录全新（./data/<实例名>），原单实例（主机 h%d）数据不自动迁移——需导出导入；原容器请经卸载/清理移除，端口冲突将被预检拦截",
					code, form.ID, row.HostID)
				break
			}
		}
		// 原服务出列（Order 原位替换为实例序列；Meta/放置清理——组态后原服务不再单独存在）
		var newOrder []string
		for _, svc := range pp.Order {
			if svc == origSvc {
				newOrder = append(newOrder, newSvcs...)
				continue
			}
			newOrder = append(newOrder, svc)
		}
		pp.Order = newOrder
		delete(pp.Meta, origSvc)
		delete(b.pl.Host, origSvc)
		delete(b.pl.Mode, origSvc)
		// 其他服务对原服务的 depends_on 重写为主库实例（同机就绪分层/跨机 wait 门控时序保持——
		// 不重写则依赖悬空被剥净，app 启动不再等数据库）
		for _, svc := range pp.Order {
			if m := pp.Meta[svc]; m != nil {
				for i, dep := range m.Deps {
					if dep == origSvc {
						m.Deps[i] = primarySvc
					}
				}
			}
		}
	}
	return nil
}
