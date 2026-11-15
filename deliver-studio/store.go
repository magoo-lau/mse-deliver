// deliver-studio 存储层：SQLite（modernc.org/sqlite 纯 Go，双平台交叉编译无 cgo 依赖）。
// 建表幂等（CREATE TABLE IF NOT EXISTS）；列表类字段以 JSON 文本落库。
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

func OpenStore(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "studio.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者：串行化写，规避 database is busy
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { _ = s.db.Close() }

func now() string { return time.Now().Format("2006-01-02 15:04:05") }

const appsV2DDL = `CREATE TABLE IF NOT EXISTS apps(
	id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT UNIQUE NOT NULL, name TEXT DEFAULT '',
	default_version TEXT DEFAULT '1.0.0', definition TEXT DEFAULT '{}',
	remark TEXT DEFAULT '', updated_at TEXT DEFAULT '')`

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS components(
			id INTEGER PRIMARY KEY AUTOINCREMENT, code TEXT UNIQUE NOT NULL, name TEXT DEFAULT '',
			category TEXT DEFAULT 'middleware', default_checked INTEGER DEFAULT 0,
			images TEXT DEFAULT '[]', compose_services TEXT DEFAULT '', files TEXT DEFAULT '[]',
			app_config TEXT DEFAULT '{}', endpoints TEXT DEFAULT '[]', credentials TEXT DEFAULT '[]',
			schema TEXT DEFAULT '',
			config_center TEXT DEFAULT '',
			roles TEXT DEFAULT '[]', spread_hosts INTEGER DEFAULT 0,
			resources TEXT DEFAULT '',
			remark TEXT DEFAULT '', updated_at TEXT DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS materials(
			id INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL, file_name TEXT NOT NULL,
			stored_path TEXT NOT NULL, size INTEGER DEFAULT 0, app_code TEXT DEFAULT '', version TEXT DEFAULT '',
			remark TEXT DEFAULT '', created_at TEXT DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS image_store(
			id INTEGER PRIMARY KEY AUTOINCREMENT, file_name TEXT UNIQUE NOT NULL, image_ref TEXT DEFAULT '',
			size INTEGER DEFAULT 0, created_at TEXT DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS packages(
			id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, version TEXT DEFAULT '',
			app_code TEXT DEFAULT '', components TEXT DEFAULT '', standalone INTEGER DEFAULT 0,
			status TEXT DEFAULT 'ok', stage TEXT DEFAULT '', log TEXT DEFAULT '', zip_path TEXT DEFAULT '',
			created_at TEXT DEFAULT '', finished_at TEXT DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS deliveries(
			id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_code TEXT NOT NULL, tenant_name TEXT DEFAULT '',
			license_id TEXT DEFAULT '', package_type TEXT DEFAULT 'FULL', modules TEXT DEFAULT '',
			requires_json TEXT DEFAULT '', file_name TEXT NOT NULL, stored_path TEXT NOT NULL,
			size INTEGER DEFAULT 0, sha256 TEXT UNIQUE NOT NULL, origin TEXT DEFAULT 'upload',
			created_at TEXT DEFAULT '')`,
		`CREATE TABLE IF NOT EXISTS settings(k TEXT PRIMARY KEY, v TEXT DEFAULT '')`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	if err := s.migrateComponents(); err != nil {
		return err
	}
	if err := s.migratePackages(); err != nil {
		return err
	}
	return s.migrateApps()
}

// ReconcileDuplicateMaterials 制品存量对账：同身份（类型+文件名+应用+版本）多条历史记录只保留最新一条，
// 旧记录连同落盘文件清除，返回清除条数——旧版本库升级到本版后启动一次即收敛
func (s *Store) ReconcileDuplicateMaterials() (int, error) {
	rows, err := s.db.Query(`SELECT m.id, m.stored_path FROM materials m
		JOIN (SELECT MAX(id) AS keep_id, kind, file_name, app_code, version FROM materials
			GROUP BY kind, file_name, app_code, version) g
		ON m.kind = g.kind AND m.file_name = g.file_name AND m.app_code = g.app_code
			AND m.version = g.version AND m.id <> g.keep_id`)
	if err != nil {
		return 0, err
	}
	type stale struct {
		id   int64
		path string
	}
	var staleRows []stale
	for rows.Next() {
		st := stale{}
		if err := rows.Scan(&st.id, &st.path); err != nil {
			rows.Close()
			return 0, err
		}
		staleRows = append(staleRows, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(staleRows) == 0 {
		return 0, nil
	}
	for _, st := range staleRows {
		if _, err := s.db.Exec(`DELETE FROM materials WHERE id = ?`, st.id); err != nil {
			return 0, err
		}
		_ = os.Remove(st.path)
	}
	return len(staleRows), nil
}

// migrateComponents 组件表增量加列（schema：SQL 预置；config_center：配置中心声明）——存量库 ALTER 补列，新库建表已含
func (s *Store) migrateComponents() error {
	cols, err := s.tableColumns("components")
	if err != nil || len(cols) == 0 {
		return err
	}
	for col, ddl := range map[string]string{
		"schema":        `ALTER TABLE components ADD COLUMN schema TEXT DEFAULT ''`,
		"config_center": `ALTER TABLE components ADD COLUMN config_center TEXT DEFAULT ''`,
		"roles":         `ALTER TABLE components ADD COLUMN roles TEXT DEFAULT '[]'`,
		"spread_hosts":  `ALTER TABLE components ADD COLUMN spread_hosts INTEGER DEFAULT 0`,
		"resources":     `ALTER TABLE components ADD COLUMN resources TEXT DEFAULT ''`,
	} {
		if cols[col] {
			continue
		}
		if _, err := s.db.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

// migratePackages 打包记录表增量加列（standalone：单机部署随包 install.sh；kind/delivery 审计四列：
// 最终交付包=套件+交付附件重打包）——存量库 ALTER 补列
func (s *Store) migratePackages() error {
	cols, err := s.tableColumns("packages")
	if err != nil || len(cols) == 0 {
		return err
	}
	for col, ddl := range map[string]string{
		"standalone":     `ALTER TABLE packages ADD COLUMN standalone INTEGER DEFAULT 0`,
		"kind":           `ALTER TABLE packages ADD COLUMN kind TEXT DEFAULT 'kit'`,
		"delivery_id":    `ALTER TABLE packages ADD COLUMN delivery_id INTEGER DEFAULT 0`,
		"source_pkg_sha": `ALTER TABLE packages ADD COLUMN source_pkg_sha TEXT DEFAULT ''`,
		"delivery_sha":   `ALTER TABLE packages ADD COLUMN delivery_sha TEXT DEFAULT ''`,
	} {
		if cols[col] {
			continue
		}
		if _, err := s.db.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

// migrateApps 应用表 v1（逐列混装前后端）→ v2（definition JSON：单元+变量）。
// 检测旧列（ui_enabled）存在时：旧表改名 → 建 v2 表 → 旧行经归一化转换迁入 → 删旧表。
func (s *Store) migrateApps() error {
	cols, err := s.tableColumns("apps")
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		_, err := s.db.Exec(appsV2DDL)
		return err
	}
	if !cols["ui_enabled"] {
		return nil // 已是 v2
	}
	if _, err := s.db.Exec(`ALTER TABLE apps RENAME TO apps_legacy`); err != nil {
		return err
	}
	if _, err := s.db.Exec(appsV2DDL); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT code, name, default_version, port, health_path, app_compose_template,
		image_repo, jar_dockerfile, ui_enabled, ui_image_repo, ui_dockerfile, config_template, conf_dir, conf_env, remark
		FROM apps_legacy`)
	if err != nil {
		return err
	}
	type legacyRow struct {
		code, name, ver, port, health, compose, repo, jarDf, uiRepo, uiDf, conf, confDir, confEnv, remark string
		uiEnabled                                                                                         bool
	}
	var legacyRows []legacyRow
	for rows.Next() {
		var r legacyRow
		var uiEnabled int
		if err := rows.Scan(&r.code, &r.name, &r.ver, &r.port, &r.health, &r.compose, &r.repo, &r.jarDf,
			&uiEnabled, &r.uiRepo, &r.uiDf, &r.conf, &r.confDir, &r.confEnv, &r.remark); err != nil {
			rows.Close()
			return err
		}
		r.uiEnabled = uiEnabled != 0
		legacyRows = append(legacyRows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range legacyRows {
		// 组旧格式 JSON → App 反序列化即归一（normalizeLegacyApp），再按 v2 upsert
		confEnv := map[string]string{}
		_ = json.Unmarshal([]byte(orDefaultJSON(r.confEnv)), &confEnv)
		leg := map[string]any{
			"code": r.code, "name": r.name, "defaultVersion": r.ver, "port": r.port,
			"healthPath": r.health, "appComposeTemplate": r.compose, "imageRepo": r.repo,
			"jarDockerfile": r.jarDf, "uiEnabled": r.uiEnabled, "uiImageRepo": r.uiRepo,
			"uiDockerfile": r.uiDf, "configTemplate": r.conf, "confDir": r.confDir,
			"confEnv": confEnv, "remark": r.remark,
		}
		var a App
		if err := json.Unmarshal([]byte(toJSON(leg)), &a); err != nil {
			return fmt.Errorf("应用 %s 旧数据迁移失败: %w", r.code, err)
		}
		if a.Code == "" {
			a.Code = r.code
		}
		if err := s.UpsertApp(&a); err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`DROP TABLE apps_legacy`)
	return err
}

// tableColumns 表字段名集合（PRAGMA table_info；空 map = 表不存在）
func (s *Store) tableColumns(table string) (map[string]bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// ---- apps（v2：definition JSON 单元模型） ----

func scanApp(row interface{ Scan(...any) error }) (*App, error) {
	var id int64
	var code, def string
	a := &App{}
	if err := row.Scan(&id, &code, &a.Name, &a.DefaultVersion, &def, &a.Remark); err != nil {
		return nil, err
	}
	if strings.TrimSpace(def) != "" {
		if err := json.Unmarshal([]byte(def), a); err != nil {
			return nil, fmt.Errorf("应用 %s 定义解析失败: %w", code, err)
		}
	}
	a.ID = id
	a.Code = code
	return a, nil
}

func (s *Store) ListApps() ([]*App, error) {
	rows, err := s.db.Query(`SELECT id, code, name, default_version, definition, remark FROM apps ORDER BY code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetApp(code string) (*App, error) {
	row := s.db.QueryRow(`SELECT id, code, name, default_version, definition, remark FROM apps WHERE code = ?`, code)
	a, err := scanApp(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// UpsertApp 按编码 upsert（导入预设/界面编辑共用）；definition 存 v2 单元模型 JSON
func (s *Store) UpsertApp(a *App) error {
	def, err := json.Marshal(map[string]any{
		"code": a.Code, "name": a.Name, "defaultVersion": a.DefaultVersion,
		"units": a.Units, "variables": a.Variables,
		"defaultComponents": a.DefaultComponents, "remark": a.Remark,
	})
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO apps(code, name, default_version, definition, remark, updated_at)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(code) DO UPDATE SET name=excluded.name, default_version=excluded.default_version,
		definition=excluded.definition, remark=excluded.remark, updated_at=excluded.updated_at`,
		a.Code, a.Name, orDefault(a.DefaultVersion, "1.0.0"), string(def), a.Remark, now())
	return err
}

func (s *Store) DeleteApp(code string) error {
	_, err := s.db.Exec(`DELETE FROM apps WHERE code = ?`, code)
	return err
}

// DeleteAllApps 清空应用定义（整包替换导入用）
func (s *Store) DeleteAllApps() error {
	_, err := s.db.Exec(`DELETE FROM apps`)
	return err
}

// ---- components ----

const compCols = `id, code, name, category, default_checked, images, compose_services, files,
	app_config, endpoints, credentials, schema, config_center, roles, spread_hosts, resources, remark`

func scanComponent(row interface{ Scan(...any) error }) (*Component, error) {
	c := &Component{}
	var images, files, appConfig, endpoints, credentials, schema, configCenter, roles, resources string
	var defChecked, spreadHosts int
	if err := row.Scan(&c.ID, &c.Code, &c.Name, &c.Category, &defChecked, &images, &c.ComposeServices,
		&files, &appConfig, &endpoints, &credentials, &schema, &configCenter, &roles, &spreadHosts, &resources, &c.Remark); err != nil {
		return nil, err
	}
	c.DefaultChecked = defChecked != 0
	c.SpreadHosts = spreadHosts != 0
	c.Images = parseImages(images)
	c.Files = parseFiles(files)
	c.AppConfig = parseAppConfig(appConfig)
	c.Endpoints = parseEndpoints(endpoints)
	c.Credentials = parseCredentials(credentials)
	c.Roles = parseRoles(roles)
	c.Resources = parseResources(resources)
	c.Schema = parseSchema(schema)
	c.ConfigCenter = parseConfigCenter(configCenter)
	return c, nil
}

func (s *Store) ListComponents() ([]*Component, error) {
	rows, err := s.db.Query(`SELECT ` + compCols + ` FROM components ORDER BY category, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Component
	for rows.Next() {
		c, err := scanComponent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetComponent(code string) (*Component, error) {
	row := s.db.QueryRow(`SELECT `+compCols+` FROM components WHERE code = ?`, code)
	c, err := scanComponent(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return c, err
}

func (s *Store) UpsertComponent(c *Component) error {
	_, err := s.db.Exec(`INSERT INTO components(code, name, category, default_checked, images, compose_services,
		files, app_config, endpoints, credentials, schema, config_center, roles, spread_hosts, resources, remark, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(code) DO UPDATE SET name=excluded.name, category=excluded.category,
		default_checked=excluded.default_checked, images=excluded.images, compose_services=excluded.compose_services,
		files=excluded.files, app_config=excluded.app_config, endpoints=excluded.endpoints,
		credentials=excluded.credentials, schema=excluded.schema, config_center=excluded.config_center,
		roles=excluded.roles, spread_hosts=excluded.spread_hosts, resources=excluded.resources,
		remark=excluded.remark, updated_at=excluded.updated_at`,
		c.Code, c.Name, c.Category, boolInt(c.DefaultChecked), toJSON(c.Images), c.ComposeServices,
		toJSON(c.Files), toJSON(c.AppConfig), toJSON(c.Endpoints), toJSON(c.Credentials),
		toJSON(c.Schema), toJSON(c.ConfigCenter), toJSON(c.Roles), boolInt(c.SpreadHosts), toJSON(c.Resources), c.Remark, now())
	return err
}

// parseRoles 角色声明反序列化（坏数据回空，界面保存自愈）
func parseRoles(raw string) []Role {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []Role
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// parseResources 资源缺省反序列化（坏数据回 nil）
func parseResources(raw string) *ResourcesDecl {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "null" {
		return nil
	}
	var out ResourcesDecl
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	if out.Cpus == "" && out.MemMB <= 0 {
		return nil
	}
	return &out
}

func (s *Store) DeleteComponent(code string) error {
	_, err := s.db.Exec(`DELETE FROM components WHERE code = ?`, code)
	return err
}

// DeleteAllComponents 清空组件目录（整包替换导入用）
func (s *Store) DeleteAllComponents() error {
	_, err := s.db.Exec(`DELETE FROM components`)
	return err
}

// ---- materials ----

// InsertMaterial 制品入库并同身份去重：类型+文件名+应用+版本相同视为同一制品，
// 旧记录连同落盘文件清除、只保留最新一条（重复推送不堆积；运行中的打包任务按 ID 取制品，
// 恰逢重推覆盖同身份旧记录时任务报「制品不存在」，与手工删除制品同一失败路径，重排队即恢复）
func (s *Store) InsertMaterial(m *Material) error {
	res, err := s.db.Exec(`INSERT INTO materials(kind, file_name, stored_path, size, app_code, version, remark, created_at)
		VALUES(?,?,?,?,?,?,?,?)`, m.Kind, m.FileName, m.StoredPath, m.Size, m.AppCode, m.Version, m.Remark, now())
	if err != nil {
		return err
	}
	m.ID, _ = res.LastInsertId()
	m.CreatedAt = now()
	return s.deleteSupersededMaterials(m)
}

// deleteSupersededMaterials 清除同身份旧记录（保留 keep 这条最新记录），落盘文件一并清理。
// 先读后写：MaxOpenConns(1) 下不能持有未 Close 的 rows 再 Exec
func (s *Store) deleteSupersededMaterials(keep *Material) error {
	rows, err := s.db.Query(`SELECT id, stored_path FROM materials
		WHERE kind = ? AND file_name = ? AND app_code = ? AND version = ? AND id < ?`,
		keep.Kind, keep.FileName, keep.AppCode, keep.Version, keep.ID)
	if err != nil {
		return err
	}
	var olds []Material
	for rows.Next() {
		o := Material{}
		if err := rows.Scan(&o.ID, &o.StoredPath); err != nil {
			rows.Close()
			return err
		}
		olds = append(olds, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range olds {
		if _, err := s.db.Exec(`DELETE FROM materials WHERE id = ?`, o.ID); err != nil {
			return err
		}
		if o.StoredPath != keep.StoredPath {
			_ = os.Remove(o.StoredPath)
		}
	}
	return nil
}

func (s *Store) ListMaterials(kind string) ([]*Material, error) {
	q := `SELECT id, kind, file_name, stored_path, size, app_code, version, remark, created_at FROM materials`
	args := []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY id DESC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Material
	for rows.Next() {
		m := &Material{}
		if err := rows.Scan(&m.ID, &m.Kind, &m.FileName, &m.StoredPath, &m.Size, &m.AppCode, &m.Version, &m.Remark, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) GetMaterial(id int64) (*Material, error) {
	row := s.db.QueryRow(`SELECT id, kind, file_name, stored_path, size, app_code, version, remark, created_at
		FROM materials WHERE id = ?`, id)
	m := &Material{}
	err := row.Scan(&m.ID, &m.Kind, &m.FileName, &m.StoredPath, &m.Size, &m.AppCode, &m.Version, &m.Remark, &m.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

func (s *Store) DeleteMaterial(id int64) (*Material, error) {
	m, err := s.GetMaterial(id)
	if err != nil || m == nil {
		return m, err
	}
	_, err = s.db.Exec(`DELETE FROM materials WHERE id = ?`, id)
	_ = os.Remove(m.StoredPath)
	return m, err
}

// ---- image store ----

func (s *Store) InsertImage(fileName, ref string, size int64) error {
	_, err := s.db.Exec(`INSERT INTO image_store(file_name, image_ref, size, created_at) VALUES(?,?,?,?)
		ON CONFLICT(file_name) DO UPDATE SET image_ref=excluded.image_ref, size=excluded.size`,
		fileName, ref, size, now())
	return err
}

func (s *Store) ListImages() ([]*ImageItem, error) {
	rows, err := s.db.Query(`SELECT id, file_name, image_ref, size, created_at FROM image_store ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ImageItem
	for rows.Next() {
		i := &ImageItem{}
		if err := rows.Scan(&i.ID, &i.FileName, &i.ImageRef, &i.Size, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) DeleteImage(id int64) error {
	var file string
	err := s.db.QueryRow(`SELECT file_name FROM image_store WHERE id = ?`, id).Scan(&file)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM image_store WHERE id = ?`, id)
	if err == nil {
		_ = os.Remove(filepath.Join(imageStoreDir, file))
	}
	return err
}

// ---- packages ----

const pkgCols = `id, name, version, app_code, components, standalone, kind, delivery_id, source_pkg_sha, delivery_sha`

func scanPackage(row interface{ Scan(...any) error }, withLog bool) (*PackageRec, error) {
	p := &PackageRec{}
	var standalone int
	cols := []any{&p.ID, &p.Name, &p.Version, &p.AppCode, &p.Components, &standalone,
		&p.Kind, &p.DeliveryID, &p.SourcePkgSha, &p.DeliverySha}
	if withLog {
		cols = append(cols, &p.Status, &p.Stage, &p.Log, &p.ZipPath, &p.CreatedAt, &p.FinishedAt)
	} else {
		cols = append(cols, &p.Status, &p.Stage, &p.ZipPath, &p.CreatedAt, &p.FinishedAt)
	}
	if err := row.Scan(cols...); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	p.Standalone = standalone != 0
	if p.Kind == "" {
		p.Kind = "kit"
	}
	return p, nil
}

func (s *Store) InsertPackage(p *PackageRec) error {
	res, err := s.db.Exec(`INSERT INTO packages(name, version, app_code, components, standalone, kind, delivery_id,
		source_pkg_sha, delivery_sha, status, stage, log, zip_path, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.Name, p.Version, p.AppCode, p.Components, boolInt(p.Standalone),
		orDefault(p.Kind, "kit"), p.DeliveryID, p.SourcePkgSha, p.DeliverySha, p.Status, p.Stage, "", "", now())
	if err != nil {
		return err
	}
	p.ID, _ = res.LastInsertId()
	p.CreatedAt = now()
	return nil
}

func (s *Store) FinishPackage(id int64, status, stage, log, zipPath string) {
	_, _ = s.db.Exec(`UPDATE packages SET status=?, stage=?, log=?, zip_path=?, finished_at=? WHERE id=?`,
		status, stage, log, zipPath, now(), id)
}

func (s *Store) UpdatePackageStage(id int64, stage string) {
	_, _ = s.db.Exec(`UPDATE packages SET stage=? WHERE id=?`, stage, id)
}

// ReconcileRunningPackages 启动对账：打包任务队列在内存，进程重启即丢——
// 库中遗留的 running 记录会永久卡「排队中」，启动时统一改判失败并留痕。返回改判条数。
func (s *Store) ReconcileRunningPackages() int64 {
	res, err := s.db.Exec(`UPDATE packages SET status='failed', stage='已中断', finished_at=?,
		log = COALESCE(log,'') || char(10) || ? WHERE status='running'`,
		now(), "["+now()+"] 进程重启，打包任务中断（启动对账）——请重新发起打包")
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

func (s *Store) ListPackages() ([]*PackageRec, error) {
	rows, err := s.db.Query(`SELECT ` + pkgCols + `, status, stage, zip_path, created_at, finished_at
		FROM packages ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*PackageRec
	for rows.Next() {
		p, err := scanPackage(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPackage(id int64) (*PackageRec, error) {
	row := s.db.QueryRow(`SELECT `+pkgCols+`, status, stage, log, zip_path, created_at, finished_at
		FROM packages WHERE id = ?`, id)
	return scanPackage(row, true)
}

// ---- deliveries（交付附件台账） ----

const deliveryCols = `id, tenant_code, tenant_name, license_id, package_type, modules, requires_json,
	file_name, stored_path, size, sha256, origin, created_at`

func scanDelivery(row interface{ Scan(...any) error }) (*DeliveryRec, error) {
	d := &DeliveryRec{}
	if err := row.Scan(&d.ID, &d.TenantCode, &d.TenantName, &d.LicenseID, &d.PackageType, &d.Modules,
		&d.RequiresJSON, &d.FileName, &d.StoredPath, &d.Size, &d.Sha256, &d.Origin, &d.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return d, nil
}

func (s *Store) InsertDelivery(d *DeliveryRec) error {
	res, err := s.db.Exec(`INSERT INTO deliveries(tenant_code, tenant_name, license_id, package_type, modules,
		requires_json, file_name, stored_path, size, sha256, origin, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.TenantCode, d.TenantName, d.LicenseID, orDefault(d.PackageType, "FULL"), d.Modules, d.RequiresJSON,
		d.FileName, d.StoredPath, d.Size, d.Sha256, orDefault(d.Origin, "upload"), now())
	if err != nil {
		return err
	}
	d.ID, _ = res.LastInsertId()
	d.CreatedAt = now()
	return nil
}

func (s *Store) ListDeliveries() ([]*DeliveryRec, error) {
	rows, err := s.db.Query(`SELECT ` + deliveryCols + ` FROM deliveries ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DeliveryRec
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) GetDelivery(id int64) (*DeliveryRec, error) {
	row := s.db.QueryRow(`SELECT `+deliveryCols+` FROM deliveries WHERE id = ?`, id)
	return scanDelivery(row)
}

func (s *Store) GetDeliveryBySha(sha string) (*DeliveryRec, error) {
	row := s.db.QueryRow(`SELECT `+deliveryCols+` FROM deliveries WHERE sha256 = ?`, sha)
	return scanDelivery(row)
}

func (s *Store) DeleteDelivery(id int64) (*DeliveryRec, error) {
	d, err := s.GetDelivery(id)
	if err != nil || d == nil {
		return d, err
	}
	if _, err := s.db.Exec(`DELETE FROM deliveries WHERE id = ?`, id); err != nil {
		return d, err
	}
	_ = os.Remove(d.StoredPath)
	return d, nil
}

// ---- settings ----

func (s *Store) GetSetting(k, def string) string {
	var v string
	if err := s.db.QueryRow(`SELECT v FROM settings WHERE k = ?`, k).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) PutSetting(k, v string) {
	_, _ = s.db.Exec(`INSERT INTO settings(k, v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// orDefaultJSON 空值兜底为空对象字面量（conf_env 列历史空串容错）
func orDefaultJSON(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}
