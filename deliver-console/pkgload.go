// 部署包上传与解析：zip 解包（zip-slip 防护）→ format:3 声明校验 → manifest 全量 SHA-256 强校验
// （上传即门禁，与旧单机安装器的告警级不同）→ delivery/vars.env 预填提取 → 服务块归属解析。
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const (
	componentsFile = "components.json"
	composeFile    = "docker-compose.yml"
	templateFile   = "config-template.yaml"
	manifestFile   = "manifest.json"
	deliveryDir    = "delivery"
	varsEnvFile    = "vars.env"
	imagesDirName  = "images"
	artifactsDir   = "artifacts"
)

type ServiceMeta struct {
	Name    string            `json:"name"`
	Owner   string            `json:"owner"` // unit:<code> | comp:<code>
	Kind    string            `json:"kind"`  // unit|comp
	Image   string            `json:"image"` // image: 原文（未展开 env 引用）
	Ports   map[string]string `json:"ports"` // 容器端口 → 宿主端口（未发布不在内）
	Block   string            `json:"-"`
	Deps    []string          `json:"deps"`    // depends_on 目标
	Primary bool              `json:"primary"` // 单元/组件首个服务（端点/入口口径）
}

// ParsedPackage 一次解析产物：声明 + compose 服务块 + 预填变量（会话构建的全部只读输入）
type ParsedPackage struct {
	Row     *packageRow
	Decl    *ComponentsDecl
	Compose string            // docker-compose.yml 原文
	Blocks  map[string]string // 服务名 → 块文本
	Order   []string          // 服务出现顺序
	Meta    map[string]*ServiceMeta
	Prefill map[string]string // delivery/vars.env 预填（UI 变量表单默认值）
	PkgDir  string            // 解包目录绝对路径
}

// savePackageZip 解包上传的部署包 zip 到 <root>/packages/<app>-<ver>/，校验并解析。
// 返回值未入库（调用方 UpsertPackage）；失败时清理已解目录。
func savePackageZip(root string, zipPath string) (*ParsedPackage, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("读取 zip 失败: %w", err)
	}
	defer zr.Close()

	// 第一遍：找 components.json（可能在包根或唯一一级子目录——平台最终交付包/套件均根级）
	var declRaw []byte
	prefix := ""
	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		base := strings.TrimSuffix(name, "/")
		if strings.HasSuffix(base, "/"+componentsFile) || base == componentsFile {
			if base == componentsFile {
				prefix = ""
			} else {
				prefix = strings.TrimSuffix(name[:len(name)-len(componentsFile)], "/") + "/"
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			declRaw, _ = io.ReadAll(rc)
			rc.Close()
			break
		}
	}
	if len(declRaw) == 0 {
		return nil, errors.New("包内未找到 components.json（format:3 部署包）")
	}
	var decl ComponentsDecl
	if err := json.Unmarshal(declRaw, &decl); err != nil {
		return nil, fmt.Errorf("components.json 解析失败: %w", err)
	}
	if decl.Format != declFormat && decl.Format != declFormat4 {
		return nil, fmt.Errorf("包格式 format=%d 不受支持（本控制台仅接受 format:%d/%d，请用 deliver-studio 新版重新打包）", decl.Format, declFormat, declFormat4)
	}
	if decl.Name == "" || decl.Version == "" {
		return nil, errors.New("components.json 缺少 name/version")
	}

	dest := filepath.Join(root, "packages", fmt.Sprintf("%s-%s", sanitizeSeg(decl.Name), sanitizeSeg(decl.Version)))
	if err := os.RemoveAll(dest); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return nil, err
	}
	fail := func(err error) (*ParsedPackage, error) {
		_ = os.RemoveAll(dest)
		return nil, err
	}

	// 第二遍：并发解包 + 流式 SHA-256（一次读盘同时产出解压文件与哈希，文件清单与
	// manifest 校验全部复用，不再二次扫盘——多镜像 tar 大包的耗时大头就在这两遍全包 IO）；
	// zip-slip 防护：拒绝绝对路径/../.. 与盘符
	type fileItem struct {
		rel    string // 包内相对路径（slash）
		target string // 落盘绝对路径
		size   uint64
		f      *zip.File
	}
	var items []*fileItem
	for _, f := range zr.File {
		rel := strings.TrimPrefix(filepath.ToSlash(f.Name), prefix)
		if rel == "" {
			continue
		}
		clean, err := safeRelPath(rel)
		if err != nil {
			return fail(fmt.Errorf("包内非法路径 %q: %w", f.Name, err))
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(dest, filepath.FromSlash(clean)), 0755); err != nil {
				return fail(err)
			}
			continue
		}
		items = append(items, &fileItem{rel: clean,
			target: filepath.Join(dest, filepath.FromSlash(clean)), size: f.UncompressedSize64, f: f})
	}
	sums := make(map[string]string, len(items))
	var mu sync.Mutex
	var firstErr error
	jobs := make(chan *fileItem)
	workers := runtime.GOMAXPROCS(0)
	if workers > 8 {
		workers = 8
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				sum, err := extractAndHash(it.f, it.target)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				sums[it.rel] = sum
				mu.Unlock()
			}
		}()
	}
	for _, it := range items {
		jobs <- it
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return fail(firstErr)
	}

	manifestPath := filepath.Join(dest, manifestFile)
	if _, err := os.Stat(manifestPath); err != nil {
		return fail(errors.New("包内缺少 manifest.json（完整性清单）"))
	}
	parsed, err := parsePackageDir(dest)
	if err != nil {
		return fail(err)
	}
	// manifest 强校验（上传即门禁：任一不符即拒绝，防传输损坏/篡改）——直接比对解包时已算好的哈希
	bad := parsed.verifyManifest(sums)
	if len(bad) > 0 {
		return fail(fmt.Errorf("manifest 完整性校验不符 %d 项（首例 %s）——请重新导出/上传", len(bad), bad[0]))
	}
	// 包指纹=sha256(manifest.json)——审计溯源（清单本身即全量哈希目录，其哈希即包唯一指纹）
	if sha, ok := sums[manifestFile]; ok {
		parsed.Row.PkgSha = sha
	} else if sha, err := fileSHA256(manifestPath); err == nil {
		parsed.Row.PkgSha = sha
	}
	// 文件清单（load FileSpec 的来源：解包条目 + 复用哈希；manifest.json 本身不入清单）——入库持久化，
	// 后续详情/编排经 DB 行回填，不再现场重扫全包
	parsed.Row.Files = make([]pkgFile, 0, len(items))
	for _, it := range items {
		if it.rel == manifestFile {
			continue
		}
		parsed.Row.Files = append(parsed.Row.Files, pkgFile{Path: it.rel, Size: int64(it.size), SHA256: sums[it.rel]})
	}
	return parsed, nil
}

// extractAndHash 解包单个 zip 条目并流式计算 SHA-256（单次顺序读完成两份产物）
func extractAndHash(f *zip.File, target string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return "", err
	}
	rc, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), rc)
	cerr := out.Close()
	if err != nil {
		return "", err
	}
	if cerr != nil {
		return "", cerr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parsePackageDir 从已解包目录解析声明/compose 服务块/预填（轻量：不读大文件内容、不哈希——
// 文件清单在上传时随解包一次产出并入库，详情/编排消费方经 DB 行回填 Files）
func parsePackageDir(dir string) (*ParsedPackage, error) {
	declRaw, err := os.ReadFile(filepath.Join(dir, componentsFile))
	if err != nil {
		return nil, fmt.Errorf("读取 components.json 失败: %w", err)
	}
	var decl ComponentsDecl
	if err := json.Unmarshal(declRaw, &decl); err != nil {
		return nil, fmt.Errorf("components.json 解析失败: %w", err)
	}
	if decl.Format != declFormat && decl.Format != declFormat4 {
		return nil, fmt.Errorf("仅支持 format:%d/%d 部署包", declFormat, declFormat4)
	}
	if err := validateRoleDecls(&decl); err != nil {
		return nil, err
	}
	pp := &ParsedPackage{
		Row:     &packageRow{App: decl.Name, Version: decl.Version, Dir: dir, Decl: string(declRaw)},
		Decl:    &decl,
		Meta:    map[string]*ServiceMeta{},
		Prefill: loadVarsEnvFile(filepath.Join(dir, deliveryDir, varsEnvFile)),
		PkgDir:  dir,
	}
	composeRaw, err := os.ReadFile(filepath.Join(dir, composeFile))
	if err != nil {
		return nil, fmt.Errorf("读取 docker-compose.yml 失败: %w", err)
	}
	pp.Compose = string(composeRaw)
	pp.Blocks = splitServices(pp.Compose)
	pp.Order = serviceOrder(pp.Compose)

	// 服务归属：单元（Services 清单）→ 组件（Services 清单）；entry 单元标记
	ownerOf := map[string]string{}
	kindOf := map[string]string{}
	primary := map[string]bool{}
	for i := range decl.Units {
		u := &decl.Units[i]
		svcs := u.Services
		if len(svcs) == 0 && u.Service != "" {
			svcs = []string{u.Service}
		}
		for j, s := range svcs {
			ownerOf[s], kindOf[s] = "unit:"+u.Code, "unit"
			if j == 0 {
				primary[s] = true
			}
		}
	}
	for _, c := range decl.Components {
		for j, s := range c.Services {
			ownerOf[s], kindOf[s] = "comp:"+c.Code, "comp"
			if j == 0 {
				primary[s] = true
			}
		}
		// format:4：角色服务归属组件（容错——Services 清单漏列角色服务时按 Roles 补齐归属）
		for _, r := range c.Roles {
			for _, s := range r.Services {
				if ownerOf[s] == "" {
					ownerOf[s], kindOf[s] = "comp:"+c.Code, "comp"
				}
			}
		}
	}
	for name, block := range pp.Blocks {
		m := &ServiceMeta{Name: name, Owner: ownerOf[name], Kind: kindOf[name],
			Image: serviceImage(block), Ports: servicePorts(block), Block: block,
			Deps: serviceDeps(block), Primary: primary[name]}
		if m.Kind == "" {
			m.Kind = "unknown"
		}
		pp.Meta[name] = m
	}
	if _, ok := pp.Blocks[decl.Entry.Service]; decl.Entry.Service != "" && !ok {
		return nil, fmt.Errorf("入口服务 %s 不在 compose 编排中", decl.Entry.Service)
	}
	// format:4：角色服务与单元多配置文件路径的存在性/合法性收口
	for _, c := range decl.Components {
		for _, r := range c.Roles {
			for _, s := range r.Services {
				if _, ok := pp.Blocks[s]; !ok {
					return nil, fmt.Errorf("组件 %s 角色 %s 的服务 %s 不在 compose 编排中", c.Code, r.Name, s)
				}
			}
		}
	}
	return pp, nil
}

// validateRoleDecls format:4 声明静态校验（与 compose 解析无关的部分）：角色名/服务/初始化步骤 id
// 唯一性、角色服务须在组件 Services 清单、文件路径防逃逸、探针类型与脚本完备性
func validateRoleDecls(decl *ComponentsDecl) error {
	for i := range decl.Units {
		for _, f := range decl.Units[i].ConfigFiles {
			if f.Path == "" || f.Template == "" {
				return fmt.Errorf("单元 %s 声明的配置文件缺少 path/template", decl.Units[i].Code)
			}
			if _, err := safeRelPath(f.Path); err != nil {
				return fmt.Errorf("单元 %s 配置文件路径非法 %q: %w", decl.Units[i].Code, f.Path, err)
			}
		}
	}
	for _, c := range decl.Components {
		if len(c.Roles) == 0 {
			continue
		}
		svcSet := map[string]bool{}
		for _, s := range c.Services {
			svcSet[s] = true
		}
		roleNames := map[string]bool{}
		seenSvcs := map[string]bool{}
		for _, r := range c.Roles {
			if r.Name == "" {
				return fmt.Errorf("组件 %s 存在未命名角色", c.Code)
			}
			if roleNames[r.Name] {
				return fmt.Errorf("组件 %s 角色名 %s 重复", c.Code, r.Name)
			}
			roleNames[r.Name] = true
			if len(r.Services) == 0 {
				return fmt.Errorf("组件 %s 角色 %s 未声明服务", c.Code, r.Name)
			}
			for _, s := range r.Services {
				if !svcSet[s] {
					return fmt.Errorf("组件 %s 角色 %s 的服务 %s 不在组件 Services 清单", c.Code, r.Name, s)
				}
				if seenSvcs[s] {
					return fmt.Errorf("服务 %s 同时归属组件 %s 的多个角色", s, c.Code)
				}
				seenSvcs[s] = true
			}
			for _, f := range r.Files {
				if f.Path == "" || f.Template == "" {
					return fmt.Errorf("组件 %s 角色 %s 的配置文件缺少 path/template", c.Code, r.Name)
				}
				if _, err := safeRelPath(f.Path); err != nil {
					return fmt.Errorf("组件 %s 角色 %s 配置文件路径非法 %q: %w", c.Code, r.Name, f.Path, err)
				}
			}
			if p := r.Readiness; p != nil {
				switch p.Type {
				case "", "tcp", "http":
				case "exec":
					if p.Script == "" {
						return fmt.Errorf("组件 %s 角色 %s 的 exec 探针缺少 script", c.Code, r.Name)
					}
				default:
					return fmt.Errorf("组件 %s 角色 %s 探针类型非法 %q（tcp|http|exec）", c.Code, r.Name, p.Type)
				}
			}
			initIDs := map[string]bool{}
			for _, st := range r.Init {
				if st.ID == "" || st.Script == "" {
					return fmt.Errorf("组件 %s 角色 %s 初始化步骤缺少 id/script", c.Code, r.Name)
				}
				if initIDs[st.ID] {
					return fmt.Errorf("组件 %s 角色 %s 初始化步骤 id %s 重复", c.Code, r.Name, st.ID)
				}
				initIDs[st.ID] = true
			}
		}
		// After 引用复核（角色间引用可能在声明顺序上后向）
		for _, r := range c.Roles {
			for _, st := range r.Init {
				if st.After != "" && !roleNames[st.After] {
					return fmt.Errorf("组件 %s 角色 %s 初始化步骤 %s 引用了不存在的角色 %q", c.Code, r.Name, st.ID, st.After)
				}
			}
		}
	}
	return nil
}

// verifyManifest manifest.json 逐文件 SHA-256 比对（sums=解包时已算好的哈希，缺项回源读盘兜底）；
// 返回不符清单（空=通过）
func (pp *ParsedPackage) verifyManifest(sums map[string]string) []string {
	data, err := os.ReadFile(filepath.Join(pp.PkgDir, manifestFile))
	if err != nil {
		return []string{manifestFile + " 不可读"}
	}
	var m struct {
		Files map[string]string `json:"files"`
	}
	if json.Unmarshal(data, &m) != nil {
		return []string{manifestFile + " 解析失败"}
	}
	var bad []string
	for rel, want := range m.Files {
		got, ok := sums[filepath.ToSlash(filepath.Clean(rel))]
		if !ok || got == "" {
			g, err := fileSHA256(filepath.Join(pp.PkgDir, filepath.FromSlash(rel)))
			if err != nil {
				bad = append(bad, rel+"（缺失）")
				continue
			}
			got = g
		}
		if got != strings.ToLower(want) {
			bad = append(bad, rel)
		}
	}
	return bad
}

func loadVarsEnvFile(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	vals := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			vals[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return vals
}

// safeRelPath 相对路径防逃逸（与 agent 侧同规则）
func safeRelPath(p string) (string, error) {
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || p == "" {
		return "", fmt.Errorf("空路径")
	}
	if p == ".." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") ||
		filepath.IsAbs(filepath.FromSlash(p)) {
		return "", fmt.Errorf("非法路径 %q", p)
	}
	if len(p) >= 2 && p[1] == ':' {
		return "", fmt.Errorf("非法路径 %q", p)
	}
	return p, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sanitizeSeg(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '.' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "app"
	}
	return b.String()
}
