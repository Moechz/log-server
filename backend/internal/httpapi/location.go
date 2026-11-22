// location.go —— 存储位置配置（FR-307 / D-023，重启生效）。
//
// 三个端点：
//
//	GET  /api/v1/storage/location   当前/已选位置、可用空间、是否需要重启
//	GET  /api/v1/storage/browse     浏览 /Volume* 下的目录（供 UI 选择）
//	PUT  /api/v1/storage/location   校验并保存新位置（重启后生效，仅管理员）
package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/Moechz/log-server/backend/internal/storageloc"
)

// engineDirInUse 是进程启动时生效的引擎目录（用于判断是否需要重启）。
func (s *Server) engineDirInUse() string {
	if s.EngineDir != "" {
		return s.EngineDir
	}
	return s.Cfg.VLogsStorage
}

func (s *Server) handleStorageLocation(w http.ResponseWriter, _ *http.Request) {
	cfgFile := storageloc.ConfigFile(s.Cfg.DataDir)
	chosen := storageloc.Load(cfgFile, s.Cfg.VLogsStorage)
	inUse := s.engineDirInUse()

	total, free, _ := storageloc.DiskUsage(chosen)
	if total == 0 { // 目标尚不存在时退回数据目录所在文件系统
		total, free, _ = storageloc.DiskUsage(s.Cfg.DataDir)
	}
	writable := storageloc.EnsureWritable(chosen, false) == nil

	writeData(w, map[string]any{
		"engine_dir":         chosen,
		"engine_in_use":      inUse,
		"default_engine_dir": s.Cfg.VLogsStorage,
		"data_dir":           s.Cfg.DataDir,
		"config_file":        cfgFile,
		"restart_required":   filepath.Clean(chosen) != filepath.Clean(inUse),
		"writable":           writable,
		"disk_total_bytes":   total,
		"disk_free_bytes":    free,
		"policy":             "存储位置必须位于 /Volume* 数据卷下；拒绝系统分区与 tmpfs（FR-308）",
	})
}

func (s *Server) handleStorageBrowse(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	entries, err := storageloc.List(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_path", err.Error())
		return
	}
	parent := ""
	if p != "" {
		if d := filepath.Dir(filepath.Clean(p)); d != filepath.Clean(p) {
			parent = d
		}
	}
	writeData(w, map[string]any{
		"path":    p,
		"parent":  parent,
		"entries": entries,
	})
}

func (s *Server) handleStorageLocationPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EngineDir string `json:"engine_dir"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "请求体解析失败: "+err.Error())
		return
	}
	dir := filepath.Clean(strings.TrimSpace(body.EngineDir))
	if err := storageloc.EnsureWritable(dir, true); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_location", err.Error())
		return
	}

	cfgFile := storageloc.ConfigFile(s.Cfg.DataDir)
	// 保存用户选择；同时更新进程内“待生效”目录（重启后由启动流程读取）。
	if err := storageloc.Save(cfgFile, dir); err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	if s.Self != nil {
		s.Self.Add("info", "storage", "存储位置已更新（重启后生效）", "path", dir)
	}
	s.Logger.Info("存储位置已更新（重启后生效）", "engine_dir", dir, "config", cfgFile)
	s.audit(r, "storage.location.put", dir, "ok", "restart_required")

	writeData(w, map[string]any{
		"engine_dir":       dir,
		"engine_in_use":    s.engineDirInUse(),
		"restart_required": filepath.Clean(dir) != filepath.Clean(s.engineDirInUse()),
		"config_file":      cfgFile,
	})
}
