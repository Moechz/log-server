// handlers_config.go —— 配置备份：导出 / 导入（FR-1002）
//
// 背景（D-025）：已确认 **TOS 的系统备份 / 换机迁移 / 快照不覆盖第三方应用数据**，
// 因此应用必须自己提供"配置可带走"的通道。本文件实现最小可用的一对接口：
//
//	GET  /api/v1/config/export  → 下载一份 JSON（自带元信息 + 各配置文件内容）
//	POST /api/v1/config/import  → 上传该 JSON，校验后原子写回
//
// 覆盖范围 = `<DataDir>/config/` 下**我们自己的 JSON 配置**：
//
//	config.json   采集源 / 转发目标 / 告警规则 / auth 段
//	storage.json  存储位置（D-023，重启生效）
//
// 明确**不包含**：
//   - 日志数据（VictoriaLogs 库在 data/engine，体积大且需停机保证一致性 → 自行备份数据目录）
//   - `config/tls/` 下的证书与**私钥**（红线 7：私钥绝不进入任何导出件）
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Moechz/log-server/backend/internal/version"
)

// configBackupFiles 是允许导出/导入的配置文件名白名单（不接受任何其它名字，
// 因此不存在路径穿越：写回时只拼接 filepath.Base 校验过的名字）。
var configBackupFiles = []string{"config.json", "storage.json"}

// configBackup 是导出件格式（也是导入时接受的格式）。
type configBackup struct {
	AppID      string                     `json:"app_id"`
	Version    string                     `json:"version"`
	ExportedAt string                     `json:"exported_at"`
	Note       string                     `json:"note,omitempty"`
	Files      map[string]json.RawMessage `json:"files"`
}

const configBackupMaxBody = 4 << 20 // 4 MiB：配置是纯文本 JSON，远小于此

// configDir 返回应用配置目录（<DataDir>/config）。
func (s *Server) configDir() string {
	if s.Cfg.ConfigDir != "" {
		return s.Cfg.ConfigDir
	}
	return filepath.Join(s.Cfg.DataDir, "config")
}

// handleConfigExport 打包当前配置（FR-1002）。
func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	dir := s.configDir()
	out := configBackup{
		AppID:      version.AppID,
		Version:    version.Version,
		ExportedAt: time.Now().Format(time.RFC3339),
		Note:       "配置备份（不含日志数据与 TLS 私钥）。导入后需重启应用生效。",
		Files:      map[string]json.RawMessage{},
	}
	for _, name := range configBackupFiles {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // 未配置过该项 → 导出件里就不含它
			}
			writeErr(w, http.StatusInternalServerError, "read_failed", fmt.Sprintf("读取 %s 失败: %v", name, err))
			return
		}
		if !isJSONObject(b) {
			writeErr(w, http.StatusInternalServerError, "invalid_config",
				fmt.Sprintf("%s 不是合法 JSON，已拒绝导出（避免生成无法导入的备份件）", name))
			return
		}
		out.Files[name] = json.RawMessage(b)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("%s-config-%s.json", version.AppID, time.Now().Format("20060102-150405"))))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(b, '\n'))
	if s.Self != nil {
		s.Self.Add("info", "config", fmt.Sprintf("导出配置（%d 个文件）", len(out.Files)))
	}
	s.audit(r, "config.export", "", "ok", fmt.Sprintf("files=%d", len(out.Files)))
}

// handleConfigImport 导入配置（校验 + 原子写回；重启后生效）。
func (s *Server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	var in configBackup
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, configBackupMaxBody)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "备份件解析失败: "+err.Error())
		return
	}
	if in.AppID != "" && in.AppID != version.AppID {
		writeErr(w, http.StatusBadRequest, "app_id_mismatch",
			fmt.Sprintf("备份件属于 %q，本应用是 %q", in.AppID, version.AppID))
		return
	}
	if len(in.Files) == 0 {
		writeErr(w, http.StatusBadRequest, "empty_backup", "备份件里没有任何配置文件")
		return
	}
	dir := s.configDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir_failed", err.Error())
		return
	}
	written := make([]string, 0, len(in.Files))
	for name, raw := range in.Files {
		// 白名单校验：既挡未知文件，也从根本上消除路径穿越
		if !isBackupFileName(name) {
			writeErr(w, http.StatusBadRequest, "unknown_file",
				fmt.Sprintf("备份件含不受支持的文件 %q", name))
			return
		}
		if !isJSONObject(raw) {
			writeErr(w, http.StatusBadRequest, "invalid_json",
				fmt.Sprintf("%s 必须是 JSON 对象（{...}）", name))
			return
		}
		if err := writeFileAtomic(filepath.Join(dir, name), raw, 0o640); err != nil {
			writeErr(w, http.StatusInternalServerError, "write_failed", fmt.Sprintf("写入 %s 失败: %v", name, err))
			return
		}
		written = append(written, name)
	}
	if s.Self != nil {
		s.Self.Add("warn", "config", "导入配置："+strings.Join(written, ", ")+"（重启后生效）")
	}
	s.audit(r, "config.import", "", "ok", "files="+strings.Join(written, ","))
	writeData(w, map[string]any{
		"written":          written,
		"restart_required": true,
		"message":          "配置已写入，重启应用后生效（存储位置变更需重启引擎）",
	})
}

// isJSONObject 判定内容是否为 JSON 对象（配置文件都是对象；拒绝字符串/数组/标量）。
func isJSONObject(b []byte) bool {
	var probe map[string]json.RawMessage
	return json.Unmarshal(b, &probe) == nil && probe != nil
}

// isBackupFileName 判断名字是否在白名单内。
func isBackupFileName(name string) bool {
	if name == "" || name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, allow := range configBackupFiles {
		if name == allow {
			return true
		}
	}
	return false
}

// writeFileAtomic 写临时文件后 rename，避免半截文件（同 saveAppConfig 的做法）。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
