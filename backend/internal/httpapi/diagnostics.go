// 自助诊断包（FR-1003 / FR-1108 / NFR-S6）。
//
// 设计要点：
//   - **自动脱敏**：凭据类字段名一律替换为 ***；字符串里的 token/私钥/Bearer 与 URL 中的
//     用户信息（user:pass@）也一并脱敏 —— 这是 NFR-S6 的硬要求（F10 驳回理由同源）；
//   - 内容以"客服/开发者能自助定位问题"为准：版本、配置摘要、健康、能力探测、最近错误；
//   - 不含任何密钥/证书**内容**（只报告"是否已配置"）。
package httpapi

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"time"

	"github.com/Moechz/log-server/backend/internal/encoding"
	"github.com/Moechz/log-server/backend/internal/selflog"
	"github.com/Moechz/log-server/backend/internal/version"
)

// Masked 是脱敏后的占位符。
const Masked = "***"

var (
	// secretKeyRe 命中即整体替换（键名语义优先）。
	secretKeyRe = regexp.MustCompile(`(?i)(pass|passwd|secret|token|credential|apikey|api_key|private_key|key_file|cert_file|pinned_sha256|dsn)`)
	// secretValueRe 命中字符串里的凭据样式（键名看不出来时的兜底）。
	secretValueRe = regexp.MustCompile(`(ghp_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|Bearer\s+[A-Za-z0-9._~+/-]{20,})`)
	// urlUserInfoRe 命中 URL 里的 user:pass@（保留主机，去掉凭据）。
	urlUserInfoRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
)

// handleDiagnostics 生成并返回诊断包（JSON）。`?download=1` 时带下载文件名。
func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	pkg := s.buildDiagnostics()
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition",
			`attachment; filename="logserver-diagnostics-`+time.Now().UTC().Format("20060102-150405")+`.json"`)
	}
	writeData(w, pkg)
}

// Diagnostics 是诊断包结构（导出给测试与文档使用）。
type Diagnostics struct {
	GeneratedAt  string            `json:"generated_at"`
	Version      map[string]string `json:"version"`
	Runtime      map[string]any    `json:"runtime"`
	Health       map[string]any    `json:"health"`
	Ingest       map[string]any    `json:"ingest"`
	Storage      map[string]any    `json:"storage"`
	Capabilities []map[string]any  `json:"capabilities"`
	Sources      []map[string]any  `json:"sources"`
	Forward      []map[string]any  `json:"forward"`
	Auth         map[string]any    `json:"auth"`
	Audit        map[string]any    `json:"audit,omitempty"`
	SelfLog      map[string]any    `json:"selflog"`
	Config       map[string]any    `json:"config"`
	Masked       []string          `json:"masked_fields"`
}

func (s *Server) buildDiagnostics() Diagnostics {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	storeOK, storeErr := s.storeHealth()
	d := Diagnostics{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Version:     version.Info(),
		Runtime: map[string]any{
			"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
			"goroutines":   runtime.NumGoroutine(),
			"mem_alloc_mb": float64(ms.Alloc) / (1 << 20),
			"uptime_sec":   int64(time.Since(s.Started).Seconds()),
		},
		Health: map[string]any{
			"store_ok": storeOK, "store_error": storeErr, "engine": engineName(s.Store),
		},
		Capabilities: capSnapshots(s),
		Auth:         s.authInfo(),
		Config:       s.configSummary(),
		Masked: []string{
			"键名含 pass/secret/token/key/dsn 的字段", "URL 中的 user:pass@", "ghp_/github_pat_/PRIVATE KEY/Bearer 样式的字符串",
		},
	}

	if s.Audit != nil {
		d.Audit = map[string]any{"stats": s.Audit.Stats()}
	}
	d.Ingest = map[string]any{}
	if s.RxStats != nil {
		if m, ok := maskMap(s.RxStats()).(map[string]any); ok {
			d.Ingest = m
		}
	}
	if s.Agg != nil {
		t := s.Agg.Totals(time.Now().UTC().Add(-24 * time.Hour))
		d.Ingest["totals_24h"] = map[string]any{
			"total": t.Total, "bytes": t.Bytes, "missed": t.Missed,
		}
	}
	if s.Store != nil {
		if u, err := s.Store.Usage(contextTODO()); err == nil {
			d.Storage = map[string]any{
				"total_bytes": u.TotalBytes, "record_count": u.RecordCount,
				"oldest_event":     u.OldestEvent.UTC().Format(time.RFC3339),
				"newest_event":     u.NewestEvent.UTC().Format(time.RFC3339),
				"disk_total_bytes": u.DiskTotalBytes, "disk_free_bytes": u.DiskFreeBytes,
				"watermark_pct": u.WatermarkPct,
			}
		} else {
			d.Storage = map[string]any{"error": err.Error()}
		}
		if m, ok := maskMap(d.Storage).(map[string]any); ok {
			d.Storage = m
		}
	}
	// 采集源（含编码声明，便于排查乱码问题）
	if srcs := s.sourcesSnapshot(); len(srcs) > 0 {
		d.Sources = maskSlice(srcs)
	}
	// 转发目标（addr 可能含 user:pass@，必须脱敏）
	if fw := s.forwardConfig(); len(fw) > 0 {
		d.Forward = maskSlice(fw)
	}
	// 自身日志：完整统计 + 最近错误/告警（FR-1108）
	if s.Self != nil {
		items := []selflog.Entry{}
		for _, e := range s.Self.Recent(0) {
			if e.Level == "warn" || e.Level == "error" {
				items = append(items, e)
			}
		}
		if len(items) > 200 {
			items = items[len(items)-200:]
		}
		d.SelfLog = map[string]any{"stats": s.Self.Stats(), "recent_issues": items}
		if m, ok := maskMap(d.SelfLog).(map[string]any); ok {
			d.SelfLog = m
		}
	}
	return d
}

// configSummary 是配置摘要（白名单式，绝不整包 dump）。
func (s *Server) configSummary() map[string]any {
	c := s.Cfg
	if m, ok := maskMap(map[string]any{
		"resource_mode":       string(c.ResourceMode),
		"data_dir":            c.DataDir,
		"config_dir":          c.ConfigDir,
		"http_addr":           c.HTTPAddr,
		"http_port":           c.HTTPPort,
		"syslog_udp_port":     c.SyslogUDPPort,
		"syslog_tcp_port":     c.SyslogTCPPort,
		"syslog_tls_port":     c.SyslogTLSPort,
		"tls_cert_configured": c.TLSCertFile != "",
		"tls_key_configured":  c.TLSKeyFile != "",
		"vlogs_addr":          c.VLogsAddr,
		"vlogs_port":          c.VLogsPort,
		"vlogs_storage":       c.VLogsStorage,
		"queue_max_bytes":     c.QueueMax,
		"encoding_converter":  encodingConverterState(),
	}).(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func encodingConverterState() string {
	if encoding.ConverterAvailable() {
		return "iconv 可用（GB18030/Big5/EUC-KR/Shift_JIS 可自动转码）"
	}
	return "iconv 不可用（多字节编码仅保留原文，不转换）"
}

func capSnapshots(s *Server) []map[string]any {
	if s.Caps == nil {
		return nil
	}
	out := make([]map[string]any, 0)
	for _, c := range s.Caps.Snapshot() {
		out = append(out, map[string]any{
			"id": c.ID, "title": c.Title, "available": c.Available(),
			"reason": c.Reason, "evidence": c.Evidence, "min_tos": c.MinTOS,
			"checked_at": c.CheckedAt.UTC().Format(time.RFC3339),
		})
	}
	return out
}

// ── 脱敏 ────────────────────────────────────────────────────────────

// maskMap 递归脱敏 map（导出给测试）。
func maskMap(v any) any { return maskAny(v) }

func maskSlice(v any) []map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return []map[string]any{{"error": "序列化失败: " + err.Error()}}
	}
	var raw []any
	if err := json.Unmarshal(b, &raw); err != nil {
		return []map[string]any{{"error": "反序列化失败: " + err.Error()}}
	}
	masked := maskAny(raw)
	out := make([]map[string]any, 0, len(raw))
	if list, ok := masked.([]any); ok {
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// maskAny 递归脱敏：键名语义优先，字符串内容兜底（URL 用户信息 + 凭据样式）。
func maskAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretKeyRe.MatchString(k) {
				out[k] = Masked
				continue
			}
			out[k] = maskAny(val)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if secretKeyRe.MatchString(k) {
				out[k] = Masked
				continue
			}
			out[k] = maskString(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, maskAny(e))
		}
		return out
	case string:
		return maskString(t)
	default:
		// 结构体 / 具名切片 / 具名 map / 指针：先转成通用 JSON 结构再脱敏，
		// 否则会漏掉嵌套在结构体里的 map[string]string（如 selflog.Entry.Fields）。
		switch reflect.ValueOf(v).Kind() {
		case reflect.Struct, reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
			if b, err := json.Marshal(v); err == nil {
				var m any
				if json.Unmarshal(b, &m) == nil {
					return maskAny(m)
				}
			}
		}
		return v
	}
}

func maskString(s string) string {
	s = secretValueRe.ReplaceAllString(s, Masked)
	return urlUserInfoRe.ReplaceAllString(s, "$1"+Masked+"@")
}
