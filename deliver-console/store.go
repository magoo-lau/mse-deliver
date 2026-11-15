// deliver-console 存储层：SQLite（modernc.org/sqlite 纯 Go，与 deliver-studio 同款依赖口径）。
// 建表幂等；JSON 类字段以文本落库；时间为 unix 秒。
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var errNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

func OpenStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "console.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者：串行化写
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { _ = s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS settings(k TEXT PRIMARY KEY, v TEXT DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS hosts(
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL,
			agent_token TEXT UNIQUE NOT NULL, local INTEGER NOT NULL DEFAULT 0,
			addr TEXT DEFAULT '', labels TEXT DEFAULT '', info TEXT DEFAULT '{}',
			revoked INTEGER NOT NULL DEFAULT 0, last_seen INTEGER DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS packages(
			id INTEGER PRIMARY KEY AUTOINCREMENT, app TEXT NOT NULL, version TEXT NOT NULL,
			dir TEXT NOT NULL, decl TEXT NOT NULL, files TEXT DEFAULT '{}', pkg_sha TEXT DEFAULT '',
			created_at INTEGER NOT NULL,
			UNIQUE(app, version))`,
		`CREATE TABLE IF NOT EXISTS schemes(
			id INTEGER PRIMARY KEY AUTOINCREMENT, app TEXT NOT NULL, name TEXT NOT NULL,
			placement TEXT DEFAULT '{}', dag TEXT DEFAULT '[]',
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(app, name))`,
		`CREATE TABLE IF NOT EXISTS sessions(
			id INTEGER PRIMARY KEY AUTOINCREMENT, app TEXT NOT NULL, version TEXT NOT NULL,
			package_id INTEGER NOT NULL, mode TEXT NOT NULL, status TEXT NOT NULL,
			vars TEXT DEFAULT '{}', plan TEXT DEFAULT '[]', summary TEXT DEFAULT '',
			placement TEXT DEFAULT '', pkg_sha TEXT DEFAULT '',
			created_at INTEGER NOT NULL, finished_at INTEGER DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS session_nodes(
			id INTEGER PRIMARY KEY AUTOINCREMENT, session_id INTEGER NOT NULL,
			node_key TEXT NOT NULL, host_id INTEGER NOT NULL DEFAULT 0, kind TEXT NOT NULL,
			title TEXT DEFAULT '', status TEXT NOT NULL DEFAULT 'pending', message TEXT DEFAULT '',
			started_at INTEGER DEFAULT 0, finished_at INTEGER DEFAULT 0,
			UNIQUE(session_id, node_key))`,
		`CREATE TABLE IF NOT EXISTS node_logs(
			id INTEGER PRIMARY KEY AUTOINCREMENT, session_id INTEGER NOT NULL,
			node_key TEXT NOT NULL, seq INTEGER NOT NULL, line TEXT NOT NULL,
			UNIQUE(session_id, node_key, seq))`,
		`CREATE TABLE IF NOT EXISTS inventory(
			app TEXT NOT NULL, host_id INTEGER NOT NULL, service TEXT NOT NULL,
			kind TEXT NOT NULL, detail TEXT DEFAULT '{}', updated_at INTEGER NOT NULL,
			PRIMARY KEY(app, host_id, service))`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	// 存量库补列（新库建表已含；重复列静默=幂等）：packages.pkg_sha / sessions.placement+pkg_sha
	for _, a := range []struct{ table, ddl string }{
		{"packages", `ALTER TABLE packages ADD COLUMN pkg_sha TEXT DEFAULT ''`},
		{"sessions", `ALTER TABLE sessions ADD COLUMN placement TEXT DEFAULT ''`},
		{"sessions", `ALTER TABLE sessions ADD COLUMN pkg_sha TEXT DEFAULT ''`},
	} {
		if _, err := s.db.Exec(a.ddl); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// ---- settings ----

func (s *Store) GetSetting(k, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT v FROM settings WHERE k=?`, k).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) PutSetting(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO settings(k,v) VALUES(?,?)
		ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (s *Store) DelSetting(k string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE k=?`, k)
	return err
}

// ---- hosts ----

type hostRow struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	AgentToken string   `json:"-"`
	Local      bool     `json:"local"`
	Addr       string   `json:"addr"`
	Labels     string   `json:"labels"`
	Info       HostInfo `json:"info"`
	Revoked    bool     `json:"revoked,omitempty"`
	LastSeen   int64    `json:"lastSeen"`
	CreatedAt  int64    `json:"createdAt"`
	Online     bool     `json:"online"`
}

func (s *Store) UpsertHost(h *hostRow) error {
	h.CreatedAt = time.Now().Unix()
	info, _ := json.Marshal(h.Info)
	res, err := s.db.Exec(`INSERT INTO hosts(name, agent_token, local, addr, labels, info, created_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET agent_token=excluded.agent_token, local=excluded.local,
			addr=excluded.addr, labels=excluded.labels, info=excluded.info, revoked=0`,
		h.Name, h.AgentToken, boolInt(h.Local), h.Addr, h.Labels, string(info), h.CreatedAt)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		h.ID = id
		return nil
	}
	return s.db.QueryRow(`SELECT id FROM hosts WHERE name=?`, h.Name).Scan(&h.ID)
}

func scanHost(row interface{ Scan(...any) error }) (*hostRow, error) {
	h := &hostRow{}
	var local, revoked int
	var info string
	if err := row.Scan(&h.ID, &h.Name, &h.AgentToken, &local, &h.Addr, &h.Labels, &info, &revoked, &h.LastSeen, &h.CreatedAt); err != nil {
		return nil, err
	}
	h.Local, h.Revoked = local == 1, revoked == 1
	_ = json.Unmarshal([]byte(info), &h.Info)
	h.Online = h.OnlineCheck()
	return h, nil
}

func (h *hostRow) OnlineCheck() bool {
	if h.Local {
		return true
	}
	return time.Now().Unix()-h.LastSeen < 90 // 长轮询周期 25s + 心跳 120s，90s 内有轮询即在线
}

const hostCols = `id, name, agent_token, local, addr, labels, info, revoked, last_seen, created_at`

func (s *Store) ListHosts() ([]*hostRow, error) {
	rows, err := s.db.Query(`SELECT ` + hostCols + ` FROM hosts ORDER BY local DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*hostRow
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) HostByID(id int64) (*hostRow, error) {
	h, err := scanHost(s.db.QueryRow(`SELECT `+hostCols+` FROM hosts WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return h, err
}

func (s *Store) HostByToken(token string) (*hostRow, error) {
	if token == "" {
		return nil, errNotFound
	}
	h, err := scanHost(s.db.QueryRow(`SELECT `+hostCols+` FROM hosts WHERE agent_token=?`, token))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return h, err
}

func (s *Store) HostByName(name string) (*hostRow, error) {
	h, err := scanHost(s.db.QueryRow(`SELECT `+hostCols+` FROM hosts WHERE name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return h, err
}

func (s *Store) UpdateHost(id int64, addr, labels string) error {
	_, err := s.db.Exec(`UPDATE hosts SET addr=?, labels=? WHERE id=?`, addr, labels, id)
	return err
}

func (s *Store) TouchHost(id int64, info *HostInfo) error {
	if info != nil {
		b, _ := json.Marshal(info)
		_, err := s.db.Exec(`UPDATE hosts SET last_seen=?, info=? WHERE id=?`, time.Now().Unix(), string(b), id)
		return err
	}
	_, err := s.db.Exec(`UPDATE hosts SET last_seen=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

func (s *Store) RevokeHost(id int64) error {
	_, err := s.db.Exec(`UPDATE hosts SET revoked=1 WHERE id=?`, id)
	return err
}

func (s *Store) DeleteHost(id int64) error {
	_, err := s.db.Exec(`DELETE FROM hosts WHERE id=?`, id)
	return err
}

// ---- packages ----

type pkgFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type packageRow struct {
	ID        int64     `json:"id"`
	App       string    `json:"app"`
	Version   string    `json:"version"`
	PkgSha    string    `json:"pkgSha"` // 包指纹=sha256(manifest.json)——审计溯源（会话快照防包删除后失联）
	Dir       string    `json:"-"`
	Decl      string    `json:"-"`
	Files     []pkgFile `json:"files"`
	CreatedAt int64     `json:"createdAt"`
}

// UpsertPackage 同 app+version 重复上传=覆盖更新（对齐介质库「同槽位同版本覆盖」口径——解包层本就
// 无条件覆盖目录，DB 元数据/指纹随新包一并刷新）；id 保持不变，历史会话 package_id 引用不断链
func (s *Store) UpsertPackage(p *packageRow) error {
	files, _ := json.Marshal(p.Files)
	now := time.Now().Unix()
	if _, err := s.db.Exec(`INSERT INTO packages(app, version, dir, decl, files, pkg_sha, created_at)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(app, version) DO UPDATE SET
			dir=excluded.dir, decl=excluded.decl, files=excluded.files,
			pkg_sha=excluded.pkg_sha, created_at=excluded.created_at`,
		p.App, p.Version, p.Dir, p.Decl, string(files), p.PkgSha, now); err != nil {
		return err
	}
	// DO UPDATE 分支 LastInsertId 不可靠，按唯一键回查 id
	if err := s.db.QueryRow(`SELECT id FROM packages WHERE app=? AND version=?`, p.App, p.Version).
		Scan(&p.ID); err != nil {
		return err
	}
	p.CreatedAt = now
	return nil
}

func (s *Store) PackageByAppVersion(app, version string) (*packageRow, error) {
	p := &packageRow{}
	var decl, files string
	err := s.db.QueryRow(`SELECT id, app, version, dir, decl, files, pkg_sha, created_at FROM packages WHERE app=? AND version=?`, app, version).
		Scan(&p.ID, &p.App, &p.Version, &p.Dir, &decl, &files, &p.PkgSha, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Decl = decl
	_ = json.Unmarshal([]byte(files), &p.Files)
	return p, nil
}

func (s *Store) ListPackages() ([]*packageRow, error) {
	rows, err := s.db.Query(`SELECT id, app, version, dir, decl, files, pkg_sha, created_at FROM packages ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*packageRow
	for rows.Next() {
		p := &packageRow{}
		var decl, files string
		if err := rows.Scan(&p.ID, &p.App, &p.Version, &p.Dir, &decl, &files, &p.PkgSha, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.Decl, p.Files = decl, nil
		_ = json.Unmarshal([]byte(files), &p.Files)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) PackageByID(id int64) (*packageRow, error) {
	p := &packageRow{}
	var decl, files string
	err := s.db.QueryRow(`SELECT id, app, version, dir, decl, files, pkg_sha, created_at FROM packages WHERE id=?`, id).
		Scan(&p.ID, &p.App, &p.Version, &p.Dir, &decl, &files, &p.PkgSha, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	p.Decl = decl
	_ = json.Unmarshal([]byte(files), &p.Files)
	return p, nil
}

func (s *Store) DeletePackage(id int64) error {
	_, err := s.db.Exec(`DELETE FROM packages WHERE id=?`, id)
	return err
}

// ---- schemes（安装方案：分配 + 自定义 DAG） ----

type schemeRow struct {
	ID        int64  `json:"id"`
	App       string `json:"app"`
	Name      string `json:"name"`
	Placement string `json:"placement"`
	Dag       string `json:"dag"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

func (s *Store) UpsertScheme(sc *schemeRow) error {
	now := time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO schemes(app, name, placement, dag, created_at, updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(app, name) DO UPDATE SET placement=excluded.placement, dag=excluded.dag, updated_at=excluded.updated_at`,
		sc.App, sc.Name, sc.Placement, sc.Dag, now, now)
	if err != nil {
		return err
	}
	if id, err := res.LastInsertId(); err == nil && id > 0 {
		sc.ID = id
	} else {
		_ = s.db.QueryRow(`SELECT id FROM schemes WHERE app=? AND name=?`, sc.App, sc.Name).Scan(&sc.ID)
	}
	sc.UpdatedAt = now
	return nil
}

func (s *Store) ListSchemes(app string) ([]*schemeRow, error) {
	q, args := `SELECT id, app, name, placement, dag, created_at, updated_at FROM schemes`, []any{}
	if app != "" {
		q += ` WHERE app=?`
		args = append(args, app)
	}
	rows, err := s.db.Query(q+` ORDER BY id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*schemeRow
	for rows.Next() {
		sc := &schemeRow{}
		if err := rows.Scan(&sc.ID, &sc.App, &sc.Name, &sc.Placement, &sc.Dag, &sc.CreatedAt, &sc.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, rows.Err()
}

func (s *Store) DeleteScheme(id int64) error {
	_, err := s.db.Exec(`DELETE FROM schemes WHERE id=?`, id)
	return err
}

// ---- sessions / nodes / logs ----

type sessionRow struct {
	ID         int64  `json:"id"`
	App        string `json:"app"`
	Version    string `json:"version"`
	PackageID  int64  `json:"packageId"`
	PkgSha     string `json:"pkgSha"` // 包指纹快照（防包删除后溯源失联）
	Mode       string `json:"mode"`   // fresh|incremental
	Status     string `json:"status"`
	Vars       string `json:"-"`
	Plan       string `json:"-"`
	Placement  string `json:"placement"` // 分配方案快照（JSON 文本；介质回退=用它重放增量安装）
	Summary    string `json:"summary"`
	CreatedAt  int64  `json:"createdAt"`
	FinishedAt int64  `json:"finishedAt"`
}

func (s *Store) InsertSession(ss *sessionRow, nodes []planNode) error {
	ss.CreatedAt = time.Now().Unix()
	res, err := s.db.Exec(`INSERT INTO sessions(app, version, package_id, mode, status, vars, plan, placement, pkg_sha, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, ss.App, ss.Version, ss.PackageID, ss.Mode, "running", ss.Vars, ss.Plan, ss.Placement, ss.PkgSha, ss.CreatedAt)
	if err != nil {
		return err
	}
	ss.ID, _ = res.LastInsertId()
	for i := range nodes {
		n := nodes[i]
		if _, err := s.db.Exec(`INSERT OR REPLACE INTO session_nodes(session_id, node_key, host_id, kind, title, status)
			VALUES(?,?,?,?,?, 'pending')`, ss.ID, n.Key, n.Host, n.Kind, n.Title); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListSessions(app string) ([]*sessionRow, error) {
	q, args := `SELECT id, app, version, package_id, mode, status, vars, plan, summary, placement, pkg_sha, created_at, finished_at FROM sessions`, []any{}
	if app != "" {
		q += ` WHERE app=?`
		args = append(args, app)
	}
	rows, err := s.db.Query(q+` ORDER BY id DESC LIMIT 100`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*sessionRow
	for rows.Next() {
		ss := &sessionRow{}
		if err := rows.Scan(&ss.ID, &ss.App, &ss.Version, &ss.PackageID, &ss.Mode, &ss.Status, &ss.Vars, &ss.Plan, &ss.Summary, &ss.Placement, &ss.PkgSha, &ss.CreatedAt, &ss.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

func (s *Store) SessionByID(id int64) (*sessionRow, error) {
	ss := &sessionRow{}
	err := s.db.QueryRow(`SELECT id, app, version, package_id, mode, status, vars, plan, summary, placement, pkg_sha, created_at, finished_at
		FROM sessions WHERE id=?`, id).
		Scan(&ss.ID, &ss.App, &ss.Version, &ss.PackageID, &ss.Mode, &ss.Status, &ss.Vars, &ss.Plan, &ss.Summary, &ss.Placement, &ss.PkgSha, &ss.CreatedAt, &ss.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return ss, err
}

// LastSessionVars 应用最近一次会话的变量取值（增量安装：随机凭据/输入变量的稳定预填来源）
// LastSessionVars 应用最近一次成功会话的变量取值（增量安装表单未覆盖项沿用——随机凭据稳定）。
// 只认 done 会话：失败/中止会话的变量可能是半套（新变量已填旧变量缺失），沿用会污染下次增量。
// 无成功会话时回落任意最近一次（首次增量场景兜底——旧库存无成功记录时不能拿空表单硬跑）。
func (s *Store) LastSessionVars(app string) map[string]string {
	var raw string
	_ = s.db.QueryRow(`SELECT vars FROM sessions WHERE app=? AND status='done' ORDER BY id DESC LIMIT 1`, app).Scan(&raw)
	if raw == "" {
		_ = s.db.QueryRow(`SELECT vars FROM sessions WHERE app=? ORDER BY id DESC LIMIT 1`, app).Scan(&raw)
	}
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func (s *Store) UpdateSessionStatus(id int64, status, summary string) error {
	fin := 0
	if status == "done" || status == "failed" {
		fin = int(time.Now().Unix())
	}
	_, err := s.db.Exec(`UPDATE sessions SET status=?, summary=?, finished_at=? WHERE id=?`, status, summary, fin, id)
	return err
}

type nodeRow struct {
	Key        string `json:"key"`
	HostID     int64  `json:"hostId"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt"`
	LogCount   int64  `json:"logCount"`
}

func (s *Store) SessionNodes(sessionID int64) ([]nodeRow, error) {
	rows, err := s.db.Query(`SELECT node_key, host_id, kind, title, status, message, started_at, finished_at,
		(SELECT COUNT(*) FROM node_logs l WHERE l.session_id=n.session_id AND l.node_key=n.node_key)
		FROM session_nodes n WHERE session_id=? ORDER BY n.id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []nodeRow
	for rows.Next() {
		var n nodeRow
		if err := rows.Scan(&n.Key, &n.HostID, &n.Kind, &n.Title, &n.Status, &n.Message, &n.StartedAt, &n.FinishedAt, &n.LogCount); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AddSessionNode 向既有会话追加节点（主机终端逐命令追加 cmd-N；status 由调用方指定）
func (s *Store) AddSessionNode(sessionID int64, key string, hostID int64, kind, title, status string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO session_nodes(session_id, node_key, host_id, kind, title, status)
		VALUES(?,?,?,?,?,?)`, sessionID, key, hostID, kind, title, status)
	return err
}

// FinalizeStaleTerminals console 启动兜底：上次运行遗留的 running 终端会话统一中断收尾
// （终端会话是内存态，重启即失联——agent 侧任务按自身超时自然结束）
func (s *Store) FinalizeStaleTerminals() {
	rows, err := s.db.Query(`SELECT id FROM sessions WHERE app='terminal' AND status='running'`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		_, _ = s.db.Exec(`UPDATE session_nodes SET status='failed', message='console 重启，终端会话中断'
			WHERE session_id=? AND status IN ('pending','dispatched','running')`, id)
		_ = s.UpdateSessionStatus(id, "failed", "console 重启，终端会话中断")
	}
}

func (s *Store) NodeStatus(sessionID int64, key string) (string, error) {
	var st string
	err := s.db.QueryRow(`SELECT status FROM session_nodes WHERE session_id=? AND node_key=?`, sessionID, key).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errNotFound
	}
	return st, err
}

// hostSessionEntry 主机执行历史条目：会话行 + 落在该主机上的节点（主机池「日志」抽屉数据源）
type hostSessionEntry struct {
	sessionRow
	Nodes []nodeRow `json:"nodes"`
}

// HostSessions 该主机上有节点的会话清单（最新在前，LIMIT 上限），每条附本主机节点行
func (s *Store) HostSessions(hostID int64, limit int) ([]*hostSessionEntry, error) {
	rows, err := s.db.Query(`SELECT s.id, s.app, s.version, s.package_id, s.mode, s.status, s.summary, s.created_at, s.finished_at
		FROM sessions s WHERE EXISTS(SELECT 1 FROM session_nodes n WHERE n.session_id=s.id AND n.host_id=?)
		ORDER BY s.id DESC LIMIT ?`, hostID, limit)
	if err != nil {
		return nil, err
	}
	var out []*hostSessionEntry
	byID := map[int64]*hostSessionEntry{}
	for rows.Next() {
		e := &hostSessionEntry{}
		if err := rows.Scan(&e.ID, &e.App, &e.Version, &e.PackageID, &e.Mode, &e.Status, &e.Summary, &e.CreatedAt, &e.FinishedAt); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
		byID[e.ID] = e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	nrows, err := s.db.Query(`SELECT session_id, node_key, host_id, kind, title, status, message, started_at, finished_at,
		(SELECT COUNT(*) FROM node_logs l WHERE l.session_id=n.session_id AND l.node_key=n.node_key)
		FROM session_nodes n WHERE host_id=? ORDER BY n.id`, hostID)
	if err != nil {
		return nil, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var sid int64
		var n nodeRow
		if err := nrows.Scan(&sid, &n.Key, &n.HostID, &n.Kind, &n.Title, &n.Status, &n.Message, &n.StartedAt, &n.FinishedAt, &n.LogCount); err != nil {
			return nil, err
		}
		if e, ok := byID[sid]; ok {
			e.Nodes = append(e.Nodes, n)
		}
	}
	return out, nrows.Err()
}

func (s *Store) UpdateNodeStatus(sessionID int64, key, status, message string) error {
	now := time.Now().Unix()
	switch status {
	case "running":
		_, err := s.db.Exec(`UPDATE session_nodes SET status=?, message=?, started_at=? WHERE session_id=? AND node_key=?`, status, message, now, sessionID, key)
		return err
	case "ok", "failed":
		_, err := s.db.Exec(`UPDATE session_nodes SET status=?, message=?, finished_at=? WHERE session_id=? AND node_key=?`, status, message, now, sessionID, key)
		return err
	case "pending", "dispatched", "skipped":
		_, err := s.db.Exec(`UPDATE session_nodes SET status=?, message=? WHERE session_id=? AND node_key=?`, status, message, sessionID, key)
		return err
	}
	return fmt.Errorf("未知节点状态 %q", status)
}

type logLine struct {
	ID   int64  `json:"id"`
	Node string `json:"node"`
	Seq  int64  `json:"seq"`
	Line string `json:"line"`
}

func (s *Store) AppendLogs(sessionID int64, nodeKey string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM node_logs WHERE session_id=? AND node_key=?`, sessionID, nodeKey).Scan(&seq); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, l := range lines {
		seq++
		if _, err := tx.Exec(`INSERT INTO node_logs(session_id, node_key, seq, line) VALUES(?,?,?,?)`, sessionID, nodeKey, seq, l); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// LogsAfter 增量拉取会话日志（after=上次游标行 id）；nodeKey 非空时仅取该节点行
func (s *Store) LogsAfter(sessionID int64, afterID int64, limit int, nodeKey string) ([]logLine, error) {
	q, args := `SELECT id, node_key, seq, line FROM node_logs WHERE session_id=? AND id>?`, []any{sessionID, afterID}
	if nodeKey != "" {
		q += ` AND node_key=?`
		args = append(args, nodeKey)
	}
	rows, err := s.db.Query(q+` ORDER BY id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []logLine
	for rows.Next() {
		var l logLine
		if err := rows.Scan(&l.ID, &l.Node, &l.Seq, &l.Line); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ---- inventory（已装清单：增量安装 diff 基线） ----

type invRow struct {
	App       string `json:"app"`
	HostID    int64  `json:"hostId"`
	Service   string `json:"service"`
	Kind      string `json:"kind"` // compose|systemd
	Detail    string `json:"detail"`
	UpdatedAt int64  `json:"updatedAt"`
}

// AllInventory 全部应用 inventory（应用中心列表聚合）
func (s *Store) AllInventory() ([]invRow, error) {
	rows, err := s.db.Query(`SELECT app, host_id, service, kind, detail, updated_at FROM inventory`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []invRow
	for rows.Next() {
		var r invRow
		if err := rows.Scan(&r.App, &r.HostID, &r.Service, &r.Kind, &r.Detail, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) InventoryByApp(app string) ([]invRow, error) {
	rows, err := s.db.Query(`SELECT app, host_id, service, kind, detail, updated_at FROM inventory WHERE app=?`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []invRow
	for rows.Next() {
		var r invRow
		if err := rows.Scan(&r.App, &r.HostID, &r.Service, &r.Kind, &r.Detail, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpsertInventory(rows []invRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT INTO inventory(app, host_id, service, kind, detail, updated_at) VALUES(?,?,?,?,?,?)
			ON CONFLICT(app, host_id, service) DO UPDATE SET kind=excluded.kind, detail=excluded.detail, updated_at=excluded.updated_at`,
			r.App, r.HostID, r.Service, r.Kind, r.Detail, time.Now().Unix()); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// DeleteInventoryRow 删除单条已装记录（服务迁移驱逐：cleanup 节点成功后清旧宿主行）
func (s *Store) DeleteInventoryRow(app string, hostID int64, service string) error {
	_, err := s.db.Exec(`DELETE FROM inventory WHERE app=? AND host_id=? AND service=?`, app, hostID, service)
	return err
}

func (s *Store) DeleteInventoryNotIn(app string, hostIDs []int64, services []string) error {
	// 清理已不存在的服务（按 app 下未出现在新清单的 (host, service) 删除）
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT host_id, service FROM inventory WHERE app=?`, app)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	type key struct {
		h int64
		s string
	}
	keep := map[key]bool{}
	for i := range hostIDs {
		for _, svc := range strings.Split(services[i], ",") {
			if svc != "" {
				keep[key{hostIDs[i], svc}] = true
			}
		}
	}
	var drop []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.h, &k.s); err == nil && !keep[k] {
			drop = append(drop, k)
		}
	}
	rows.Close()
	for _, k := range drop {
		if _, err := tx.Exec(`DELETE FROM inventory WHERE app=? AND host_id=? AND service=?`, app, k.h, k.s); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
