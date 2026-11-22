// Package httpapi 提供管理界面与 HTTP API（仅监听回环，由平台 nginx 反代）。
//
// 响应信封统一：{"ok":bool,"data":...,"error":{"code","message"}}。
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Moechz/log-server/backend/internal/aggregate"
	"github.com/Moechz/log-server/backend/internal/alert"
	"github.com/Moechz/log-server/backend/internal/audit"
	"github.com/Moechz/log-server/backend/internal/auth"
	"github.com/Moechz/log-server/backend/internal/capability"
	"github.com/Moechz/log-server/backend/internal/config"
	"github.com/Moechz/log-server/backend/internal/forward"
	"github.com/Moechz/log-server/backend/internal/notify"
	"github.com/Moechz/log-server/backend/internal/onboarding"
	"github.com/Moechz/log-server/backend/internal/receive"
	"github.com/Moechz/log-server/backend/internal/selflog"
	"github.com/Moechz/log-server/backend/internal/store"
	"github.com/Moechz/log-server/backend/internal/version"
)

// Server 组装全部 HTTP 依赖。
type Server struct {
	Cfg   config.Config
	Caps  *capability.Registry
	Store store.Store
	Agg   *aggregate.Counters
	Self  *selflog.Log
	Audit *audit.Log
	// 以下钩子由 main 注入，避免 httpapi 依赖 receive/forward/alert 包的具体实现
	RxStats       func() map[string]any
	ReceivePolicy func() map[string]any      // 当前准入/限速策略
	ReceiveApply  func(receive.Policy) error // 保存并热生效
	IngestQueue   func() map[string]any      // 入库磁盘队列指标（D-030）
	ForwardStats  func() []forward.Status
	ForwardApply  func([]forward.Target) error
	AlertStats    func() map[string]any
	Alerts        func(n int) []alert.Alert
	Rules         func() []alert.Rule
	Sources       func() []map[string]any
	// SourcesApply / IngestPause（FR-102 / FR-1107）：采集源管理 + 采集总开关
	SourcesApply func([]SourceSpec) error
	IngestPause  func(paused bool)
	IngestPaused func() bool
	// 首次使用向导（FR-1005）
	OnboardingGet      func() onboarding.State
	OnboardingComplete func(actor string) error
	OnboardingReset    func() error
	// AlertEngine 用于 FR-804（确认/静默/维护窗口）；nil = 该功能不可用
	AlertEngine *alert.Engine
	// 通知通道（FR-803）；由 main 注入，httpapi 不直接依赖具体实现
	NotifyConfig func() []notify.Config
	NotifyApply  func([]notify.Config) error
	NotifyStatus func() []notify.Delivery
	NotifyTest   func(ctx context.Context, kind string) error
	// Auth 是会话鉴权器（复用 TOS 用户与会话，D-008）；nil = 不启用鉴权（仅测试）
	Auth *auth.Authenticator
	// EngineDir 是进程启动时实际生效的存储引擎目录（D-023 重启生效判定）
	EngineDir string
	WebRoot   string
	Started   time.Time
	Logger    *slog.Logger
}

// New 构造 Server。
func New(cfg config.Config, caps *capability.Registry, st store.Store,
	agg *aggregate.Counters, self *selflog.Log, webroot string, lg *slog.Logger) *Server {
	if lg == nil {
		// 防御：Logger 为空时退化为默认 logger，避免请求路径 panic
		lg = slog.Default()
	}
	if self == nil {
		self = selflog.New(16)
	}
	if agg == nil {
		agg = aggregate.New(24 * time.Hour)
	}
	return &Server{Cfg: cfg, Caps: caps, Store: st, Agg: agg, Self: self,
		WebRoot: webroot, Started: time.Now(), Logger: lg}
}

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/about", s.handleAbout)
	mux.HandleFunc("GET /api/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/network", s.handleNetwork)
	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /api/v1/logs", s.handleLogs)
	mux.HandleFunc("GET /api/v1/logs/histogram", s.handleLogsHistogram)
	mux.HandleFunc("GET /api/v1/logs/context", s.handleLogsContext)
	mux.HandleFunc("GET /api/v1/selflog", s.handleSelflog)
	mux.HandleFunc("GET /api/v1/selflog/download", s.handleSelflogDownload)
	mux.HandleFunc("GET /api/v1/diagnostics", s.handleDiagnostics)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	// 转发（M5）
	mux.HandleFunc("GET /api/v1/forward/targets", s.handleForwardList)
	mux.HandleFunc("PUT /api/v1/forward/targets", s.adminOnly(s.handleForwardPut))
	mux.HandleFunc("POST /api/v1/forward/targets/{id}/test", s.adminOnly(s.handleForwardTest))
	// 告警与规则（M8）
	mux.HandleFunc("GET /api/v1/alerts", s.handleAlerts)
	mux.HandleFunc("GET /api/v1/rules", s.handleRules)
	mux.HandleFunc("POST /api/v1/rules/dryrun", s.handleRulesDryRun)
	// 告警确认 / 静默 / 维护窗口（FR-804）
	mux.HandleFunc("POST /api/v1/alerts/{id}/ack", s.handleAlertAck)
	mux.HandleFunc("POST /api/v1/alerts/ack-all", s.handleAlertAckAll)
	mux.HandleFunc("GET /api/v1/alerts/silences", s.handleSilenceList)
	mux.HandleFunc("POST /api/v1/alerts/silences", s.handleSilenceCreate)
	mux.HandleFunc("DELETE /api/v1/alerts/silences/{id}", s.handleSilenceDelete)
	mux.HandleFunc("POST /api/v1/alerts/maintenance", s.handleMaintenanceSet)
	mux.HandleFunc("DELETE /api/v1/alerts/maintenance", s.handleMaintenanceClear)
	// 通知通道（FR-803）
	mux.HandleFunc("GET /api/v1/notify/channels", s.handleNotifyGet)
	mux.HandleFunc("PUT /api/v1/notify/channels", s.adminOnly(s.handleNotifyPut))
	mux.HandleFunc("POST /api/v1/notify/test", s.adminOnly(s.handleNotifyTest))
	// 接收准入与限速（FR-603/604）
	mux.HandleFunc("GET /api/v1/receive/policy", s.handleReceivePolicyGet)
	mux.HandleFunc("PUT /api/v1/receive/policy", s.adminOnly(s.handleReceivePolicyPut))
	// 采集源（M1）
	mux.HandleFunc("GET /api/v1/sources", s.handleSources)
	// 采集源管理（FR-102）+ 采集总开关（FR-1107）
	mux.HandleFunc("PUT /api/v1/sources", s.adminOnly(s.handleSourcesPut))
	mux.HandleFunc("POST /api/v1/ingest/pause", s.handleIngestPause)
	mux.HandleFunc("POST /api/v1/ingest/resume", s.handleIngestResume)
	// 容量（M3）
	mux.HandleFunc("GET /api/v1/storage/usage", s.handleStorageUsage)
	mux.HandleFunc("POST /api/v1/storage/estimate", s.handleStorageEstimate)
	// 存储位置（FR-307 / D-023，重启生效）
	mux.HandleFunc("GET /api/v1/storage/location", s.handleStorageLocation)
	mux.HandleFunc("PUT /api/v1/storage/location", s.adminOnly(s.handleStorageLocationPut))
	mux.HandleFunc("GET /api/v1/storage/browse", s.adminOnly(s.handleStorageBrowse))
	// 配置备份（FR-1002 / D-025）：导出与导入应用自管配置
	mux.HandleFunc("GET /api/v1/config/export", s.adminOnly(s.handleConfigExport))
	mux.HandleFunc("POST /api/v1/config/import", s.adminOnly(s.handleConfigImport))

	// 会话（前端用于展示当前用户与角色）
	mux.HandleFunc("GET /api/v1/session", s.handleSession)
	// 首次管理员认领（只允许一次；之后用 admin.token 解锁）
	mux.HandleFunc("POST /api/v1/admin/claim", s.handleAdminClaim)
	// 管理操作留痕（FR-1104）：仅管理员可读
	mux.HandleFunc("GET /api/v1/audit/actions", s.adminOnly(s.handleAuditActions))
	// 首次使用向导（FR-1005）
	mux.HandleFunc("GET /api/v1/onboarding", s.handleOnboarding)
	mux.HandleFunc("POST /api/v1/onboarding/complete", s.adminOnly(s.handleOnboardingComplete))
	mux.HandleFunc("POST /api/v1/onboarding/reset", s.adminOnly(s.handleOnboardingReset))

	if root := s.resolveWebRoot(); root != "" {
		// ★ 升级后浏览器/平台可能缓存旧的 JS 模块（ES module 尤其明显）→ 强制每次重验证。
		//   no-cache 不等于不缓存：浏览器仍会带 If-Modified-Since 询问，未变则 304，变了立即拿新的。
		fs := http.FileServer(http.Dir(root))
		mux.Handle("/", revalidate(fs))
		s.Logger.Info("已挂载前端静态资源", "root", root)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "log-server-for-tos: 前端资源未找到（webui 未解压或未打包）")
		})
	}
	return logMiddleware(s.Logger, s.withAuth(mux))
}

// withAuth 对非豁免路径强制校验会话（复用 TOS 会话，D-008 / 官方文档 §8.10）。
// 未通过时返回 401，前端据此跳回 `/` 触发 TOS 重新登录。
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.Auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.ShouldExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		u, ok := s.Auth.Authenticate(r)
		if !ok {
			s.Logger.Warn("拒绝未认证请求", "path", r.URL.Path, "remote", r.RemoteAddr)
			auth.Unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	})
}

// adminOnly 要求当前请求是管理员（本地令牌解锁或显式 admins=*）。
// Auth 未注入（单测/本地直接构造 Server）时保持旧行为放行，避免测试与嵌入式用法被误伤。
func (s *Server) adminOnly(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Auth == nil {
			fn(w, r)
			return
		}
		u, ok := auth.FromContext(r.Context())
		if !ok || !u.IsAdmin {
			s.audit(r, "admin.denied", r.URL.Path, "denied", "当前会话不是管理员")
			writeErr(w, http.StatusForbidden, "admin_required",
				"需要管理员权限：请在「设置」页输入管理员令牌（数据目录 config/admin.token）")
			return
		}
		fn(w, r)
	}
}

// resolveWebRoot 解析前端资源目录。
//
// 顺序：显式 -webroot → 可执行文件旁（<exe>/../webui）→ 传统安装路径 → 开发期源码目录。
// ★ 不再硬编码 appid：App Center 安装时 /usr/local/<appid> 是指向数据卷的**符号链接**，
// 而 /proc/self/exe 会解析成真实路径（/Volume<N>/@apps/<appid>/bin/<bin>），
// 因此 exe 相对路径在两种安装布局下都正确，改 appid 也不会失效（F12 的教训）。
func (s *Server) resolveWebRoot() string {
	var cands []string
	if s.WebRoot != "" {
		cands = append(cands, s.WebRoot)
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "..", "webui"))
	}
	cands = append(cands,
		"/usr/local/log-server-for-tos/webui",
		"../frontend", "../../frontend", "./frontend")
	for _, c := range cands {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// ── 各端点 ────────────────────────────────────────────────────────────

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	// 健康检查：进程活着即 200（存储不可用由 /api/v1/status 体现，避免被误重启）
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleAbout(w http.ResponseWriter, _ *http.Request) {
	info := version.Info()
	writeData(w, map[string]any{
		"name":      "log server for TOS",
		"appid":     version.AppID,
		"version":   info["version"],
		"build":     info["build"],
		"commit":    info["commit"],
		"arch":      info["arch"],
		"goversion": info["goversion"],
		"tos_min":   "TOS7.0",
		"publisher": "Moechz",
		"repo":      "https://github.com/Moechz/log-server",
		"data_dir":  s.Cfg.DataDir,
		"engine":    engineName(s.Store),
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeData(w, map[string]any{"items": s.Caps.Snapshot()})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	snap := s.Caps.Snapshot()
	bad := 0
	for _, c := range snap {
		if !c.Available() {
			bad++
		}
	}
	level := "ok"
	switch {
	case bad == 0:
		level = "ok"
	case bad == len(snap):
		level = "crit"
	default:
		level = "warn"
	}

	storeOK, storeErr := s.storeHealth()
	resp := map[string]any{
		"level":       level,
		"uptime_sec":  int64(time.Since(s.Started).Seconds()),
		"version":     version.Version,
		"caps_total":  len(snap),
		"caps_bad":    bad,
		"store_ok":    storeOK,
		"store_error": storeErr,
		"engine":      engineName(s.Store),
		"auth":        s.authInfo(),
	}
	if s.Audit != nil {
		resp["audit"] = s.Audit.Stats()
	}
	if s.IngestQueue != nil {
		resp["ingest_queue"] = s.IngestQueue()
	}
	writeData(w, resp)
}

func (s *Server) handleOverview(w http.ResponseWriter, _ *http.Request) {
	// ★ 总览只读聚合计数器与缓存过的用量，**绝不扫原始日志**（FR-1204）
	since := time.Now().UTC().Add(-24 * time.Hour)
	totals := s.Agg.Totals(since)
	series := s.Agg.Series(since)

	buckets := make([]map[string]any, 0, len(series))
	for t, c := range series {
		buckets = append(buckets, map[string]any{"bucket": t.Format(time.RFC3339), "count": c})
	}

	var usage store.Usage
	if s.Store != nil {
		if u, err := s.Store.Usage(contextTODO()); err == nil {
			usage = u
		}
	}

	writeData(w, map[string]any{
		"generated_at":  time.Now().UTC(),
		"uptime_sec":    int64(time.Since(s.Started).Seconds()),
		"version":       version.Version,
		"resource_mode": string(s.Cfg.ResourceMode),
		"counters": map[string]any{
			"total":       totals.Total,
			"bytes":       totals.Bytes,
			"by_class":    totals.ByClass,
			"by_severity": totals.BySeverity,
			"missed":      totals.Missed,
			"buckets":     len(series),
		},
		"series": buckets,
		"top": map[string]any{
			"host":        s.Agg.TopN(since, "host", 5),
			"app":         s.Agg.TopN(since, "app", 5),
			"source_kind": s.Agg.TopN(since, "source_kind", 5),
		},
		"storage": map[string]any{
			"engine":         engineName(s.Store),
			"total_bytes":    usage.TotalBytes,
			"disk_total":     usage.DiskTotalBytes,
			"disk_free":      usage.DiskFreeBytes,
			"watermark_pct":  usage.WatermarkPct,
			"retention_days": s.Cfg.RetentionDays,
			"max_disk_bytes": s.Cfg.MaxDiskBytes,
		},
		"forward":      s.forwardSnapshot(),
		"receivers":    s.receiverSnapshot(),
		"rx":           s.rxStats(),
		"alerts":       s.alertSnapshot(),
		"sources":      s.sourcesSnapshot(),
		"audit":        s.auditSnapshot(),
		"self":         map[string]any{"log": s.Self.Stats()},
		"capabilities": s.Caps.Snapshot(),
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage_unavailable", "存储引擎不可用")
		return
	}
	sq := logQueryFromRequest(r, 0)
	res, err := s.Store.Query(r.Context(), sq)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "store_error", err.Error())
		return
	}
	writeData(w, res)
}

func (s *Server) handleSelflog(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 {
		n = 100
	}
	writeData(w, map[string]any{
		"stats": s.Self.Stats(),
		"file":  s.Self.FileStats(),
		"items": s.Self.Recent(n),
	})
}

// handleSelflogDownload 下载自身日志文件（tail：默认最近 1000 行；?lines=N 可调）。
// 目的：出问题时用户能把应用自身日志取出来（此前只有内存 2000 条且重启即丢）。
func (s *Server) handleSelflogDownload(w http.ResponseWriter, r *http.Request) {
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if lines <= 0 || lines > 20000 {
		lines = 1000
	}
	st := s.Self.FileStats()
	if st.Path == "" {
		writeErr(w, http.StatusNotFound, "no_file", "自身日志未启用落盘（检查 <DataDir>/logs 是否可写）")
		return
	}
	f, err := os.Open(st.Path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no_file", "自身日志文件不存在: "+err.Error())
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition",
		`attachment; filename="log-server-for-tos-selflog-`+time.Now().Format("20060102-150405")+`.log"`)
	if err := selflog.TailLines(f, lines, w); err != nil {
		s.Logger.Warn("自身日志下载中断", "err", err)
	}
}

// handleMetrics 暴露 Prometheus 文本格式（FR-1105）。
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	since := time.Now().UTC().Add(-24 * time.Hour)
	t := s.Agg.Totals(since)
	var b strings.Builder
	line := func(name, help, typ string, val float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, typ, name, val)
	}
	line("logserver_ingest_total_24h", "近 24h 接收日志条数", "counter", float64(t.Total))
	line("logserver_ingest_bytes_24h", "近 24h 接收字节数", "gauge", float64(t.Bytes))
	line("logserver_counter_missed", "因窗口淘汰而丢弃的计数", "counter", float64(t.Missed))
	line("logserver_selflog_dropped", "自身日志因容量上限丢弃条数", "counter", float64(s.Self.Stats().Dropped))
	if rx := s.rxStats(); len(rx) > 0 {
		line("logserver_receive_acl_denied", "来源准入拒绝条数", "counter", numOf(rx["acl_denied"]))
		line("logserver_receive_rate_limited", "每来源限速拒绝条数", "counter", numOf(rx["rate_limited"]))
	}
	if s.Audit != nil {
		as := s.Audit.Stats()
		line("logserver_audit_actions_total", "管理操作留痕累计写入条数（进程启动以来）", "counter", float64(as.Entries))
		line("logserver_audit_write_errors", "管理操作留痕写入失败次数", "counter", float64(as.WriteErrors))
	}
	if s.IngestQueue != nil {
		q := s.IngestQueue()
		line("logserver_ingest_queue_depth", "入库磁盘队列待写条数", "gauge", numOf(q["depth"]))
		line("logserver_ingest_queue_bytes", "入库磁盘队列占用字节", "gauge", numOf(q["bytes"]))
		line("logserver_ingest_queue_oldest_sec", "入库队列最老记录停留秒数", "gauge", numOf(q["oldest_age_sec"]))
		line("logserver_ingest_queue_pushed", "入库队列累计入队条数（进程启动以来）", "counter", numOf(q["pushed"]))
		line("logserver_ingest_queue_acked", "入库队列累计确认（已写入存储）条数（进程启动以来）", "counter", numOf(q["acked"]))
		line("logserver_ingest_queue_retried", "入库队列写失败重试次数", "counter", numOf(q["retried"]))
		line("logserver_ingest_queue_dropped", "入库队列因上限/写入失败丢弃条数", "counter", numOf(q["dropped"]))
		line("logserver_ingest_queue_write_errors", "入库队列写存储失败批次计数", "counter", numOf(q["write_errors"]))
	}
	if u, err := s.Store.Usage(contextTODO()); err == nil {
		line("logserver_storage_bytes", "日志存储占用字节", "gauge", float64(u.TotalBytes))
		line("logserver_disk_watermark_pct", "磁盘水位百分比", "gauge", u.WatermarkPct)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// ── 辅助 ──────────────────────────────────────────────────────────────

// numOf 把队列快照里的数值统一成 float64（供 /metrics 输出）。
func numOf(v any) float64 {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case float64:
		return x
	default:
		return 0
	}
}

func (s *Server) rxStats() map[string]any {
	if s.RxStats == nil {
		return map[string]any{}
	}
	return s.RxStats()
}

func (s *Server) storeHealth() (bool, string) {
	if s.Store == nil {
		return false, "未配置存储引擎"
	}
	if err := s.Store.Ping(contextTODO()); err != nil {
		return false, err.Error()
	}
	return true, ""
}

func (s *Server) receiverSnapshot() []map[string]any {
	if s.RxStats != nil {
		if v, ok := s.RxStats()["listeners"].([]map[string]any); ok && len(v) > 0 {
			return v
		}
	}
	return []map[string]any{
		{"protocol": "udp", "port": s.Cfg.SyslogUDPPort, "enabled": true},
		{"protocol": "tcp", "port": s.Cfg.SyslogTCPPort, "enabled": true},
		{"protocol": "tls", "port": s.Cfg.SyslogTLSPort, "enabled": false,
			"note": "需配置证书；且需平台 capability 绑定低端口"},
		{"protocol": "snmp_trap", "port": s.Cfg.SNMPTrapPort, "enabled": false, "note": "P1"},
	}
}

func (s *Server) auditSnapshot() map[string]any {
	st, _ := s.Caps.Get(capability.CapAudit)
	return map[string]any{
		"mode":    st.State,
		"reason":  st.Reason,
		"lost":    0,
		"tier":    "",
		"verify":  "not_run",
		"records": 0,
		"oldest":  nil,
	}
}

func engineName(st store.Store) string {
	if st == nil {
		return "none"
	}
	return st.Capabilities().Engine
}

// writeData 写成功信封。
func writeData(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": data})
}

// writeErr 写错误信封。
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"ok": false, "error": map[string]any{"code": code, "message": msg},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// revalidate 给静态资源加缓存重验证头：浏览器/平台不会再用旧 JS，但未变化的文件仍走 304。
// 修复：升级后“看不见改动”多因 ES module 被缓存。
func revalidate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

func logMiddleware(lg *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		lg.Debug("http", "method", r.Method, "path", r.URL.Path,
			"took_ms", time.Since(start).Milliseconds())
	})
}
