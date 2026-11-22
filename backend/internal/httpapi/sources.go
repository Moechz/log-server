// sources.go —— 文件采集源管理（FR-102）与采集总开关（FR-1107）。
package httpapi

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// SourceSpec 是文件采集源的写入形态（与 config.json 的 sources 对齐）。
type SourceSpec struct {
	Path      string `json:"path"`
	Multiline bool   `json:"multiline"`
	Enabled   bool   `json:"enabled"`
	Encoding  string `json:"encoding,omitempty"`
}

func (s *Server) ingestPaused() bool {
	return s.IngestPaused != nil && s.IngestPaused()
}

// handleSourcesPut 全量替换文件采集源（校验绝对路径、去重），保存并热生效。
func (s *Server) handleSourcesPut(w http.ResponseWriter, r *http.Request) {
	if s.SourcesApply == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "采集源管理未启用")
		return
	}
	var body struct {
		Sources []SourceSpec `json:"sources"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "请求体解析失败: "+err.Error())
		return
	}
	seen := map[string]bool{}
	out := make([]SourceSpec, 0, len(body.Sources))
	for _, sp := range body.Sources {
		sp.Path = strings.TrimSpace(sp.Path)
		if sp.Path == "" {
			continue
		}
		if !filepath.IsAbs(sp.Path) {
			writeErr(w, http.StatusBadRequest, "bad_path", "采集路径必须是绝对路径："+sp.Path)
			return
		}
		if seen[sp.Path] {
			continue // 去重
		}
		seen[sp.Path] = true
		out = append(out, sp)
	}
	if err := s.SourcesApply(out); err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	if s.Self != nil {
		s.Self.Add("info", "ingest", "采集源配置已更新", "count", strconv.Itoa(len(out)))
	}
	s.audit(r, "sources.put", "", "ok", "count="+strconv.Itoa(len(out)))
	writeData(w, map[string]any{"items": s.sourcesSnapshot(), "paused": s.ingestPaused()})
}

// handleIngestPause / handleIngestResume —— 采集总开关（维护模式，FR-1107）。
// 语义：暂停「读取」（文件 + journald），接收端仍继续收并写入存储（不丢数据）。
func (s *Server) handleIngestPause(w http.ResponseWriter, r *http.Request) {
	s.setIngestPaused(w, r, true)
}

func (s *Server) handleIngestResume(w http.ResponseWriter, r *http.Request) {
	s.setIngestPaused(w, r, false)
}

func (s *Server) setIngestPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	if s.IngestPause == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "采集总开关未启用")
		return
	}
	s.IngestPause(paused)
	action := "ingest.resume"
	if paused {
		action = "ingest.pause"
	}
	s.audit(r, action, "", "ok", "")
	writeData(w, map[string]any{"paused": paused, "items": s.sourcesSnapshot()})
}
