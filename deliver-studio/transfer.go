// deliver-studio 数据迁移：内嵌示例预设一键载入、导出（声明 JSON / 含物料与镜像 zip）、导入（自动识别格式）。
// 声明数据（apps/components/settings）按编码 upsert；?replace=1 先清空应用与组件再导入（物料/镜像恒追加）。
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// applyPreset 导入预设：校验组件与应用 → 可选清空 → upsert 应用/组件 → 可选写设置。返回导入计数。
func (s *Server) applyPreset(p *Preset, replace bool) (int, int, error) {
	for i := range p.Components {
		if err := validateComponent(&p.Components[i]); err != nil {
			return 0, 0, fmt.Errorf("组件 %s: %w", p.Components[i].Code, err)
		}
	}
	for i := range p.Apps {
		if err := validateApp(&p.Apps[i]); err != nil {
			return 0, 0, fmt.Errorf("应用 %s: %w", p.Apps[i].Code, err)
		}
	}
	if replace {
		if err := s.store.DeleteAllApps(); err != nil {
			return 0, 0, err
		}
		if err := s.store.DeleteAllComponents(); err != nil {
			return 0, 0, err
		}
	}
	for i := range p.Apps {
		if err := s.store.UpsertApp(&p.Apps[i]); err != nil {
			return 0, 0, err
		}
	}
	for i := range p.Components {
		if err := s.store.UpsertComponent(&p.Components[i]); err != nil {
			return 0, 0, err
		}
	}
	for k, v := range p.Settings {
		s.store.PutSetting(k, v)
	}
	return len(p.Apps), len(p.Components), nil
}

// ---- 内嵌示例预设 ----

type PresetInfo struct {
	Name       string   `json:"name"`
	Title      string   `json:"title"` // 自动概览（如「应用 1 · 组件 7」）
	Apps       int      `json:"apps"`
	Components int      `json:"components"`
	AppCodes   []string `json:"appCodes"`
	CompCodes  []string `json:"compCodes"`
}

// handlePresets 列出内嵌示例预设（presets/*.json）
func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(405)
		return
	}
	files, err := embeddedPresets()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := make([]PresetInfo, 0, len(files))
	for name, data := range files {
		var p Preset
		if err := json.Unmarshal(data, &p); err != nil {
			continue
		}
		info := PresetInfo{Name: name, Apps: len(p.Apps), Components: len(p.Components)}
		for i := range p.Apps {
			info.AppCodes = append(info.AppCodes, p.Apps[i].Code)
		}
		for i := range p.Components {
			info.CompCodes = append(info.CompCodes, p.Components[i].Code)
		}
		info.Title = fmt.Sprintf("应用 %d · 组件 %d", info.Apps, info.Components)
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, 200, out)
}

// handlePresetApply 一键载入内嵌预设（幂等 upsert，不动已有其他内容）
func (s *Server) handlePresetApply(w http.ResponseWriter, r *http.Request, name string) {
	files, err := embeddedPresets()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	data, ok := files[name]
	if !ok {
		writeErr(w, 404, fmt.Errorf("内置预设不存在: %s", name))
		return
	}
	var p Preset
	if err := json.Unmarshal(data, &p); err != nil {
		writeErr(w, 500, fmt.Errorf("内置预设解析失败: %w", err))
		return
	}
	apps, comps, err := s.applyPreset(&p, false)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]int{"apps": apps, "components": comps})
}

// ---- 导出 ----

// handleExport 导出：缺省纯声明 JSON（应用+组件+设置）；
// ?materials=1&images=1 勾选后导出 zip（声明 + 物料文件 + 镜像仓 tar），导入端自动识别两种格式。
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	withMaterials := r.URL.Query().Get("materials") == "1"
	withImages := r.URL.Query().Get("images") == "1"

	p := Preset{Version: "1", ExportedAt: time.Now().Format("2006-01-02 15:04:05")}
	apps, err := s.store.ListApps()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	for _, a := range apps {
		va := *a
		va.ID = 0
		p.Apps = append(p.Apps, va)
	}
	comps, err := s.store.ListComponents()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	for _, c := range comps {
		vc := *c
		vc.ID = 0
		p.Components = append(p.Components, vc)
	}
	p.Settings = map[string]string{
		"defaultOutName": s.store.GetSetting("defaultOutName", ""),
		"defaultVersion": s.store.GetSetting("defaultVersion", "1.0.0"),
	}

	stamp := time.Now().Format("20060102-150405")
	if !withMaterials && !withImages {
		w.Header().Set("Content-Disposition", `attachment; filename="deliver-studio-export-`+stamp+`.json"`)
		writeJSON(w, 200, p)
		return
	}

	if withMaterials {
		mats, err := s.store.ListMaterials("")
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		for _, m := range mats {
			if !fileExists(m.StoredPath) {
				continue
			}
			mc := *m
			mc.ID = 0
			mc.StoredPath = materialsDir + "/" + filepath.Base(m.StoredPath)
			p.Materials = append(p.Materials, mc)
		}
	}
	if withImages {
		imgs, err := s.store.ListImages()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		for _, im := range imgs {
			ic := *im
			ic.ID = 0
			if !fileExists(filepath.Join(s.root, imageStoreDir, ic.FileName)) {
				continue
			}
			p.Images = append(p.Images, ic)
		}
	}

	if err := os.MkdirAll(filepath.Join(s.root, stagingDir), 0755); err != nil {
		writeErr(w, 500, err)
		return
	}
	tmp := filepath.Join(s.root, stagingDir, "export-"+nowNanos()+".zip")
	if err := writeExportZip(&p, s.root, tmp); err != nil {
		writeErr(w, 500, err)
		return
	}
	defer os.Remove(tmp)
	w.Header().Set("Content-Disposition", `attachment; filename="deliver-studio-export-`+stamp+`.zip"`)
	http.ServeFile(w, r, tmp)
}

// writeExportZip 组装完整导出包：export.json（声明+元数据）+ materials/ + image-store/
func writeExportZip(p *Preset, root, zipPath string) error {
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	meta, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := addZipBytes(zw, "export.json", meta); err != nil {
		return err
	}
	for i := range p.Materials {
		base := filepath.Base(p.Materials[i].StoredPath)
		if err := addZipFile(zw, materialsDir+"/"+base, filepath.Join(root, materialsDir, base)); err != nil {
			return err
		}
	}
	for i := range p.Images {
		if err := addZipFile(zw, imageStoreDir+"/"+p.Images[i].FileName,
			filepath.Join(root, imageStoreDir, p.Images[i].FileName)); err != nil {
			return err
		}
	}
	return zw.Close()
}

func addZipBytes(zw *zip.Writer, name string, data []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// addZipFile 已压缩件（tar/jar/exe 等）用 Store 直存，其余 Deflate
func addZipFile(zw *zip.Writer, name, src string) error {
	method := zip.Deflate
	for _, s := range []string{".tar", ".zip", ".gz", ".jar", ".exe"} {
		if strings.HasSuffix(name, s) {
			method = zip.Store
			break
		}
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
	if err != nil {
		return err
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// ---- 导入（自动识别 json / zip） ----

// handleImport 上传导入文件（multipart file；?replace=1 清空应用与组件后替换，缺省按编码合并 upsert）
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	replace := r.URL.Query().Get("replace") == "1"
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := os.MkdirAll(filepath.Join(s.root, stagingDir), 0755); err != nil {
		writeErr(w, 500, err)
		return
	}
	tmpPath, uploadName := "", ""
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if part.FormName() != "file" {
			continue
		}
		uploadName = filepath.Base(part.FileName())
		tmpPath = filepath.Join(s.root, stagingDir, "import-"+nowNanos()+"-"+sanitizeFileName(uploadName))
		out, err := os.Create(tmpPath)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		_, err = io.Copy(out, part)
		out.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
			writeErr(w, 500, err)
			return
		}
	}
	if tmpPath == "" {
		writeErr(w, 400, fmt.Errorf("缺少 file 部分"))
		return
	}
	defer os.Remove(tmpPath)
	res, err := s.importFile(tmpPath, uploadName, replace)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) importFile(path, uploadName string, replace bool) (map[string]int, error) {
	switch sniffFormat(path, uploadName) {
	case "zip":
		return s.importZip(path, replace)
	default:
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var p Preset
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %w", err)
		}
		apps, comps, err := s.applyPreset(&p, replace)
		if err != nil {
			return nil, err
		}
		return map[string]int{"apps": apps, "components": comps,
			"settings": len(p.Settings), "materials": 0, "images": 0, "skipped": 0}, nil
	}
}

// sniffFormat 优先按上传文件名后缀识别，无后缀时看文件头（zip 魔数 PK）
func sniffFormat(path, name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".zip":
		return "zip"
	case ".json":
		return "json"
	}
	f, err := os.Open(path)
	if err != nil {
		return "json"
	}
	defer f.Close()
	var head [2]byte
	if _, err := io.ReadFull(f, head[:]); err == nil && head[0] == 'P' && head[1] == 'K' {
		return "zip"
	}
	return "json"
}

// importZip 解完整导出包：export.json 必需（声明+设置）；materials/ 与 image-store/ 按元数据回拷并登记，
// 无元数据对应的包内文件跳过计数（skipped）
func (s *Server) importZip(zipPath string, replace bool) (map[string]int, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var meta []byte
	for _, f := range zr.File {
		if filepath.ToSlash(f.Name) == "export.json" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			meta, err = io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
			break
		}
	}
	if meta == nil {
		return nil, fmt.Errorf("zip 内缺 export.json——完整包导入仅支持工作台导出的 zip；纯声明请用 .json 文件")
	}
	var p Preset
	if err := json.Unmarshal(meta, &p); err != nil {
		return nil, fmt.Errorf("export.json 解析失败: %w", err)
	}
	apps, comps, err := s.applyPreset(&p, replace)
	if err != nil {
		return nil, err
	}
	res := map[string]int{"apps": apps, "components": comps,
		"settings": len(p.Settings), "materials": 0, "images": 0, "skipped": 0}

	matMeta := map[string]*Material{}
	for i := range p.Materials {
		m := &p.Materials[i]
		matMeta[filepath.Base(m.StoredPath)] = m
	}
	imgMeta := map[string]*ImageItem{}
	for i := range p.Images {
		imgMeta[p.Images[i].FileName] = &p.Images[i]
	}

	for _, f := range zr.File {
		name := filepath.ToSlash(f.Name)
		if strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
			return nil, fmt.Errorf("zip 条目路径非法: %s", f.Name)
		}
		switch {
		case strings.HasPrefix(name, materialsDir+"/"):
			base := filepath.Base(name)
			m, ok := matMeta[base]
			if !ok {
				res["skipped"]++
				continue
			}
			dest := filepath.Join(s.root, materialsDir, sanitizeFileName(base))
			if err := extractZipEntry(f, dest); err != nil {
				return nil, err
			}
			mm := *m
			mm.ID = 0
			mm.StoredPath = dest
			if info, err := os.Stat(dest); err == nil {
				mm.Size = info.Size()
			}
			if err := s.store.InsertMaterial(&mm); err != nil {
				return nil, err
			}
			res["materials"]++
		case strings.HasPrefix(name, imageStoreDir+"/"):
			base := filepath.Base(name)
			if !strings.HasSuffix(base, ".tar") {
				res["skipped"]++
				continue
			}
			ref := ""
			if im, ok := imgMeta[base]; ok {
				ref = im.ImageRef
			}
			dest := filepath.Join(s.root, imageStoreDir, sanitizeFileName(base))
			if err := extractZipEntry(f, dest); err != nil {
				return nil, err
			}
			var size int64
			if info, err := os.Stat(dest); err == nil {
				size = info.Size()
			}
			if err := s.store.InsertImage(filepath.Base(dest), ref, size); err != nil {
				return nil, err
			}
			res["images"]++
		}
	}
	return res, nil
}

func extractZipEntry(f *zip.File, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, rc)
	return err
}
