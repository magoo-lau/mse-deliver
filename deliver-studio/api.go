// deliver-studio REST API：/api/apps|components|materials|images|packages|settings。
// 上传走 multipart（MultipartReader 流式落盘）；错误统一 {"error": "..."} + 4xx/5xx。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	store  *Store
	engine *Engine
	root   string
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readBody(r *http.Request, v any) error {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// ---- apps ----

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		apps, err := s.store.ListApps()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, apps)
	case http.MethodPost, http.MethodPut:
		var a App
		if err := readBody(r, &a); err != nil {
			writeErr(w, 400, err)
			return
		}
		if strings.TrimSpace(a.Code) == "" {
			writeErr(w, 400, fmt.Errorf("应用编码必填"))
			return
		}
		if err := validateApp(&a); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := s.store.UpsertApp(&a); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, a)
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) handleAppDelete(w http.ResponseWriter, r *http.Request, code string) {
	if err := s.store.DeleteApp(code); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- components ----

func (s *Server) handleComponents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		comps, err := s.store.ListComponents()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, comps)
	case http.MethodPost, http.MethodPut:
		var c Component
		if err := readBody(r, &c); err != nil {
			writeErr(w, 400, err)
			return
		}
		if strings.TrimSpace(c.Code) == "" {
			writeErr(w, 400, fmt.Errorf("组件编码必填"))
			return
		}
		if err := validateComponent(&c); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := s.store.UpsertComponent(&c); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, c)
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) handleComponentDelete(w http.ResponseWriter, r *http.Request, code string) {
	if err := s.store.DeleteComponent(code); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleComponentsImport 导入预设（{apps:[], components:[]} 全量按编码 upsert；?replace=1 先清空应用与组件）
func (s *Server) handleComponentsImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var preset Preset
	if err := readBody(r, &preset); err != nil {
		writeErr(w, 400, err)
		return
	}
	apps, comps, err := s.applyPreset(&preset, r.URL.Query().Get("replace") == "1")
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]int{"apps": apps, "components": comps})
}

// handleComponentsExport 导出全部组件为预设 JSON
func (s *Server) handleComponentsExport(w http.ResponseWriter, r *http.Request) {
	comps, err := s.store.ListComponents()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	out := make([]Component, 0, len(comps))
	for _, c := range comps {
		out = append(out, *c)
	}
	writeJSON(w, 200, Preset{Components: out})
}

// validateComponent 组件声明校验：服务块缩进形态（与单元共用 validateComposeBlock）、镜像引用形态
func validateComponent(c *Component) error {
	block := strings.TrimRight(c.ComposeServices, "\n")
	if strings.TrimSpace(block) == "" && len(c.Images) == 0 {
		return fmt.Errorf("服务块与镜像至少其一")
	}
	if err := validateComposeBlock(c.ComposeServices, "组件 "+c.Code+" "); err != nil {
		return err
	}
	for _, img := range c.Images {
		parts := strings.Split(img, ":")
		if strings.TrimSpace(img) == "" || strings.Contains(img, " ") || len(parts) > 2 {
			return fmt.Errorf("镜像引用形态应如 repo/name:tag: %s", img)
		}
	}
	return nil
}

// ---- materials（multipart 上传） ----

var materialKinds = map[string]bool{"artifact": true, "jar": true, "ui_zip": true, "image_tar": true, "extra": true}

func (s *Server) handleMaterials(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		kind := r.URL.Query().Get("kind")
		list, err := s.store.ListMaterials(kind)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		mr, err := r.MultipartReader()
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		var kind, appCode, version, remark, fileName string
		var saved int64
		var storedPath string
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				writeErr(w, 400, err)
				return
			}
			switch part.FormName() {
			case "kind", "appCode", "version", "remark":
				val, _ := io.ReadAll(io.LimitReader(part, 256))
				switch part.FormName() {
				case "kind":
					kind = string(val)
				case "appCode":
					appCode = string(val)
				case "version":
					version = string(val)
				case "remark":
					remark = string(val)
				}
			case "file":
				fileName = filepath.Base(part.FileName())
				if err := os.MkdirAll(filepath.Join(s.root, materialsDir), 0755); err != nil {
					writeErr(w, 500, err)
					return
				}
				// 落盘文件名加纳秒后缀防同名覆盖；DB 记原始名
				storedPath = filepath.Join(s.root, materialsDir,
					nowNanos()+"-"+sanitizeFileName(fileName))
				out, err := os.Create(storedPath)
				if err != nil {
					writeErr(w, 500, err)
					return
				}
				n, err := io.Copy(out, part)
				out.Close()
				if err != nil {
					_ = os.Remove(storedPath)
					writeErr(w, 500, err)
					return
				}
				saved = n
			}
		}
		if storedPath == "" {
			writeErr(w, 400, fmt.Errorf("缺少 file 部分"))
			return
		}
		// kind 校验放在循环后：multipart part 按到达顺序流式处理，字段可能晚于 file（浏览器 FormData 按 append 序发送）
		if !materialKinds[kind] {
			_ = os.Remove(storedPath)
			writeErr(w, 400, fmt.Errorf("kind 须为 artifact(应用包)/image_tar(镜像包)/extra(附加文件)；jar/ui_zip 为旧值兼容"))
			return
		}
		if fileName == "" || fileName == "." {
			fileName = kind + "-upload.bin"
		}
		// 附加文件闸门：交付附件包只能走『打包交付 → 交付附件』入口（出交付包时逐条目解压落位），
		// 原样进套件 extras/ 会成为无人消费的嵌套 zip
		if kind == "extra" && looksLikeDeliveryZip(storedPath, fileName) {
			_ = os.Remove(storedPath)
			writeErr(w, 400, fmt.Errorf("「%s」是交付附件包，不能作为附加文件——请上传到『打包交付 → 交付附件』", fileName))
			return
		}
		m := &Material{Kind: kind, FileName: fileName, StoredPath: storedPath,
			Size: saved, AppCode: appCode, Version: version, Remark: remark}
		if err := s.store.InsertMaterial(m); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, m)
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) handleMaterialDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if _, err := s.store.DeleteMaterial(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- images（本地镜像仓） ----

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListImages()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		// 两种来源：multipart 上传 tar / JSON {ref} 从本机 docker save 导入
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "multipart/") {
			s.imageUpload(w, r)
			return
		}
		var req struct {
			Ref string `json:"ref"`
		}
		if err := readBody(r, &req); err != nil || strings.TrimSpace(req.Ref) == "" {
			writeErr(w, 400, fmt.Errorf("body 须为 {\"ref\": \"repo/name:tag\"}"))
			return
		}
		safe := safeImageName(req.Ref) + ".tar"
		dest := filepath.Join(s.root, imageStoreDir, safe)
		if exec.Command("docker", "image", "inspect", req.Ref).Run() != nil {
			writeErr(w, 400, fmt.Errorf("本机 docker 无镜像 %s（请先 docker pull/build）", req.Ref))
			return
		}
		cmd := exec.Command("docker", "save", req.Ref, "-o", dest)
		if out, err := cmd.CombinedOutput(); err != nil {
			writeErr(w, 500, fmt.Errorf("docker save 失败: %v: %s", err, string(out)))
			return
		}
		info, _ := os.Stat(dest)
		var size int64
		if info != nil {
			size = info.Size()
		}
		if err := s.store.InsertImage(safe, req.Ref, size); err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, ImageItem{FileName: safe, ImageRef: req.Ref, Size: size})
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) imageUpload(w http.ResponseWriter, r *http.Request) {
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var ref string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		if part.FormName() == "ref" {
			val, _ := io.ReadAll(io.LimitReader(part, 256))
			ref = string(val)
		}
		if part.FormName() == "file" {
			name := filepath.Base(part.FileName())
			if !strings.HasSuffix(name, ".tar") {
				writeErr(w, 400, fmt.Errorf("镜像仓仅收 .tar（docker save 产物）"))
				return
			}
			if err := os.MkdirAll(filepath.Join(s.root, imageStoreDir), 0755); err != nil {
				writeErr(w, 500, err)
				return
			}
			dest := filepath.Join(s.root, imageStoreDir, sanitizeFileName(name))
			out, err := os.Create(dest)
			if err != nil {
				writeErr(w, 500, err)
				return
			}
			n, err := io.Copy(out, part)
			out.Close()
			if err != nil {
				_ = os.Remove(dest)
				writeErr(w, 500, err)
				return
			}
			if err := s.store.InsertImage(filepath.Base(dest), ref, n); err != nil {
				writeErr(w, 500, err)
				return
			}
			writeJSON(w, 200, ImageItem{FileName: filepath.Base(dest), ImageRef: ref, Size: n})
			return
		}
	}
	writeErr(w, 400, fmt.Errorf("缺少 file 部分"))
}

func (s *Server) handleImageDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.store.DeleteImage(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- deliveries（交付附件：签发方上传/直推 → 套件重打包出最终交付包） ----

// handleDeliveries GET 列表 / POST multipart 上传附件包 zip（attachments format:1，全量校验+sha 幂等）
func (s *Server) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListDeliveries()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		mr, err := r.MultipartReader()
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		var origin, fileName, savedPath string
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				writeErr(w, 400, err)
				return
			}
			if part.FormName() == "origin" {
				val, _ := io.ReadAll(io.LimitReader(part, 32))
				origin = strings.TrimSpace(string(val))
				continue
			}
			if part.FormName() != "file" {
				continue
			}
			fileName = filepath.Base(part.FileName())
			if fileName == "" || fileName == "." || !strings.HasSuffix(strings.ToLower(fileName), ".zip") {
				writeErr(w, 400, fmt.Errorf("交付附件须为 zip（含 attachments.json 的附件包）"))
				return
			}
			if err := os.MkdirAll(filepath.Join(s.root, deliveriesDir), 0755); err != nil {
				writeErr(w, 500, err)
				return
			}
			savedPath = filepath.Join(s.root, deliveriesDir, nowNanos()+"-"+sanitizeFileName(fileName))
			out, err := os.Create(savedPath)
			if err != nil {
				writeErr(w, 500, err)
				return
			}
			_, err = io.Copy(out, part)
			out.Close()
			if err != nil {
				_ = os.Remove(savedPath)
				writeErr(w, 500, err)
				return
			}
		}
		if savedPath == "" {
			writeErr(w, 400, fmt.Errorf("缺少 file 部分"))
			return
		}
		rec, err := s.engine.saveDeliveryZip(savedPath, fileName, orDefault(origin, "upload"))
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, rec)
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) handleDeliveryDelete(w http.ResponseWriter, r *http.Request, id int64) {
	if _, err := s.store.DeleteDelivery(id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handlePackageDeliver 套件 + 交付附件 → 最终交付包（异步重打包任务，复用打包记录任务机）
func (s *Server) handlePackageDeliver(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var req struct {
		DeliveryID int64 `json:"deliveryId"`
	}
	if err := readBody(r, &req); err != nil || req.DeliveryID <= 0 {
		writeErr(w, 400, fmt.Errorf("body 须为 {\"deliveryId\": <附件记录号>}"))
		return
	}
	rec, err := s.engine.StartDeliver(id, req.DeliveryID)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, rec)
}

// ---- packages ----

func (s *Server) handlePackages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListPackages()
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, list)
	case http.MethodPost:
		var req AssembleRequest
		if err := readBody(r, &req); err != nil {
			writeErr(w, 400, err)
			return
		}
		rec, err := s.engine.StartAssemble(&req)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		writeJSON(w, 200, rec)
	default:
		w.WriteHeader(405)
	}
}

func (s *Server) handlePackageDetail(w http.ResponseWriter, r *http.Request, id int64) {
	rec, err := s.store.GetPackage(id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if rec == nil {
		writeErr(w, 404, fmt.Errorf("打包记录不存在"))
		return
	}
	if live := s.engine.LiveLog(id); live != nil {
		rec.Log = strings.Join(live, "\n")
	}
	writeJSON(w, 200, rec)
}

// ---- 大文件下载票 ----
// 套件/交付包为 GB 级 zip：前端 fetch+blob 会把整包读进浏览器内存（实测 1.6GB 即表现为
// 「点击下载无反应」）。改为登录态换一次性下载票 → 浏览器原生下载器直 GET（带进度、流式
// 落盘、零内存压力）。票 10 分钟有效（覆盖中断重试），内存表随进程重启失效（单管理员场景足够）。

type dlTicket struct {
	pkgID int64
	exp   time.Time
}

var (
	dlTicketsMu sync.Mutex
	dlTickets   = map[string]dlTicket{}
)

const dlTicketTTL = 10 * time.Minute

// handlePackageDownloadTicket POST /api/packages/{id}/ticket（登录态）→ {url}
func (s *Server) handlePackageDownloadTicket(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, fmt.Errorf("POST only"))
		return
	}
	rec, err := s.store.GetPackage(id)
	if err != nil || rec == nil || rec.ZipPath == "" || rec.Status != "ok" {
		writeErr(w, 404, fmt.Errorf("部署包不存在或未完成"))
		return
	}
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	tok := hex.EncodeToString(buf)
	now := time.Now()
	dlTicketsMu.Lock()
	for k, v := range dlTickets { // 顺手清扫过期票，防慢性堆积
		if now.After(v.exp) {
			delete(dlTickets, k)
		}
	}
	dlTickets[tok] = dlTicket{pkgID: id, exp: now.Add(dlTicketTTL)}
	dlTicketsMu.Unlock()
	writeJSON(w, 200, map[string]string{"url": fmt.Sprintf("/api/packages/%d/download?t=%s", id, tok),
		"expiresIn": dlTicketTTL.String()})
}

// validDownloadTicket 校验并按需作废下载票（过期/错包的票删除防重放）
func validDownloadTicket(tok string, id int64) bool {
	dlTicketsMu.Lock()
	defer dlTicketsMu.Unlock()
	tk, ok := dlTickets[tok]
	if ok && (time.Now().After(tk.exp) || tk.pkgID != id) {
		delete(dlTickets, tok)
		ok = false
	}
	return ok
}

func (s *Server) handlePackageDownload(w http.ResponseWriter, r *http.Request, id int64) {
	if t := r.URL.Query().Get("t"); t != "" && !validDownloadTicket(t, id) {
		writeErr(w, 401, fmt.Errorf("下载票无效或已过期（有效期 %s）——请回页面重新点击下载", dlTicketTTL))
		return
	}
	rec, err := s.store.GetPackage(id)
	if err != nil || rec == nil || rec.ZipPath == "" || rec.Status != "ok" {
		writeErr(w, 404, fmt.Errorf("部署包不存在或未完成"))
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(rec.ZipPath)+`"`)
	http.ServeFile(w, r, rec.ZipPath)
}

// ---- settings ----

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, map[string]string{
			"defaultOutName": s.store.GetSetting("defaultOutName", ""),
			"defaultVersion": s.store.GetSetting("defaultVersion", "1.0.0"),
		})
	case http.MethodPut:
		var kv map[string]string
		if err := readBody(r, &kv); err != nil {
			writeErr(w, 400, err)
			return
		}
		for k, v := range kv {
			s.store.PutSetting(k, v)
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	default:
		w.WriteHeader(405)
	}
}

// ---- 工具 ----

func nowNanos() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func sanitizeFileName(name string) string {
	return strings.NewReplacer("\\", "_", "/", "_", ":", "_", "..", "_").Replace(name)
}

func pathID(path string) (int64, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[len(parts)-1], 10, 64)
	return id, err == nil
}
