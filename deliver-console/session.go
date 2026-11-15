// 会话执行器：DAG 拓扑调度（依赖全 ok/skipped 才放行）→ 分发（本机节点进程内执行 / agent
// 长轮询队列）→ 看门狗超时兜底 → retry/skip/gate 人工干预 → 成功回写 inventory（增量基线）。
// console 重启恢复：dispatched/running 节点重置为 pending 重新调度（步骤幂等）。
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// planSnapshot 会话计划快照（随会话落库，重启恢复用）
type planSnapshot struct {
	Nodes    []planNode `json:"nodes"`
	EntryURL string     `json:"entryUrl,omitempty"`
	Warnings []string   `json:"warnings,omitempty"`
	Mode     string     `json:"mode"`
}

type sessionRun struct {
	id     int64
	app    string
	srv    *Server
	pkgDir string

	mu      sync.Mutex
	nodes   []planNode
	byKey   map[string]*planNode
	status  map[string]string // pending|dispatched|running|ok|failed|skipped
	since   map[string]time.Time
	snap    *planSnapshot
	wake    chan struct{}
	closed  bool
	aborted   bool             // 用户中止（finalize 据此落 aborted 终态）
	invApplied map[string]bool // 节点 inventory 已回写标记（防重试重复 upsert）
}

// StartSession 创建并启动会话调度（节点行已入库）
func (s *Server) StartSession(id int64, app string, pkgDir string, snap *planSnapshot) {
	run := &sessionRun{id: id, app: app, srv: s, pkgDir: pkgDir,
		byKey: map[string]*planNode{}, status: map[string]string{}, since: map[string]time.Time{},
		invApplied: map[string]bool{}, snap: snap, wake: make(chan struct{}, 1)}
	for i := range snap.Nodes {
		n := snap.Nodes[i]
		run.nodes = append(run.nodes, n)
		run.byKey[n.Key] = &run.nodes[i]
		run.status[n.Key] = "pending"
	}
	s.mu.Lock()
	s.sess[id] = run
	s.mu.Unlock()
	go run.loop()
}

// ResumeSession 启动时恢复未终态会话：dispatched/running → pending 重调度
func (s *Server) ResumeSession(id int64, app string, pkgDir string, snap *planSnapshot, statuses map[string]string) {
	run := &sessionRun{id: id, app: app, srv: s, pkgDir: pkgDir,
		byKey: map[string]*planNode{}, status: map[string]string{}, since: map[string]time.Time{},
		invApplied: map[string]bool{}, snap: snap, wake: make(chan struct{}, 1)}
	for i := range snap.Nodes {
		n := snap.Nodes[i]
		run.nodes = append(run.nodes, n)
		run.byKey[n.Key] = &run.nodes[i]
		st := statuses[n.Key]
		if st == "" {
			st = "pending"
		}
		if st == "dispatched" || st == "running" {
			st = "pending"
			_ = s.store.UpdateNodeStatus(id, n.Key, "pending", "console 重启，重新调度")
		}
		run.status[n.Key] = st
		if st == "ok" || st == "skipped" {
			// 崩溃窗口对账：状态已 ok 但 inventory 行可能未落（console 在状态更新与回写之间退出）——
			// 恢复时幂等补写
			run.invApplied[n.Key] = true
			run.applyInvNode(&run.nodes[i])
		}
	}
	s.mu.Lock()
	s.sess[id] = run
	s.mu.Unlock()
	go run.loop()
}

func (s *Server) session(id int64) *sessionRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess[id]
}

func (r *sessionRun) nodeStatus(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status[key]
}

func (r *sessionRun) setLocalStatus(key, status, message string) {
	r.mu.Lock()
	if r.status[key] != status {
		r.status[key] = status
		r.since[key] = time.Now()
	}
	n := r.byKey[key]
	applyInv := false
	if (status == "ok" || status == "skipped") && n != nil && !r.invApplied[key] {
		r.invApplied[key] = true
		applyInv = true
	}
	r.mu.Unlock()
	_ = r.srv.store.UpdateNodeStatus(r.id, key, status, message)
	if status == "running" {
		_ = r.srv.store.AppendLogs(r.id, key, nil)
	}
	// 逐节点即时回写（K8s 资源状态对应物）：节点成功即 upsert 其 inventory 行——部分失败会话已成功
	// 服务不再丢基线（旧口径仅会话整体 done 才回写，一次失败下次全量重算）；驱逐节点成功即删旧行
	if applyInv {
		r.applyInvNode(n)
	}
}

// applyInvNode 单节点 inventory 回写/驱逐删行
func (r *sessionRun) applyInvNode(n *planNode) {
	var rows []invRow
	for _, svc := range n.InvCompose {
		rows = append(rows, invRow{App: r.app, HostID: n.Host, Service: svc, Kind: "compose", Detail: n.InvDetail[svc]})
	}
	if n.InvSystemd != "" {
		rows = append(rows, invRow{App: r.app, HostID: n.Host, Service: n.InvSystemd, Kind: "systemd", Detail: n.InvDetail[n.InvSystemd]})
	}
	if len(rows) > 0 {
		_ = r.srv.store.UpsertInventory(rows)
	}
	if n.InvEvictHost != 0 && len(n.InvEvictSvcs) > 0 {
		for _, svc := range n.InvEvictSvcs {
			_ = r.srv.store.DeleteInventoryRow(r.app, n.InvEvictHost, svc)
		}
	}
}

// Report agent/本机节点上报入口（状态 + 日志）。running 心跳与执行中日志的最后一行同步进
// 节点 message（UI 消息列实时反馈下载进度/等待状态）；running→running 不再刷 started_at
// （看门狗计时与 UI 时长以首次运行为准）
func (r *sessionRun) Report(key, status, message string, logs []string) {
	_ = r.srv.store.AppendLogs(r.id, key, logs)
	tail := ""
	if len(logs) > 0 {
		tail = clipMsg("⟳ " + logs[len(logs)-1])
	}
	r.mu.Lock()
	_, known := r.status[key]
	cur := r.status[key]
	r.mu.Unlock()
	if !known {
		return
	}
	if status == "" || status == "running" {
		switch cur {
		case "dispatched": // agent 首次领取任务：转 running，started_at=此刻
			msg := message
			if msg == "" {
				msg = tail
			}
			r.setLocalStatus(key, "running", msg)
			return
		case "running": // 执行中进度：仅回显 message，不动状态与计时
			msg := clipMsg(message)
			if msg == "" {
				msg = tail
			}
			if msg != "" {
				_ = r.srv.store.UpdateNodeStatus(r.id, key, "running", msg)
			}
			return
		}
		return
	}
	r.setLocalStatus(key, status, message)
	if status == "ok" || status == "failed" {
		r.notify()
	}
}

// clipMsg 节点消息截断（日志尾行可能很长）
func clipMsg(s string) string {
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}

func (r *sessionRun) notify() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// satisfied 依赖满足判定：ok / skipped（skip=强制放行）均算满足
func (r *sessionRun) satisfied(deps []string) bool {
	for _, d := range deps {
		st := r.nodeStatus(d)
		if st != "ok" && st != "skipped" {
			return false
		}
	}
	return true
}

func (r *sessionRun) loop() {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		r.watchdogTick()
		r.schedule()
		if r.finalize() {
			return
		}
		select {
		case <-r.wake:
		case <-tick.C:
		}
	}
}

// schedule 挑选可运行节点并分发；gate 节点不自动分发（等人工确认）。
// 注意锁内只读快照——satisfied 判定在锁外做（nodeStatus 也要拿同一把锁，不可重入）
func (r *sessionRun) schedule() {
	r.mu.Lock()
	status := make(map[string]string, len(r.status))
	for k, v := range r.status {
		status[k] = v
	}
	var nodes []planNode
	nodes = append(nodes, r.nodes...)
	r.mu.Unlock()
	okStatus := func(st string) bool { return st == "ok" || st == "skipped" }
	var runnable []string
	for _, n := range nodes {
		if status[n.Key] != "pending" || n.Kind == "gate" {
			continue
		}
		ready := true
		for _, d := range n.Deps {
			if !okStatus(status[d]) {
				ready = false
				break
			}
		}
		if ready {
			runnable = append(runnable, n.Key)
		}
	}
	if len(runnable) == 0 {
		return
	}
	r.mu.Lock()
	for _, key := range runnable {
		if r.status[key] == "pending" {
			r.status[key] = "dispatched"
			r.since[key] = time.Now()
		}
	}
	r.mu.Unlock()
	for _, key := range runnable {
		if r.status[key] != "dispatched" && status[key] != "pending" {
			continue
		}
		_ = r.srv.store.UpdateNodeStatus(r.id, key, "dispatched", "")
		r.dispatch(key)
	}
}

// dispatch 分发到本机执行器或 agent 队列
func (r *sessionRun) dispatch(key string) {
	r.mu.Lock()
	n := *r.byKey[key]
	r.mu.Unlock()
	if n.Kind == "gate" {
		return
	}
	host, err := r.srv.store.HostByID(n.Host)
	if err != nil {
		r.setLocalStatus(key, "failed", fmt.Sprintf("主机 #%d 不存在", n.Host))
		r.notify()
		return
	}
	if host.Local {
		go r.srv.localExec(r, host, &n)
		return
	}
	task := &Task{SessionID: fmt.Sprint(r.id), NodeID: key, Kind: n.Kind, Payload: n.Payload}
	if err := r.srv.enqueue(host.ID, task); err != nil {
		// 队列满：回 pending 下轮再试
		r.setLocalStatus(key, "pending", "")
		return
	}
	if !host.OnlineCheck() {
		// 离线主机：照常入队（agent 上线即领），看门狗兜底
		_ = r.srv.store.UpdateNodeStatus(r.id, key, "dispatched", "主机离线，任务排队等待 agent 上线")
	}
}

// finalize 终态判定：全部终态 → done/failed；仅剩 gate 等待或被阻断 pending → 继续等
func (r *sessionRun) finalize() bool {
	r.mu.Lock()
	pendingGates, inFlight, failed := 0, 0, 0
	for _, n := range r.nodes {
		st := r.status[n.Key]
		switch st {
		case "failed":
			failed++
		case "pending":
			if n.Kind == "gate" {
				pendingGates++
			}
			inFlight++
		case "dispatched", "running":
			inFlight++
		}
	}
	r.mu.Unlock()
	if inFlight > pendingGates { // 存在非 gate 的在途/待放行节点（含被失败阻断的，等人工 retry/skip）
		return false
	}
	if inFlight == 0 { // 全部终态
		status, summary := "done", "安装完成"
		if r.snap.EntryURL != "" {
			summary = "安装完成：入口 " + r.snap.EntryURL
		}
		r.mu.Lock()
		aborted := r.aborted
		r.mu.Unlock()
		if aborted {
			status, summary = "aborted", "安装已中止（用户发起；已起容器保留在主机上，可重新发起安装幂等收敛）"
		} else if failed > 0 {
			status = "failed"
			summary = fmt.Sprintf("安装失败：%d 个节点失败（可修复后单节点重试）", failed)
		}
		_ = r.srv.store.UpdateSessionStatus(r.id, status, summary)
		// inventory 已逐节点即时回写（setLocalStatus），此处无需整体回写——部分失败会话的已成功
		// 服务基线同样在位，下次增量只重算失败部分
		r.srv.mu.Lock()
		delete(r.srv.sess, r.id)
		r.srv.mu.Unlock()
		return true
	}
	return false // 只剩 gate 等待人工确认
}

// watchdogTick 看门狗：dispatched 长期未被领取（agent 离线/中断）与 running 执行超时
// （节点 timeout + 3min 缓冲）分别判失败，避免节点无限悬挂
func (r *sessionRun) watchdogTick() {
	r.mu.Lock()
	type expired struct {
		key, msg string
	}
	var out []expired
	for _, n := range r.nodes {
		st := r.status[n.Key]
		if st != "dispatched" && st != "running" {
			continue
		}
		since, ok := r.since[n.Key]
		if !ok {
			continue
		}
		if st == "dispatched" {
			if time.Since(since) > 10*time.Minute {
				out = append(out, expired{n.Key, "任务长期未被 agent 领取（>10 分钟）——agent 离线或执行中断，可重试"})
			}
			continue
		}
		limit := time.Duration(n.TimeoutSec)*time.Second + 3*time.Minute
		if n.TimeoutSec == 0 {
			limit = 18 * time.Minute // 无声明超时节点（cmd 等）的保守上限
		}
		if time.Since(since) > limit {
			out = append(out, expired{n.Key, fmt.Sprintf("执行超时（>%s）——agent 失联或任务卡死，可重试", limit)})
		}
	}
	r.mu.Unlock()
	for _, e := range out {
		r.setLocalStatus(e.key, "failed", e.msg)
		r.notify()
	}
}

// Retry / Skip / Gate 人工干预
func (r *sessionRun) Retry(key string) error {
	r.mu.Lock()
	st := r.status[key]
	_, known := r.byKey[key]
	r.mu.Unlock()
	if !known {
		return errNotFound
	}
	if st == "dispatched" || st == "running" {
		return fmt.Errorf("节点执行中，不能重试（等待看门狗超时或完成）")
	}
	r.mu.Lock()
	delete(r.invApplied, key) // 重试再成功时重新回写（upsert 幂等）
	r.mu.Unlock()
	r.setLocalStatus(key, "pending", "")
	_ = r.srv.store.UpdateSessionStatus(r.id, "running", "")
	r.notify()
	return nil
}

func (r *sessionRun) Skip(key string) error {
	r.mu.Lock()
	_, known := r.byKey[key]
	r.mu.Unlock()
	if !known {
		return errNotFound
	}
	r.setLocalStatus(key, "skipped", "人工跳过（依赖方按已满足放行）")
	r.notify()
	return nil
}

func (r *sessionRun) Gate(key string) error {
	r.mu.Lock()
	n, known := r.byKey[key]
	r.mu.Unlock()
	if !known || n.Kind != "gate" {
		return fmt.Errorf("节点 %s 不是 gate 节点", key)
	}
	r.setLocalStatus(key, "ok", "人工确认通过")
	r.notify()
	return nil
}

// Abort 用户中止安装：未完成节点（pending/dispatched/running，含等待中的 gate）全部置 failed，
// 终态落库与执行器退出由 finalize 统一承担（状态=aborted，不回写 inventory）。在途 agent 任务
// 经 report 响应 cancel 中断（runCmdIO 杀进程组）；队列中未领取任务由 popTask 出队时丢弃。
func (r *sessionRun) Abort() error {
	r.mu.Lock()
	if r.aborted {
		r.mu.Unlock()
		return nil // 幂等：重复中止直接成功
	}
	r.aborted = true
	var toFail []string
	for _, n := range r.nodes {
		switch r.status[n.Key] {
		case "pending", "dispatched", "running":
			toFail = append(toFail, n.Key)
		}
	}
	r.mu.Unlock()
	for _, key := range toFail {
		r.setLocalStatus(key, "failed", "已中止：用户发起")
	}
	r.notify()
	return nil
}

// ---- agent 任务队列 ----

func (s *Server) enqueue(hostID int64, task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.queues[hostID]
	if !ok {
		q = make(chan *Task, 16)
		s.queues[hostID] = q
	}
	select {
	case q <- task:
		return nil
	default:
		return fmt.Errorf("主机 %d 任务队列已满", hostID)
	}
}

// popTask 长轮询取任务（hold 最多 wait 秒；无任务返回 nil）。已中止会话的滞留任务出队即丢
// （节点状态已被 Abort 置终态，执行只会白跑——大镜像 load 尤其不该再下发）
func (s *Server) popTask(hostID int64, wait time.Duration) *Task {
	s.mu.Lock()
	q, ok := s.queues[hostID]
	if !ok {
		q = make(chan *Task, 16)
		s.queues[hostID] = q
	}
	s.mu.Unlock()
	deadline := time.After(wait)
	for {
		select {
		case t := <-q:
			if sid, err := strconv.ParseInt(t.SessionID, 10, 64); err == nil && s.sessionAborted(sid) {
				continue // 已中止会话的任务：丢弃，继续等下一个
			}
			return t
		case <-deadline:
			return nil
		}
	}
}

// sessionAborted 会话是否处于已中止终态（以 store 为准——执行器终结删除后，迟到的 report
// 仍可据此回 cancel 中断在途任务）
func (s *Server) sessionAborted(sid int64) bool {
	ss, err := s.store.SessionByID(sid)
	return err == nil && ss.Status == "aborted"
}

// LoadPlanSnapshot 从会话行解析计划快照
func LoadPlanSnapshot(raw string) *planSnapshot {
	var snap planSnapshot
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &snap)
	}
	if snap.Nodes == nil {
		snap.Nodes = []planNode{}
	}
	return &snap
}
