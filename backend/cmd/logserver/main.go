// Command logserver 是 TOS 应用「日志服务器」的主程序。
//
// 装配顺序：配置 → 能力探测 → 存储适配 → 队列/转发 → 告警 → 采集与接收 → HTTP。
// 本文件只做装配与生命周期，业务逻辑都在 internal 各包。
//
// 设计约束（AGENTS.md §6 / D-017）：
//   - 不做任何系统级集成（不装包、不改 rsyslog/journald、不加系统组、不改防火墙）
//   - 只监听回环；能力未就绪时如实报告，不绕过
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Moechz/log-server/backend/internal/aggregate"
	"github.com/Moechz/log-server/backend/internal/alert"
	"github.com/Moechz/log-server/backend/internal/audit"
	"github.com/Moechz/log-server/backend/internal/auth"
	"github.com/Moechz/log-server/backend/internal/capability"
	appconfig "github.com/Moechz/log-server/backend/internal/config"
	"github.com/Moechz/log-server/backend/internal/forward"
	"github.com/Moechz/log-server/backend/internal/httpapi"
	fileingest "github.com/Moechz/log-server/backend/internal/ingest/file"
	"github.com/Moechz/log-server/backend/internal/ingest/spool"
	"github.com/Moechz/log-server/backend/internal/model"
	"github.com/Moechz/log-server/backend/internal/notify"
	"github.com/Moechz/log-server/backend/internal/onboarding"
	"github.com/Moechz/log-server/backend/internal/pipeline"
	"github.com/Moechz/log-server/backend/internal/receive"
	"github.com/Moechz/log-server/backend/internal/selflog"
	"github.com/Moechz/log-server/backend/internal/storageloc"
	"github.com/Moechz/log-server/backend/internal/store"
	"github.com/Moechz/log-server/backend/internal/store/vlstore"
	"github.com/Moechz/log-server/backend/internal/version"
)

const (
	defaultEnvFile = "/usr/local/log-server-for-tos/log-server-for-tos.env"
	capInterval    = 60 * time.Second
	aggWindow      = 24 * time.Hour
)

func main() {
	var (
		envFile         = flag.String("env", defaultEnvFile, "运行时配置（env 文件）路径")
		jsonFile        = flag.String("config", "", "config.json 路径（留空则取自数据目录）")
		webRoot         = flag.String("webroot", "", "前端资源目录（留空自动探测）")
		logLevel        = flag.String("log-level", "info", "日志级别: debug|info|warn|error")
		showVer         = flag.Bool("version", false, "打印版本后退出")
		printEnginePath = flag.Bool("print-engine-path", false, "打印当前生效的存储引擎目录后退出（供引擎服务启动时解析，D-023）")
	)
	flag.Parse()

	if *showVer {
		for k, v := range version.Info() {
			fmt.Printf("%-10s %s\n", k+":", v)
		}
		return
	}

	// 供独立引擎服务在启动时解析存储位置（不启动任何服务、不写任何文件）
	if *printEnginePath {
		cfg, err := appconfig.Load(*envFile, *jsonFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "配置加载失败:", err)
			os.Exit(1)
		}
		if cfg.ConfigDir == "" {
			cfg.ConfigDir = filepath.Join(cfg.DataDir, "config")
		}
		fmt.Println(storageloc.Load(storageloc.ConfigFile(cfg.DataDir), cfg.VLogsStorage))
		return
	}

	lg := newLogger(*logLevel)
	self := selflog.New(2000)
	lg.Info("log-server-for-tos 启动", "version", version.Version, "arch", version.Info()["arch"])
	self.Add("info", "main", "log-server-for-tos 启动", "version", version.Version)

	cfg, err := appconfig.Load(*envFile, *jsonFile)
	if err != nil {
		lg.Error("配置加载失败", "err", err)
		self.Add("error", "config", "配置加载失败", "err", err.Error())
		os.Exit(1)
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = filepath.Join(cfg.DataDir, "config")
	}
	cfgPath := filepath.Join(cfg.ConfigDir, "config.json")
	if *jsonFile != "" {
		cfgPath = *jsonFile
	}
	// 存储位置（FR-307 / D-023）：
	//   用户可在「存储」页选择引擎目录，保存在 <DataDir>/config/storage.json；
	//   独立引擎服务用 `--print-engine-path` 解析同一份配置 → 重启后两个进程都落到新目录。
	engineDir := storageloc.Load(storageloc.ConfigFile(cfg.DataDir), cfg.VLogsStorage)
	lg.Info("配置已加载",
		"data_dir", cfg.DataDir,
		"engine_dir", engineDir,
		"http", fmt.Sprintf("%s:%d", cfg.HTTPAddr, cfg.HTTPPort),
		"syslog", fmt.Sprintf("udp/%d tcp/%d tls/%d", cfg.SyslogUDPPort, cfg.SyslogTCPPort, cfg.SyslogTLSPort),
		"mode", string(cfg.ResourceMode), "config", cfgPath)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// ── 能力探测（D-014 / D-017）──────────────────────────────────────
	caps := capability.New()
	caps.Start(ctx, capInterval)
	logCaps(lg, self, caps.Snapshot())

	// ── 存储适配层（FR-312）──────────────────────────────────────────
	var st store.Store = vlstore.New()
	scfg := store.Config{
		Engine:      "victorialogs",
		Addr:        fmt.Sprintf("%s:%d", cfg.VLogsAddr, cfg.VLogsPort),
		StoragePath: engineDir,
		Timeout:     15 * time.Second,
		LowResource: cfg.ResourceMode == appconfig.ModeLow,
	}
	if err := st.Open(ctx, scfg); err != nil {
		lg.Error("存储适配器初始化失败", "err", err)
		self.Add("error", "store", "存储适配器初始化失败", "err", err.Error())
		os.Exit(1)
	}
	if err := st.Ping(ctx); err != nil {
		lg.Warn("存储引擎暂不可用（应用继续启动，界面会提示）", "err", err, "addr", scfg.Addr)
		self.Add("warn", "store", "存储引擎暂不可用", "addr", scfg.Addr, "err", err.Error())
	} else {
		lg.Info("存储引擎就绪", "engine", st.Capabilities().Engine, "addr", scfg.Addr)
	}

	// ── 用户配置（sources / forward_targets / alert_rules）───────────
	// 应用自身日志落盘（<DataDir>/logs；兑现「存储」页 self-7d 的 200 MB / 7 天承诺）。
	// 目录不可写时**降级为仅内存**并如实记录——自身日志的落盘绝不应阻断主进程启动。
	selfSink, sinkErr := selflog.OpenFileSink(selflog.FileOptions{Dir: filepath.Join(cfg.DataDir, "logs")})
	if sinkErr != nil {
		lg.Warn("自身日志落盘不可用，降级为仅内存", "err", sinkErr)
	} else {
		self.SetFileSink(selfSink)
		defer func() { _ = selfSink.Close() }()
		self.Add("info", "main", "自身日志已落盘", "path", selfSink.Path(),
			"max_total_mb", "200", "retention_days", "7")
	}

	// 管理操作留痕（FR-1104）：独立于日志存储，引擎故障/日志轮转都不影响。
	var auditLog *audit.Log
	if al, aerr := audit.Open(audit.Options{Dir: filepath.Join(cfg.DataDir, "audit")}); aerr != nil {
		lg.Warn("管理操作留痕不可用（审计页将提示；不影响业务）", "err", aerr)
		self.Add("warn", "audit", "管理操作留痕不可用", "err", aerr.Error())
	} else {
		auditLog = al
		defer func() { _ = auditLog.Close() }()
		lg.Info("管理操作留痕已启用", "path", auditLog.Path())
		self.Add("info", "audit", "管理操作留痕已启用", "path", auditLog.Path())
	}

	acfg := loadAppConfig(cfgPath, lg)

	// ── 聚合计数器（总览唯一数据源，FR-1105）────────────────────────
	agg := aggregate.New(aggWindow)

	// ── 转发管理器（M5）────────────────────────────────────────────
	fwd := forward.NewManager(forward.Config{
		QueueRoot: filepath.Join(cfg.DataDir, "queue", "forward"),
		MaxBytes:  cfg.QueueMax,
		Timeout:   10 * time.Second,
		Logger:    lg,
		OnSelfLog: func(level, msg string, kv ...string) {
			self.Add(level, "forward", msg, kv...)
			lg.Warn(msg, kvToArgs(kv)...)
		},
	})
	if err := fwd.Apply(acfg.ForwardTargets); err != nil {
		lg.Error("转发目标配置有误（其余功能继续）", "err", err)
		self.Add("error", "forward", "转发目标配置有误", "err", err.Error())
	}
	fwd.Start(ctx)
	defer fwd.Close()

	// ── 通知通道（FR-803）：TOS 通知中心 / SMTP / Webhook ─────────
	nd := notify.New(lg)
	if warns := nd.Apply(acfg.Notifications); len(warns) > 0 {
		for _, wm := range warns {
			lg.Warn("通知通道配置有问题", "err", wm)
			self.Add("warn", "notify", "通知通道配置有问题", "err", wm)
		}
	}
	silencePath := filepath.Join(cfg.ConfigDir, "alert_silences.json")

	// ── 告警规则引擎（M8）────────────────────────────────────────
	eng := alert.New(alert.Config{
		RecentMax: 500,
		OnAlert: func(a alert.Alert) {
			lg.Warn("告警触发", "rule", a.RuleID, "level", a.LevelText,
				"host", a.Host, "summary", a.Summary)
			self.Add("warn", "alert", a.Summary,
				"rule", a.RuleID, "level", a.LevelText, "host", a.Host)
			// FR-803：分发到通知通道（各通道独立失败，不阻塞告警）
			nd.Send(notify.Message{
				Title: a.Summary, Level: int(a.Level), LevelText: a.LevelText,
				Category: a.Category, Host: a.Host, App: a.App, RuleID: a.RuleID, At: a.At,
				Body: "规则：" + a.RuleName + "\n样例：" + a.SampleRaw,
			})
		},
		// FR-804：静默/维护窗口变化 → 落盘，重启不丢
		OnSilenceChange: func(list []alert.Silence) {
			if err := saveSilences(silencePath, list); err != nil {
				lg.Warn("保存告警静默失败", "err", err)
			}
		},
	})
	if list, err := loadSilences(silencePath); err == nil && len(list) > 0 {
		eng.RestoreSilences(list)
		lg.Info("已恢复告警静默/维护窗口", "count", len(list))
	}
	rules := acfg.AlertRules
	if len(rules) == 0 {
		rules = alert.BuiltinRules()
	}
	if err := eng.Add(rules...); err != nil {
		lg.Error("告警规则加载失败", "err", err)
		self.Add("error", "alert", "告警规则加载失败", "err", err.Error())
	}
	lg.Info("告警规则已加载", "count", len(eng.Rules()))

	// ── 处理管线 + 入库磁盘队列（D-030：先落盘，再批量写存储）─────────
	proc := pipeline.New(pipeline.DefaultConfig())
	ingestDir := cfg.IngestQueueDir
	if ingestDir == "" {
		ingestDir = filepath.Join(cfg.DataDir, "queue", "ingest")
	}
	sp, err := spool.Open(spool.Config{
		Dir:      ingestDir,
		MaxBytes: cfg.IngestQueueMax,
		Store:    st,
		Logger:   lg,
		SelfLog: func(level, module, msg string, kv ...string) {
			self.Add(level, module, msg, kv...)
		},
	})
	if err != nil {
		lg.Error("入库队列初始化失败（数据目录不可写）", "err", err, "dir", ingestDir)
		self.Add("error", "spool", "入库队列初始化失败", "err", err.Error())
		os.Exit(1)
	}
	sp.Start(ctx)
	var deliverDropped atomic.Uint64

	// 队列快照（总览 / 接收页 / 状态 / 诊断包共用同一份口径）
	ingestQueueSnapshot := func() map[string]any {
		sps := sp.Stats()
		return map[string]any{
			"dir": sp.Dir(), "depth": sps.Depth, "bytes": sps.Bytes,
			"max_bytes": sps.MaxBytes, "oldest_age_sec": sps.OldestAgeSec,
			"pushed": sps.Pushed, "acked": sps.Acked, "retried": sps.Retried,
			"dropped": sps.Dropped + deliverDropped.Load(), "corrupt": sps.Corrupt,
			"write_errors": sps.WriteErrors, "segments": sps.Segments,
			"last_err": sps.LastErr, "closed": sps.Closed,
		}
	}

	// 统一的投递入口：扇出（聚合/转发/告警）+ **同步落盘**。
	// 落盘在来源线程完成，但不等存储引擎 —— 存储故障由 spool 的写循环退避重试。
	deliver := func(r *model.Record) {
		agg.Add(r)
		fwd.Enqueue(r)
		eng.Evaluate(r)
		if err := sp.Enqueue(r); err != nil {
			n := deliverDropped.Add(1)
			if n%1000 == 1 {
				self.Add("warn", "spool", "入库队列写入失败/已满，已丢弃消息",
					"err", err.Error(), "dropped", fmt.Sprint(n))
			}
		}
	}

	// 接收面准入与限速（FR-603/604）：env 为默认，config.json 的 UI 设置覆盖之。
	recvAllow, recvDeny, recvEPS, recvBPS := cfg.ReceiveAllow, cfg.ReceiveDeny, cfg.ReceiveMaxEPS, cfg.ReceiveMaxBPS
	if acfg.Receive != nil {
		recvAllow, recvDeny = acfg.Receive.Allow, acfg.Receive.Deny
		recvEPS, recvBPS = acfg.Receive.MaxEPS, acfg.Receive.MaxBytesPerSec
	}
	recvPolicy, perr := receive.ParsePolicy(recvAllow, recvDeny, recvEPS, recvBPS)
	if perr != nil {
		lg.Warn("接收准入/限速配置无效，已回退为默认（全部放行、不限速）", "err", perr)
		self.Add("warn", "receive", "接收准入/限速配置无效，已回退默认", "err", perr.Error())
		recvPolicy = receive.Policy{}
	}

	// ── 接收监听器（UDP/TCP syslog）─────────────────────────────────
	rcv := receive.New(receive.Options{
		UDPAddr:     fmt.Sprintf("0.0.0.0:%d", cfg.SyslogUDPPort),
		TCPAddr:     fmt.Sprintf("0.0.0.0:%d", cfg.SyslogTCPPort),
		TLSAddr:     fmt.Sprintf("0.0.0.0:%d", cfg.SyslogTLSPort),
		TLSCertFile: cfg.TLSCertFile,
		TLSKeyFile:  cfg.TLSKeyFile,
		Policy:      recvPolicy,
		Logger:      lg,
		Handler: func(raw []byte, src receive.SourceInfo) {
			deliver(proc.Process(raw, src, time.Now().UTC()))
		},
	})
	if err := rcv.Start(ctx); err != nil {
		lg.Error("接收监听启动失败（应用继续运行，界面会提示）", "err", err)
		self.Add("error", "receive", "接收监听启动失败", "err", err.Error())
	} else {
		defer rcv.Close()
		if rcv.TLSEnabled() {
			self.Add("info", "receive", "TLS 接收已启动", "port", fmt.Sprint(cfg.SyslogTLSPort))
		} else {
			self.Add("warn", "receive", "TLS 接收未启动（未配置证书）", "port", fmt.Sprint(cfg.SyslogTLSPort))
		}
	}

	// ── 采集管理（文件源 + journald；运行时增删与总开关，FR-102/FR-1107）──
	// ★ journald 只读 + 能力门控（D-014/D-017）：没有权限就不启动，绝不自我授权。
	jrEnabled := !strings.EqualFold(cfg.JournaldEnabled, "off")
	if !jrEnabled {
		self.Add("info", "ingest", "系统日志采集已按配置关闭（LOGSERVER_JOURNALD_ENABLED=off）")
	}
	jrOpts := buildJournaldOptions(cfg, caps, self, lg, proc, deliver)
	mgr := newIngestManager(ctx, cfg.DataDir, proc, deliver, self, lg, jrOpts, jrEnabled)
	mgr.Start(acfg.Sources)
	defer mgr.Stop()

	// ── 采集/转发健康巡检（静默即故障，FR-107）──────────────────────
	go healthLoop(ctx, mgr, fwd, eng, self, lg)

	// ── 会话鉴权（D-008：复用 TOS 用户与会话；官方文档 §8.10）────────
	// 平台没有"查询当前用户/角色"的公开接口 → 默认 TOS 会话按 operator（最小权限）；
	// 管理操作（改配置/存储位置/导出配置）需要本地管理员令牌（<DataDir>/config/admin.token）。
	adminToken, tokenCreated, tokErr := auth.LoadOrCreateAdminToken(auth.AdminTokenPath(cfg.ConfigDir), os.Getenv)
	if tokErr != nil {
		lg.Warn("管理员令牌不可用（管理操作将保持锁定；检查数据卷可写性）", "err", tokErr)
		self.Add("warn", "auth", "管理员令牌不可用（管理操作锁定）", "err", tokErr.Error())
	} else {
		lg.Info("管理员令牌已就绪（不记录令牌内容）",
			"path", auth.AdminTokenPath(cfg.ConfigDir), "created", tokenCreated)
		if tokenCreated {
			self.Add("info", "auth", "已生成管理员令牌文件（0600）",
				"path", auth.AdminTokenPath(cfg.ConfigDir))
		}
	}
	acfgAuth := auth.FromEnv(os.Getenv)
	acfgAuth.AdminToken = adminToken
	acfgAuth.ClaimPath = filepath.Join(cfg.ConfigDir, "admin.claimed")
	acfgAuth.CookiePath = "/" + version.AppID + "/" // 只发给本应用前缀，避免同主机其他应用收到
	// env 文件（log-server-for-tos.env）里的鉴权设置也要生效 —— 否则用 `-env` 启动或直接跑二进制时
	// 会静默回落到默认值（systemd 下 EnvironmentFile 会遮住这个问题，属隐患）。
	// 优先级：进程环境变量 > env 文件；config.json 的 auth 段再覆盖两者。
	if os.Getenv("LOGSERVER_AUTH") == "" && cfg.AuthMode != "" {
		acfgAuth.Mode = auth.Mode(cfg.AuthMode)
	}
	if os.Getenv("LOGSERVER_AUTH_ADMINS") == "" && len(cfg.AuthAdmins) > 0 {
		acfgAuth.Admins = cfg.AuthAdmins
	}
	if acfg.Auth != nil {
		if m := strings.TrimSpace(acfg.Auth.Mode); m != "" {
			acfgAuth.Mode = auth.Mode(m)
		}
		if len(acfg.Auth.Admins) > 0 {
			acfgAuth.Admins = acfg.Auth.Admins
		}
	}
	authn := auth.New(acfgAuth)
	lg.Info("会话鉴权已启用", "mode", authn.Mode(),
		"platform_validation", acfgAuth.ValidateSession != nil)
	if w := authn.StartupWarning(); w != "" {
		lg.Warn(w)
		self.Add("warn", "auth", w)
	}

	// ── HTTP 服务（仅回环）─────────────────────────────────────────
	srv := httpapi.New(cfg, caps, st, agg, self, *webRoot, lg)
	srv.Auth = authn
	srv.Audit = auditLog
	onboardingPath := filepath.Join(cfg.ConfigDir, "onboarding.json")
	srv.OnboardingGet = func() onboarding.State { return onboarding.Load(onboardingPath) }
	srv.OnboardingComplete = func(actor string) error { return onboarding.Complete(onboardingPath, actor) }
	srv.OnboardingReset = func() error { return onboarding.Reset(onboardingPath) }
	srv.EngineDir = engineDir
	srv.RxStats = func() map[string]any {
		stx := rcv.Stats()
		q := ingestQueueSnapshot()
		return map[string]any{
			"udp_packets": stx.UDPPackets, "tcp_conns": stx.TCPConns, "tls_conns": stx.TLSConns,
			"tcp_active": stx.TCPActive, "tls_active": stx.TLSActive, "dropped": stx.Dropped,
			"acl_denied": stx.ACLDenied, "rate_limited": stx.RateLimited,
			"policy": rcv.Policy().Summary(),
			// 兼容前端既有字段；完整队列指标在 "queue" 里。
			"queue_depth": q["depth"], "queue_dropped": q["dropped"],
			"queue_bytes": q["bytes"], "queue_oldest_age_sec": q["oldest_age_sec"],
			"queue":          q,
			"last_recv_unix": stx.LastRecvUnix,
			"listeners": []map[string]any{
				{"protocol": "udp", "port": cfg.SyslogUDPPort, "enabled": true, "packets": stx.UDPPackets},
				{"protocol": "tcp", "port": cfg.SyslogTCPPort, "enabled": true, "conns": stx.TCPConns},
				{"protocol": "tls", "port": cfg.SyslogTLSPort, "enabled": rcv.TLSEnabled(), "conns": stx.TLSConns},
			},
		}
	}
	srv.IngestQueue = ingestQueueSnapshot
	srv.ReceivePolicy = func() map[string]any { return rcv.Policy().Summary() }
	srv.ReceiveApply = func(p receive.Policy) error {
		sm := p.Summary()
		allow, _ := sm["allow"].([]string)
		deny, _ := sm["deny"].([]string)
		prev := acfg.Receive
		acfg.Receive = &receivePolicy{Allow: allow, Deny: deny, MaxEPS: p.MaxEPS, MaxBytesPerSec: p.MaxBPS}
		if err := saveAppConfig(cfgPath, acfg, lg); err != nil {
			acfg.Receive = prev
			return err
		}
		rcv.SetPolicy(p)
		return nil
	}
	srv.ForwardStats = func() []forward.Status { return fwd.Stats() }
	srv.ForwardApply = func(t []forward.Target) error {
		if err := fwd.Apply(t); err != nil {
			return err
		}
		acfg.ForwardTargets = t
		return saveAppConfig(cfgPath, acfg, lg)
	}
	srv.AlertStats = func() map[string]any {
		st := eng.Stats()
		return map[string]any{
			"active": st.Active, "acked": st.Acked, "by_level": st.ByLevel, "total": st.Total,
			"rule_hits": st.RuleHits, "silences": st.Silences, "maintenance": st.Maintenance,
		}
	}
	srv.Alerts = func(n int) []alert.Alert { return eng.Recent(n) }
	srv.Rules = func() []alert.Rule { return eng.Rules() }

	// FR-804：告警确认 / 静默 / 维护窗口
	srv.AlertEngine = eng

	// FR-803：通知通道
	srv.NotifyConfig = func() []notify.Config { return acfg.Notifications }
	srv.NotifyApply = func(cs []notify.Config) error {
		if warns := nd.Apply(cs); len(warns) > 0 {
			return fmt.Errorf("通道配置无效：%s", strings.Join(warns, "；"))
		}
		acfg.Notifications = cs
		return saveAppConfig(cfgPath, acfg, lg)
	}
	srv.NotifyStatus = func() []notify.Delivery { return nd.Recent(50) }
	srv.NotifyTest = func(ctx context.Context, kind string) error { return nd.Test(ctx, kind) }
	srv.Sources = func() []map[string]any {
		items, _ := mgr.Sources()
		return items
	}
	// FR-102：运行时增删/启停文件采集源
	srv.SourcesApply = func(specs []httpapi.SourceSpec) error {
		out := make([]sourceSpec, 0, len(specs))
		for _, sp := range specs {
			out = append(out, sourceSpec{
				Path: sp.Path, Multiline: sp.Multiline, Enabled: sp.Enabled, Encoding: sp.Encoding,
			})
		}
		acfg.Sources = out
		if err := saveAppConfig(cfgPath, acfg, lg); err != nil {
			return err
		}
		mgr.Apply(out)
		return nil
	}
	// FR-1107：采集总开关（维护模式）
	srv.IngestPause = func(paused bool) { mgr.SetPaused(paused) }
	srv.IngestPaused = mgr.Paused

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTPAddr, cfg.HTTPPort),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0, // 为将来的实时流保留
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		lg.Info("HTTP 监听中", "addr", httpSrv.Addr)
		self.Add("info", "http", "管理界面已启动", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		lg.Info("收到退出信号，开始优雅关闭")
		self.Add("info", "main", "收到退出信号")
	case err := <-errCh:
		lg.Error("HTTP 服务异常退出", "err", err)
		self.Add("error", "main", "HTTP 服务异常退出", "err", err.Error())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		lg.Warn("HTTP 关闭超时", "err", err)
	}
	// 关闭顺序（D-030）：先停接收 → 再停入库队列（等当前批写完 / 未写完的留在队列）
	// → 最后关存储。顺序反了会让关闭后的投递被当成"已入队"而丢失。
	_ = rcv.Close()
	spCtx, spCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := sp.Close(spCtx); err != nil {
		lg.Warn("入库队列关闭超时（未写完的记录会在下次启动重放）", "err", err)
	}
	spCancel()
	if qs := sp.Stats(); qs.Depth > 0 {
		lg.Info("退出时仍有待写入记录（已持久化，下次启动重放）",
			"depth", qs.Depth, "bytes", qs.Bytes)
	}
	if err := st.Close(); err != nil {
		lg.Warn("存储关闭异常", "err", err)
	}
	lg.Info("已退出")
}

// ── 应用配置（config.json 的扩展段）─────────────────────────────────

// appConfig 是 config.json 中与运行策略相关的段（其余键由 config 包处理）。
type appConfig struct {
	Sources        []sourceSpec     `json:"sources"`
	ForwardTargets []forward.Target `json:"forward_targets"`
	AlertRules     []alert.Rule     `json:"alert_rules"`
	Notifications  []notify.Config  `json:"notifications"`
	Auth           *authSection     `json:"auth"`
	Receive        *receivePolicy   `json:"receive_policy,omitempty"`
}

type receivePolicy struct {
	Allow          []string `json:"allow"`
	Deny           []string `json:"deny"`
	MaxEPS         int      `json:"max_eps"`
	MaxBytesPerSec int64    `json:"max_bytes_per_sec"`
}

// authSection 允许用 config.json 覆盖 env 中的鉴权设置。
type authSection struct {
	Mode   string   `json:"mode"`
	Admins []string `json:"admins"`
	Note   string   `json:"note,omitempty"`
}

type sourceSpec struct {
	Path      string `json:"path"`
	Multiline bool   `json:"multiline"`
	Enabled   bool   `json:"enabled"`
	// Encoding 是该采集源的编码声明（"" / "auto" = 自动探测；
	// 遗留 GBK/Big5 日志建议显式声明，如 "GBK"）
	Encoding string `json:"encoding,omitempty"`
}

func loadAppConfig(path string, lg *slog.Logger) appConfig {
	var out appConfig
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			lg.Warn("读取 config.json 失败", "err", err, "path", path)
		}
		return out
	}
	var raw struct {
		Sources        []sourceSpec     `json:"sources"`
		ForwardTargets []forward.Target `json:"forward_targets"`
		AlertRules     []alert.Rule     `json:"alert_rules"`
		Notifications  []notify.Config  `json:"notifications"`
		Auth           *authSection     `json:"auth"`
		Receive        *receivePolicy   `json:"receive_policy"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		lg.Warn("解析 config.json 失败", "err", err)
		return out
	}
	out.Sources, out.ForwardTargets, out.AlertRules = raw.Sources, raw.ForwardTargets, raw.AlertRules
	out.Notifications, out.Auth = raw.Notifications, raw.Auth
	out.Receive = raw.Receive
	return out
}

// saveAppConfig 只更新我们负责的段，**保留文件中的其它键**（不覆盖用户/平台写入的内容）。
func saveAppConfig(path string, acfg appConfig, lg *slog.Logger) error {
	merged := map[string]json.RawMessage{}
	if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
		_ = json.Unmarshal(b, &merged)
	}
	if acfg.Sources != nil {
		merged["sources"] = mustJSON(acfg.Sources)
	}
	merged["forward_targets"] = mustJSON(acfg.ForwardTargets)
	if len(acfg.AlertRules) > 0 {
		merged["alert_rules"] = mustJSON(acfg.AlertRules)
	}
	if acfg.Notifications != nil {
		merged["notifications"] = mustJSON(acfg.Notifications)
	}
	if acfg.Receive != nil {
		merged["receive_policy"] = mustJSON(acfg.Receive)
	}
	b, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp := path + ".tmp"
	// 0600：config.json 可能含 SMTP 口令等凭据（红线 7）
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	lg.Info("配置已保存", "path", path)
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ── 文件采集器 ──────────────────────────────────────────────────────

// collector 是一个运行中的文件采集器。
type collector struct {
	path         string
	tl           *fileingest.Tailer
	multiline    bool
	encodingHint string
	stop         chan struct{}
	done         chan struct{}

	lastRaw atomic.Int64
	polls   atomic.Uint64
	emitted atomic.Uint64
	lastErr atomic.Value // string
}

// Status 返回采集源状态（FR-107：静默即故障）。
func (c *collector) Status() map[string]any {
	last := c.lastRaw.Load()
	var silentFor int64 = -1
	if last > 0 {
		silentFor = time.Now().Unix() - last
	}
	errStr, _ := c.lastErr.Load().(string)
	return map[string]any{
		"path": c.path, "multiline": c.multiline, "enabled": true, "encoding": c.encodingHint,
		"last_raw_unix": last, "silent_for_sec": silentFor,
		"polls": c.polls.Load(), "records": c.emitted.Load(),
		"offset": c.tl.State().Offset, "inode": c.tl.State().Inode,
		"rotations": c.tl.State().Rotations, "truncates": c.tl.State().Truncates,
		"error": errStr,
	}
}

func (c *collector) Stop() {
	close(c.stop)
	<-c.done
	_ = c.tl.Close()
}

// startFileCollectors 启动 config.json 里配置的文件采集源。
func startFileCollectors(ctx context.Context, specs []sourceSpec, dataDir string,
	proc *pipeline.Processor, deliver func(*model.Record), self *selflog.Log, lg *slog.Logger) []*collector {

	store := &fileingest.Store{Path: filepath.Join(dataDir, "state", "offsets.json")}
	offsets, err := store.Load()
	if err != nil {
		lg.Warn("读取采集位点失败（从 0 开始）", "err", err)
		self.Add("warn", "ingest", "读取采集位点失败，将从文件开头重新采集", "err", err.Error())
		offsets = map[string]fileingest.Offset{}
	}

	var out []*collector
	for _, sp := range specs {
		if !sp.Enabled || sp.Path == "" {
			continue
		}
		ml := fileingest.Multiline{}
		if sp.Multiline {
			ml = fileingest.DefaultMultiline()
		}
		c := &collector{
			path: sp.Path, multiline: sp.Multiline, encodingHint: sp.Encoding,
			stop: make(chan struct{}), done: make(chan struct{}),
		}
		c.lastErr.Store("")
		c.tl = fileingest.New(sp.Path, offsets[sp.Path], ml)
		out = append(out, c)

		go func(c *collector) {
			defer close(c.done)
			t := time.NewTicker(time.Second)
			defer t.Stop()
			persist := time.NewTicker(10 * time.Second)
			defer persist.Stop()

			for {
				select {
				case <-c.stop:
					for _, line := range c.tl.Flush() {
						emit(c, line, proc, deliver)
					}
					saveOffset(store, c, lg)
					return
				case <-ctx.Done():
					return
				case <-t.C:
					c.polls.Add(1)
					lines, err := c.tl.Poll()
					if err != nil {
						c.lastErr.Store(err.Error())
						lg.Debug("文件采集出错", "path", c.path, "err", err)
						self.Add("warn", "ingest", "文件采集出错", "path", c.path, "err", err.Error())
						continue
					}
					if len(lines) > 0 {
						c.lastRaw.Store(time.Now().Unix())
						for _, line := range lines {
							emit(c, line, proc, deliver)
						}
					}
				case <-persist.C:
					saveOffset(store, c, lg)
				}
			}
		}(c)
		lg.Info("文件采集已启动", "path", sp.Path, "multiline", sp.Multiline)
		self.Add("info", "ingest", "文件采集已启动", "path", sp.Path)
	}
	return out
}

func emit(c *collector, line string, proc *pipeline.Processor, deliver func(*model.Record)) {
	// 文件来源：**走管线做格式识别**（FR-201：JSON / logfmt / CLF / syslog / 纯文本）
	// 与编码探测（FR-204）。原文（Raw / _msg）始终逐字节保留。
	r := proc.ProcessFile([]byte(line), c.path, time.Now().UTC(), c.encodingHint)
	c.emitted.Add(1)
	deliver(r)
}

func saveOffset(st *fileingest.Store, c *collector, lg *slog.Logger) {
	m, err := st.Load()
	if err != nil {
		return
	}
	m[c.path] = c.tl.State()
	if err := st.Save(m); err != nil {
		lg.Warn("保存采集位点失败", "path", c.path, "err", err)
	}
}

// ── 健康巡检（静默即故障 FR-107）───────────────────────────────────

func healthLoop(ctx context.Context, mgr *ingestManager, fwd *forward.Manager,
	eng *alert.Engine, self *selflog.Log, lg *slog.Logger) {

	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	const silentThreshold = 10 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now().Unix()
			// 采集源静默（暂停维护时无采集器，自然跳过）
			for _, c := range mgr.fileCollectors() {
				last := c.lastRaw.Load()
				if last > 0 && now-last > int64(silentThreshold.Seconds()) {
					self.Add("warn", "ingest", "采集源静默（长时间无新日志）",
						"path", c.path, "silent_sec", fmt.Sprint(now-last))
					lg.Warn("采集源静默", "path", c.path, "silent_sec", now-last)
				}
			}
			// 转发积压
			for _, st := range fwd.Stats() {
				if st.Queue.Depth > 1000 {
					self.Add("warn", "forward", "转发队列积压",
						"id", st.ID, "depth", fmt.Sprint(st.Queue.Depth))
				}
				if st.LastErr != "" && st.LastErrAt != nil &&
					time.Since(*st.LastErrAt) < 2*time.Minute {
					self.Add("warn", "forward", "转发目标不可达",
						"id", st.ID, "err", st.LastErr)
				}
			}
			// 告警总量
			if s := eng.Stats(); s.Active > 100 {
				self.Add("warn", "alert", "活跃告警过多，请检查是否存在告警风暴",
					"active", fmt.Sprint(s.Active))
			}
		}
	}
}

// ── 工具 ────────────────────────────────────────────────────────────

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}

func logCaps(lg *slog.Logger, self *selflog.Log, items []capability.Status) {
	available := 0
	for _, c := range items {
		if c.Available() {
			available++
			lg.Info("能力可用", "id", c.ID, "title", c.Title)
			continue
		}
		lg.Info("能力不可用", "id", c.ID, "title", c.Title, "reason", c.Reason)
	}
	lg.Info("能力探测完成", "total", len(items), "available", available)
	self.Add("info", "capability", "能力探测完成",
		"total", fmt.Sprint(len(items)), "available", fmt.Sprint(available))
}

func kvToArgs(kv []string) []any {
	out := make([]any, 0, len(kv))
	for _, v := range kv {
		out = append(out, v)
	}
	return out
}

// 确保 strings 被使用（保留给后续解析扩展）
var _ = strings.TrimSpace

// ── 告警静默 / 维护窗口持久化（FR-804）──────────────────────────────

func loadSilences(path string) ([]alert.Silence, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []alert.Silence
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func saveSilences(path string, list []alert.Silence) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
