// deliver-studio 交付附件（attachments format:1）与最终交付包重打包。
// 职责归位（deliver-design.md ADR-9）：签发方（如 ai-mse 平台离线交付）只产出附件包
// （license + 租户数据 + 租户变量 + schema，delivery/deploy-package.zip 内层格式与签名不动），
// studio 在 format:3 套件之上叠加附件并重建外层清单，产出最终交付包——目录结构与消费链
// （deliver-console / install.sh / 应用侧自动导入）保持不变。
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// AttachTenant 附件描述符的租户身份段（id 为签发方世界的编号，原样透传仅作展示）
type AttachTenant struct {
	ID   json.Number `json:"id,omitempty"`
	Code string      `json:"code"`
	Name string      `json:"name,omitempty"`
}

// AttachmentsDecl 附件包描述符（附件 zip 根 attachments.json，attachments format:1）：
// files 为逐文件 SHA-256 清单（attachments.json 自身不入清单）；路径只允许 delivery/** 与 extras/**。
// requires 为签发方元数据透明透传（studio 不校验其语义）：platformVersion 为 string，
// perBizSchemaVersion 为嵌套对象（biz → 租户库 schema 账本最高版本，导入端消费），故值为混合类型。
type AttachmentsDecl struct {
	Format      int               `json:"format"` // 1
	Tenant      AttachTenant      `json:"tenant"`
	LicenseID   string            `json:"licenseId,omitempty"`
	PackageType string            `json:"packageType"` // FULL|UPGRADE
	Modules     []string          `json:"modules,omitempty"`
	Requires    map[string]any    `json:"requires,omitempty"`
	Files       map[string]string `json:"files"`
}

// DeliveryRec 交付附件台账（zip 存 deliveries/；按 sha256 幂等，同包重传复用）
type DeliveryRec struct {
	ID           int64  `json:"id"`
	TenantCode   string `json:"tenantCode"`
	TenantName   string `json:"tenantName"`
	LicenseID    string `json:"licenseId"`
	PackageType  string `json:"packageType"`
	Modules      string `json:"modules"` // csv
	RequiresJSON string `json:"requires"`
	FileName     string `json:"fileName"`
	StoredPath   string `json:"storedPath"`
	Size         int64  `json:"size"`
	Sha256       string `json:"sha256"`
	Origin       string `json:"origin"` // upload（界面上传）| push（签发方直推）
	CreatedAt    string `json:"createdAt"`
}

// attachmentPathAllowed 附件落位白名单：只允许 delivery/** 与 extras/**（防越权覆盖套件编排/镜像）
func attachmentPathAllowed(p string) bool {
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == "" || strings.HasPrefix(clean, "/") || strings.Contains(clean, "..") {
		return false
	}
	return strings.HasPrefix(clean, "delivery/") || strings.HasPrefix(clean, "extras/")
}

// 必备件：console 表单预填 + 应用侧自动导入的最小集合
var attachmentRequired = []string{"delivery/vars.env", "delivery/deploy-package.zip"}

// 交付附件包的签发命名（新旧两代：attachments-<code>-<type>-… / tenant-<code>-<type>-…）
var deliveryZipNameRe = regexp.MustCompile(`(?i)^(attachments|tenant)-.+\.zip$`)

// looksLikeDeliveryZip 识别"长得像交付附件包"的 zip：文件名命中签发命名，或 zip 根级含
// attachments.json（format:1 描述符）。制品库「附加文件」上传与套件打包两道闸门共用——
// 附件包只能走「交付附件」入口（出交付包时逐条目解压落位 delivery/ 与 extras/），
// 原样塞进套件 extras/ 会成为无人消费的嵌套 zip（console 只认包根 delivery/vars.env 散文件）。
func looksLikeDeliveryZip(path, name string) bool {
	if deliveryZipNameRe.MatchString(filepath.Base(name)) {
		return true
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return false
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == "attachments.json" {
			return true
		}
	}
	return false
}

// parseDeliveryZip 解析并全量校验附件包：描述符形态、必备件、逐文件 SHA-256、
// 未登记条目拦截、落位白名单。返回描述符（校验通过即信任，重打包时不再重算哈希）。
func parseDeliveryZip(zipPath string) (*AttachmentsDecl, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("附件包不是有效 zip: %w", err)
	}
	defer zr.Close()
	entries := map[string]*zip.File{}
	for i := range zr.File {
		f := zr.File[i]
		if f.FileInfo().IsDir() {
			continue
		}
		entries[filepath.ToSlash(f.Name)] = f
	}
	af, ok := entries["attachments.json"]
	if !ok {
		return nil, fmt.Errorf("附件包缺少 attachments.json 描述符（attachments format:1）")
	}
	rc, err := af.Open()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(rc, 4<<20))
	rc.Close()
	if err != nil {
		return nil, err
	}
	var decl AttachmentsDecl
	if err := json.Unmarshal(data, &decl); err != nil {
		return nil, fmt.Errorf("attachments.json 解析失败: %w", err)
	}
	if decl.Format != 1 {
		return nil, fmt.Errorf("attachments.json format 须为 1（当前 %d）", decl.Format)
	}
	if strings.TrimSpace(decl.Tenant.Code) == "" {
		return nil, fmt.Errorf("attachments.json 缺少 tenant.code（租户编码）")
	}
	if decl.PackageType != "FULL" && decl.PackageType != "UPGRADE" {
		return nil, fmt.Errorf("attachments.json packageType 须为 FULL/UPGRADE（当前 %q）", decl.PackageType)
	}
	if len(decl.Files) == 0 {
		return nil, fmt.Errorf("attachments.json files 清单为空")
	}
	for _, req := range attachmentRequired {
		if _, ok := decl.Files[req]; !ok {
			return nil, fmt.Errorf("附件包缺少必备件 %s", req)
		}
	}
	for path, want := range decl.Files {
		if !attachmentPathAllowed(path) {
			return nil, fmt.Errorf("附件路径越权（只允许 delivery/** 与 extras/**）: %s", path)
		}
		f, ok := entries[path]
		if !ok {
			return nil, fmt.Errorf("附件清单登记的文件在包内不存在: %s", path)
		}
		sum, err := zipFileSha256(f)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(sum, want) {
			return nil, fmt.Errorf("附件文件哈希与清单不符: %s", path)
		}
	}
	for name := range entries {
		if name == "attachments.json" {
			continue
		}
		if _, ok := decl.Files[name]; !ok {
			return nil, fmt.Errorf("附件包内存在清单未登记的文件: %s", name)
		}
	}
	return &decl, nil
}

func zipFileSha256(f *zip.File) (string, error) {
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// kitPkgShaFromZip 套件包指纹 = sha256(manifest.json)——与 deliver-console 的 pkgSha 同口径，
// 重打包记录为 sourcePkgSha256 供审计溯源
func kitPkgShaFromZip(zipPath string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for i := range zr.File {
		if !zr.File[i].FileInfo().IsDir() && filepath.ToSlash(zr.File[i].Name) == "manifest.json" {
			return zipFileSha256(zr.File[i])
		}
	}
	return "", fmt.Errorf("套件包内缺少 manifest.json")
}

// ---- 重打包（套件 + 附件 → 最终交付包） ----

// StartDeliver 在既有套件（format:3 打包记录）之上叠加交付附件，异步产出最终交付包。
// 异步任务机与打包共用（进度/活日志/轮询/下载端点全复用）。
func (e *Engine) StartDeliver(kitID, deliveryID int64) (*PackageRec, error) {
	kit, err := e.store.GetPackage(kitID)
	if err != nil {
		return nil, err
	}
	if kit == nil {
		return nil, fmt.Errorf("套件打包记录不存在: #%d", kitID)
	}
	if kit.Kind == "final" {
		return nil, fmt.Errorf("记录 #%d 已是最终交付包（套件+附件），不能再叠加附件", kitID)
	}
	if kit.Status != "ok" || kit.ZipPath == "" || !fileExists(kit.ZipPath) {
		return nil, fmt.Errorf("套件 #%d 未完成打包或包文件缺失，不能叠加附件", kitID)
	}
	dl, err := e.store.GetDelivery(deliveryID)
	if err != nil {
		return nil, err
	}
	if dl == nil {
		return nil, fmt.Errorf("交付附件不存在: #%d", deliveryID)
	}
	kitSha, err := kitPkgShaFromZip(kit.ZipPath)
	if err != nil {
		return nil, fmt.Errorf("套件包指纹读取失败: %w", err)
	}
	rec := &PackageRec{Name: kit.Name, Version: kit.Version, AppCode: kit.AppCode,
		Kind: "final", DeliveryID: dl.ID, SourcePkgSha: kitSha, DeliverySha: dl.Sha256,
		Status: "running", Stage: "排队中"}
	if err := e.store.InsertPackage(rec); err != nil {
		return nil, err
	}
	task := &assembleTask{}
	e.mu.Lock()
	e.tasks[rec.ID] = task
	e.mu.Unlock()
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		task.mu.Lock()
		task.lines = append(task.lines, line)
		task.mu.Unlock()
	}
	go func() {
		err := e.deliverAssemble(kit, dl, kitSha, logf)
		e.mu.Lock()
		delete(e.tasks, rec.ID)
		e.mu.Unlock()
		if err != nil {
			logf("重打包失败 ✘: %v", err)
			e.store.FinishPackage(rec.ID, "failed", "失败",
				strings.Join(append(task.snapshot(), fmt.Sprintf("重打包失败: %v", err)), "\n"), "")
			return
		}
		zipPath := filepath.Join(e.root, packagesDir, finalZipName(kit.Name, kit.Version, dl.TenantCode))
		e.store.FinishPackage(rec.ID, "ok", "完成", strings.Join(task.snapshot(), "\n"), zipPath)
		logf("最终交付包就绪 ✔ %s", zipPath)
	}()
	return rec, nil
}

func finalZipName(kitName, kitVersion, tenantCode string) string {
	return composeProjectName(kitName) + "-" + kitVersion + "-" + sanitizeFileName(tenantCode) + ".zip"
}

// deliverAssemble 重打包主体：解包套件（manifest 硬校验）→ 叠加附件（白名单+冲突拦截）→
// INSTALL.md + attachments.json 溯源 → 重建外层 manifest（含 delivery 审计段）→ zip（.sh 执行位保留）
func (e *Engine) deliverAssemble(kit *PackageRec, dl *DeliveryRec, kitSha string, logf func(string, ...any)) error {
	proj := composeProjectName(kit.Name)
	if proj == "" {
		return fmt.Errorf("套件包名无效")
	}
	pkgDir := filepath.Join(e.root, packagesDir, proj+"-"+kit.Version+"-"+sanitizeFileName(dl.TenantCode))
	_ = os.RemoveAll(pkgDir)
	logf("==> [1/4] 解包套件 %s-%s（manifest 硬校验）", kit.Name, kit.Version)
	if err := unzipDir(kit.ZipPath, pkgDir); err != nil {
		return fmt.Errorf("套件解包失败: %w", err)
	}
	if err := verifyKitManifest(pkgDir, logf); err != nil {
		return err
	}

	logf("==> [2/4] 叠加交付附件（租户 %s，%s）", dl.TenantCode, dl.PackageType)
	for _, note := range attachmentVarsNotes(kit.ZipPath, dl.StoredPath) {
		logf("%s", note)
	}
	zr, err := zip.OpenReader(dl.StoredPath)
	if err != nil {
		return fmt.Errorf("附件包打开失败: %w", err)
	}
	defer zr.Close()
	var names []string
	entry := map[string]*zip.File{}
	for i := range zr.File {
		f := zr.File[i]
		if f.FileInfo().IsDir() {
			continue
		}
		n := filepath.ToSlash(f.Name)
		names = append(names, n)
		entry[n] = f
	}
	sort.Strings(names)
	for _, n := range names {
		target := filepath.Join(pkgDir, filepath.FromSlash(n))
		if n != "attachments.json" && fileExists(target) {
			return fmt.Errorf("附件 %s 与套件既有文件冲突——套件已含同名路径（extras 附加文件请在打包时避免同名）", n)
		}
		if n == "attachments.json" {
			target = filepath.Join(pkgDir, "delivery", "attachments.json") // 溯源副本（描述符不占附件命名空间）
		}
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(pkgDir)) {
			return fmt.Errorf("附件路径越界: %s", n)
		}
		if err := writeZipEntry(entry[n], target); err != nil {
			return err
		}
		logf("    + %s", n)
	}

	logf("==> [3/4] 生成 INSTALL.md 与外层清单")
	install := installMd(kit.Name, kit.Version, dl)
	if err := os.WriteFile(filepath.Join(pkgDir, "INSTALL.md"), []byte(install), 0644); err != nil {
		return err
	}
	// 外层 manifest：与打包引擎同构的逐文件 SHA-256 全量清单 + delivery 审计段
	// （console pkgSha=sha256(manifest.json) 口径不变；files 覆盖包括 delivery/ 在内的全部条目）
	// 套件自身的旧 manifest.json 先移除再全量重算（新清单不收录自身）
	_ = os.Remove(filepath.Join(pkgDir, "manifest.json"))
	files := map[string]string{}
	if err := filepath.Walk(pkgDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(pkgDir, path)
		sum, err := fileSha256(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = sum
		return nil
	}); err != nil {
		return err
	}
	audit := map[string]any{
		"tenantCode": dl.TenantCode, "licenseId": dl.LicenseID,
		"packageType": dl.PackageType, "sourcePkgSha256": kitSha, "attachmentsSha256": dl.Sha256,
	}
	if dl.TenantName != "" {
		audit["tenantName"] = dl.TenantName
	}
	if strings.TrimSpace(dl.RequiresJSON) != "" && dl.RequiresJSON != "null" {
		var req map[string]any
		if json.Unmarshal([]byte(dl.RequiresJSON), &req) == nil && len(req) > 0 {
			audit["requires"] = req
		}
	}
	if dl.Modules != "" {
		audit["modules"] = strings.Split(dl.Modules, ",")
	}
	mj, err := json.MarshalIndent(map[string]any{
		"name": proj, "version": kit.Version, "delivery": audit, "files": files,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "manifest.json"), mj, 0644); err != nil {
		return err
	}

	logf("==> [4/4] 压缩最终交付包")
	zipPath := filepath.Join(e.root, packagesDir, finalZipName(kit.Name, kit.Version, dl.TenantCode))
	if err := zipDir(pkgDir, zipPath); err != nil {
		return err
	}
	logf("    %s（含 %d 个文件）", zipPath, len(files))
	return nil
}

// verifyKitManifest 套件 manifest 逐文件 SHA-256 硬校验（重打包动套件内容前的完整性门禁，
// 比「告警级」更严：任一不符即拒绝）；manifest.json 自身不入清单。
func verifyKitManifest(pkgDir string, logf func(string, ...any)) error {
	data, err := os.ReadFile(filepath.Join(pkgDir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("套件缺少 manifest.json: %w", err)
	}
	var m struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("套件 manifest.json 解析失败: %w", err)
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("套件 manifest.json files 清单为空")
	}
	var bad []string
	for rel, want := range m.Files {
		sum, err := fileSha256(filepath.Join(pkgDir, filepath.FromSlash(rel)))
		if err != nil {
			bad = append(bad, rel+"（缺失）")
			continue
		}
		if !strings.EqualFold(sum, want) {
			bad = append(bad, rel)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("套件完整性校验不符 %d 项（首例 %s）——套件包可能损坏，请重新打包", len(bad), bad[0])
	}
	logf("    完整性校验通过（%d 项）", len(m.Files))
	return nil
}

// attachmentVarsNotes 附件变量 vs 套件安装变量匹配告警（非阻断）：附件 vars.env 携带而套件
// components.json 未声明同名安装变量的键 → console 预填不命中（安装表单将手动输入）。
// 附件链对应用的要求是「声明同名 install 变量 + 模板引用」即可预填命中，未声明不拦截出包；
// 解析失败静默返回空——告警通道自身不阻断重打包。
func attachmentVarsNotes(kitZip, deliveryZip string) []string {
	kitVars, err := kitInstallVars(kitZip)
	if err != nil {
		return nil
	}
	keys, err := varsEnvKeys(deliveryZip)
	if err != nil || len(keys) == 0 {
		return nil
	}
	var unknown []string
	for _, k := range keys {
		if !kitVars[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return []string{fmt.Sprintf("提示: 附件变量 %s 未在套件安装变量中声明——console 预填不命中，"+
		"安装表单将手动输入；如非预期请核对附件与套件的模块集/形态匹配（应用定义声明同名 install 变量即可命中）",
		strings.Join(unknown, "/"))}
}

// kitInstallVars 套件 components.json 的安装变量名集合
func kitInstallVars(kitZip string) (map[string]bool, error) {
	zr, err := zip.OpenReader(kitZip)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for i := range zr.File {
		f := zr.File[i]
		if f.FileInfo().IsDir() || filepath.ToSlash(f.Name) != "components.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var decl struct {
			Variables []VarDecl `json:"variables"`
		}
		if err := json.NewDecoder(rc).Decode(&decl); err != nil {
			return nil, err
		}
		out := map[string]bool{}
		for _, v := range decl.Variables {
			out[v.Name] = true
		}
		return out, nil
	}
	return nil, fmt.Errorf("套件缺 components.json")
}

// varsEnvKeys 附件包 delivery/vars.env 的键清单（跳过注释与空行）
func varsEnvKeys(deliveryZip string) ([]string, error) {
	zr, err := zip.OpenReader(deliveryZip)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for i := range zr.File {
		f := zr.File[i]
		if f.FileInfo().IsDir() || filepath.ToSlash(f.Name) != "delivery/vars.env" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		if err != nil {
			return nil, err
		}
		var keys []string
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, _, _ := strings.Cut(line, "=")
			if key = strings.TrimSpace(key); key != "" {
				keys = append(keys, key)
			}
		}
		return keys, nil
	}
	return nil, fmt.Errorf("附件包缺 delivery/vars.env")
}

func writeZipEntry(f *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}

// installMd 最终交付包安装指引（deliver-console 主线；install.sh 薄壳随包时单机亦可直装）。
// 模板为原始字符串（不能含反引号），以 ~ 占位、成文时替换为 Markdown 代码span的反引号。
func installMd(kitName, kitVersion string, dl *DeliveryRec) string {
	typ := "全量交付（FULL）"
	if dl.PackageType == "UPGRADE" {
		typ = "升级数据包（UPGRADE，补丁表数据替换）"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s %s 最终交付包安装指引\n\n", kitName, kitVersion)
	fmt.Fprintf(&b, "租户 %s（%s）｜%s\n\n", dl.TenantCode, orDefault(dl.TenantName, dl.TenantCode), typ)
	b.WriteString(`本包为完整交付物：format:3 运行时套件（compose/全部镜像/配置模板）+ delivery/（授权凭证 +
租户数据 + 交付预置变量）+ extras/schema.sql（表结构）。安装统一走 deliver-console 安装控制台；
若套件打包时选择了「单机部署」，包根的 install.sh 薄壳可直接单机首装（升级/多机仍用 console）。

## 一键部署（推荐）

1. 在安装机（或任一内网机）启动 deliver-console（登录 admin，首启随机密码见启动控制台）。
2. 接入目标主机：多机时把 deliver-agent 二进制（console 主机池页可下载）拷到各目标机执行
   ~./deliver-agent-linux install -server http://<console>:7870 -token <引导令牌>~（agent 出站长轮询，
   目标机无需开放入站端口）；单机场景零 agent（console 所在机器即本机节点）。
3. console 上传本包 zip（上传即 manifest 逐文件 SHA-256 强校验）→ 编排安装：拖拽分配服务到主机
   （单元可选 docker / systemd 裸机；未落机组件=复用外部设施，端点手填）→ 租户身份/许可公钥已由
   delivery/vars.env 自动预填 → 确认 DAG → 执行。
4. 表结构：落机 MySQL 经 extras/schema.sql 首启自动预置；复用外部库由 console 按组件 schema
   声明以一次性容器执行（应用启动自足化直建兜底）。
5. delivery/deploy-package.zip 由 console 拷至入口单元所在主机配置目录，应用启动自动导入
   （RS256 验签 → license 落盘热生效 → 数据按表整表替换装载；导入完成改名 .imported-*）。
6. 使用交付的管理员账号登录（入口地址见 console 会话完成摘要）。

## 目录结构

- ~docker-compose.yml~ / ~components.json~（format:3）/ ~config-template.yaml~ —— 编排与声明（套件）
- ~images/~ —— 全部镜像 tar（console 按主机裁剪下发 docker load）
- ~artifacts/~ —— 随包制品（可选：console systemd 裸机部署单元用）
- ~extras/schema.sql~ —— 表结构 DDL（幂等；结构权威是应用启动自足化直建）
- ~delivery/vars.env~ —— 交付预置变量（租户身份 + 许可公钥，console 表单自动预填）
- ~delivery/deploy-package.zip~ —— 签名数据载荷（license + 租户数据 JSONL，导入端 RS256 验签）
- ~delivery/attachments.json~ —— 附件描述符溯源副本（租户/licenseId/来源套件指纹）
- ~INSTALL.md~ —— 本指引

## 校验与兜底

- 软件部分：manifest.json 逐文件 SHA-256（console 上传即强校验，不符拒绝）。
- 数据部分：delivery/deploy-package.zip 内 manifest 由签发方 RSA 私钥签名，租户侧仅持公钥无法伪造。
- 兜底路径：应用启动后以管理员登录「部署包导入」页，Web 上传 delivery/deploy-package.zip
  效果等同自动导入（手工环境请先完成安装器部署步骤）。
`)
	return strings.ReplaceAll(b.String(), "~", "`")
}

// saveDeliveryZip 校验通过的附件落库（sha 幂等：同包重传返回既有记录，重复落盘文件清理）
var deliveryMu sync.Mutex

func (e *Engine) saveDeliveryZip(zipPath, fileName, origin string) (*DeliveryRec, error) {
	deliveryMu.Lock()
	defer deliveryMu.Unlock()
	decl, err := parseDeliveryZip(zipPath)
	if err != nil {
		_ = os.Remove(zipPath)
		return nil, err
	}
	sum, err := fileSha256(zipPath)
	if err != nil {
		_ = os.Remove(zipPath)
		return nil, err
	}
	if old, err := e.store.GetDeliveryBySha(sum); err != nil {
		_ = os.Remove(zipPath)
		return nil, err
	} else if old != nil {
		_ = os.Remove(zipPath) // 同内容包已入库：复用记录，避免堆积
		return old, nil
	}
	rec := &DeliveryRec{
		TenantCode: decl.Tenant.Code, TenantName: decl.Tenant.Name, LicenseID: decl.LicenseID,
		PackageType: decl.PackageType, Modules: strings.Join(decl.Modules, ","),
		RequiresJSON: toJSON(decl.Requires), FileName: fileName, StoredPath: zipPath,
		Size: fileSizeOf(zipPath), Sha256: sum, Origin: origin,
	}
	if err := e.store.InsertDelivery(rec); err != nil {
		_ = os.Remove(zipPath)
		return nil, err
	}
	return rec, nil
}

func fileSizeOf(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}
