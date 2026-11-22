// ingest_manager.go —— 采集管理器：统一管理「文件采集源」与「journald」的启停。
//
// 对应需求：
//   - FR-102：运行时增删/启停文件采集源（此前只能手改 config.json）；
//   - FR-1107：采集总开关（维护模式）—— **暂停读取**（文件 + journald），
//     接收端继续收并写入存储（不丢数据），恢复后从上次位点续采。
package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Moechz/log-server/backend/internal/capability"
	appconfig "github.com/Moechz/log-server/backend/internal/config"
	"github.com/Moechz/log-server/backend/internal/ingest/journald"
	"github.com/Moechz/log-server/backend/internal/model"
	"github.com/Moechz/log-server/backend/internal/pipeline"
	"github.com/Moechz/log-server/backend/internal/selflog"
)

type ingestManager struct {
	ctx       context.Context
	dataDir   string
	proc      *pipeline.Processor
	deliver   func(*model.Record)
	self      *selflog.Log
	lg        *slog.Logger
	jrOpts    journald.Options
	jrEnabled bool

	mu     sync.Mutex
	paused bool
	specs  []sourceSpec
	cols   []*collector
	jrn    *journald.Collector
}

func newIngestManager(ctx context.Context, dataDir string, proc *pipeline.Processor,
	deliver func(*model.Record), self *selflog.Log, lg *slog.Logger,
	jrOpts journald.Options, jrEnabled bool) *ingestManager {
	return &ingestManager{
		ctx: ctx, dataDir: dataDir, proc: proc, deliver: deliver,
		self: self, lg: lg, jrOpts: jrOpts, jrEnabled: jrEnabled,
	}
}

// Start 首次启动：起文件采集（按 specs）与 journald（若允许）。
func (m *ingestManager) Start(specs []sourceSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.specs = specs
	if m.paused {
		return
	}
	m.startFilesLocked()
	m.startJournaldLocked()
}

// Apply 更新文件采集源集合（运行时增删/启停）。
func (m *ingestManager) Apply(specs []sourceSpec) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.specs = specs
	if m.paused {
		m.lg.Info("采集源已更新（当前暂停中，恢复后生效）", "count", len(specs))
		return
	}
	m.stopFilesLocked()
	m.startFilesLocked()
}

// SetPaused 采集总开关（维护模式，FR-1107）。
func (m *ingestManager) SetPaused(paused bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.paused == paused {
		return
	}
	m.paused = paused
	if paused {
		m.stopFilesLocked()
		m.stopJournaldLocked()
		m.self.Add("warn", "ingest", "采集已暂停（维护模式）：停止读取文件与系统日志；接收端继续收并写入存储")
		m.lg.Warn("采集已暂停（维护模式）")
	} else {
		m.startFilesLocked()
		m.startJournaldLocked()
		m.self.Add("info", "ingest", "采集已恢复（从上次位点继续）")
		m.lg.Info("采集已恢复")
	}
}

// Paused 返回是否处于暂停（维护模式）。
func (m *ingestManager) Paused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paused
}

// fileCollectors 返回当前文件采集器快照（供健康巡检使用）。
func (m *ingestManager) fileCollectors() []*collector {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*collector(nil), m.cols...)
}

// Sources 返回采集源状态（journald + 文件）与暂停状态。
func (m *ingestManager) Sources() ([]map[string]any, bool) {
	m.mu.Lock()
	jrn := m.jrn
	cols := append([]*collector(nil), m.cols...)
	paused := m.paused
	jrEnabled := m.jrEnabled
	m.mu.Unlock()

	items := make([]map[string]any, 0, len(cols)+1)
	if jrn != nil {
		items = append(items, jrn.Status())
	} else if jrEnabled {
		// journald 允许但当前未运行（暂停中）→ 如实说明，不假装在采
		items = append(items, map[string]any{
			"kind": "journald", "path": "journald://system", "enabled": false,
			"authorized": false, "paused": paused, "note": "采集已暂停（维护模式）",
		})
	}
	for _, c := range cols {
		st := c.Status()
		if paused {
			st["paused"] = true
		}
		items = append(items, st)
	}
	return items, paused
}

// Stop 关闭全部采集（进程退出时）。
func (m *ingestManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopFilesLocked()
	m.stopJournaldLocked()
}

func (m *ingestManager) startFilesLocked() {
	m.cols = startFileCollectors(m.ctx, m.specs, m.dataDir, m.proc, m.deliver, m.self, m.lg)
}

func (m *ingestManager) stopFilesLocked() {
	for _, c := range m.cols {
		c.Stop() // 停止前会把位点落盘，恢复后从位点续采
	}
	m.cols = nil
}

func (m *ingestManager) startJournaldLocked() {
	if !m.jrEnabled {
		return
	}
	jrn := journald.New(m.jrOpts)
	if err := jrn.Start(m.ctx); err != nil && err != journald.ErrNotAuthorized {
		m.lg.Warn("系统日志采集启动失败（不影响其他功能）", "err", err)
		m.self.Add("warn", "journald", "系统日志采集启动失败", "err", err.Error())
	}
	m.jrn = jrn
}

func (m *ingestManager) stopJournaldLocked() {
	if m.jrn != nil {
		m.jrn.Stop()
		m.jrn = nil
	}
}

// buildJournaldOptions 从运行配置构造 journald 采集参数（只读 + 能力门控，D-014/D-017）。
func buildJournaldOptions(cfg appconfig.Config, caps *capability.Registry, self *selflog.Log,
	lg *slog.Logger, proc *pipeline.Processor, deliver func(*model.Record)) journald.Options {
	args := []string{"--output=json", "--follow", "--no-pager"}
	if cfg.JournaldSince != "" {
		args = append(args, "--since="+cfg.JournaldSince)
	}
	for _, u := range cfg.JournaldUnits {
		args = append(args, "-u", u)
	}
	args = append(args, cfg.JournaldExtra...)
	return journald.Options{
		Args: args,
		Authorized: func() bool {
			st, ok := caps.Get(capability.CapSystemLogRead)
			return ok && st.Available()
		},
		AuthReason: func() string {
			if st, ok := caps.Get(capability.CapSystemLogRead); ok && st.Reason != "" {
				return st.Reason
			}
			return ""
		},
		Handler: func(raw []byte) {
			deliver(proc.ProcessJournal(raw, time.Now().UTC()))
		},
		OnSelfLog: func(level, msg string, kv ...string) { self.Add(level, "journald", msg, kv...) },
		Logger:    lg,
	}
}
