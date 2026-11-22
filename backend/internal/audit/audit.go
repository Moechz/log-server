// Package audit 实现**管理操作留痕**（FR-1104 / NFR-S5）。
//
// 与"采集到的日志"分开：本包只记录**人对应用做的操作**（改配置、导出、暂停采集、
// 告警确认/静默、管理员认领…），落盘在 <DataDir>/audit/actions.log（JSON Lines）。
//
// 为什么不写进 VictoriaLogs：
//   - 管理审计不能随日志保留期（默认 14 天）被引擎淘汰；
//   - 存储引擎故障时仍要能记录"谁改了什么"；
//   - 独立文件便于取证与导出，也不会被日志清理任务误删。
//
// 诚实边界：文件是**应用层追加写**，不是 WORM/哈希链；能读数据目录的人仍可删除/篡改。
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Moechz/log-server/backend/internal/mask"
)

// Entry 是一条管理操作记录。
type Entry struct {
	Ts     time.Time `json:"ts"`
	Actor  string    `json:"actor"`            // local-admin | tos-session | dev | unknown
	Role   string    `json:"role,omitempty"`   // admin | operator | viewer | auditor
	Source string    `json:"source,omitempty"` // local | tos | dev
	Action string    `json:"action"`           // config.export / sources.put / ...
	Target string    `json:"target,omitempty"`
	Result string    `json:"result"` // ok | denied | error
	Detail string    `json:"detail,omitempty"`
	IP     string    `json:"ip,omitempty"`
	Method string    `json:"method,omitempty"`
	Path   string    `json:"path,omitempty"`
}

// Options 是落盘参数（默认即产品口径）。
type Options struct {
	Dir           string
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxAge        time.Duration
	Now           func() time.Time
}

const (
	DefaultMaxFileBytes  = 10 << 20  // 10 MB / 文件
	DefaultMaxTotalBytes = 200 << 20 // 200 MB 总量
	DefaultMaxAge        = 180 * 24 * time.Hour
	baseName             = "actions.log"
	ringCap              = 500
)

// Stats 是审计日志统计（供 API/诊断）。
type Stats struct {
	Path        string `json:"path"`
	Files       int    `json:"files"`
	Bytes       int64  `json:"bytes"`
	Entries     int64  `json:"entries"` // 本次进程写入
	InMemory    int    `json:"in_memory"`
	WriteErrors int64  `json:"write_errors"`
	MaxAgeDays  int    `json:"max_age_days"`
	MaxTotalMB  int64  `json:"max_total_mb"`
	LastErr     string `json:"last_err,omitempty"`
}

// Log 是并发安全的追加式审计日志。
type Log struct {
	mu     sync.Mutex
	opt    Options
	now    func() time.Time
	f      *os.File
	size   int64
	ring   []Entry
	total  int64
	errs   int64
	last   string
	writes int
}

// Open 打开（或创建）审计日志并载入最近记录到内存环。
func Open(opt Options) (*Log, error) {
	if opt.Dir == "" {
		return nil, fmt.Errorf("audit: 缺少 Dir")
	}
	if opt.MaxFileBytes <= 0 {
		opt.MaxFileBytes = DefaultMaxFileBytes
	}
	if opt.MaxTotalBytes <= 0 {
		opt.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if opt.MaxAge <= 0 {
		opt.MaxAge = DefaultMaxAge
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if err := os.MkdirAll(opt.Dir, 0o750); err != nil {
		return nil, err
	}
	l := &Log{opt: opt, now: opt.Now}
	if err := l.openLocked(); err != nil {
		return nil, err
	}
	l.loadRecentLocked(ringCap)
	l.gcLocked()
	return l, nil
}

func (l *Log) Path() string { return filepath.Join(l.opt.Dir, baseName) }

func (l *Log) openLocked() error {
	f, err := os.OpenFile(l.Path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// Append 追加一条（脱敏后的）记录；失败只计数并保留内存环，不影响业务。
func (l *Log) Append(e Entry) {
	if e.Ts.IsZero() {
		e.Ts = l.now().UTC()
	}
	e.Target = mask.Value(e.Target)
	e.Detail = mask.Value(e.Detail)
	if e.Result == "" {
		e.Result = "ok"
	}
	b, err := json.Marshal(e)
	if err != nil {
		l.mu.Lock()
		l.errs++
		l.last = err.Error()
		l.mu.Unlock()
		return
	}
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	l.ring = append(l.ring, e)
	if len(l.ring) > ringCap {
		l.ring = l.ring[len(l.ring)-ringCap:]
	}
	l.total++
	l.writes++
	if l.f == nil {
		if err := l.openLocked(); err != nil {
			l.errs++
			l.last = err.Error()
			return
		}
	}
	n, werr := l.f.Write(b)
	if werr == nil {
		werr = l.f.Sync() // 管理操作低频：每条 fsync，崩溃后不留"已操作但无记录"的窗口
	}
	if werr != nil {
		l.errs++
		l.last = werr.Error()
		return
	}
	l.size += int64(n)
	if l.size >= l.opt.MaxFileBytes {
		l.rotateLocked()
	}
	if l.writes%50 == 0 {
		l.gcLocked()
	}
}

// rotateLocked 把当前文件归档，另起新文件。
func (l *Log) rotateLocked() {
	if l.f == nil {
		return
	}
	_ = l.f.Sync()
	_ = l.f.Close()
	l.f = nil
	stamp := l.now().Format("20060102")
	for i := 0; ; i++ {
		dst := filepath.Join(l.opt.Dir, fmt.Sprintf("audit-%s.%03d.log", stamp, i))
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			if err := os.Rename(l.Path(), dst); err != nil {
				l.errs++
				l.last = err.Error()
			}
			break
		}
	}
	l.size = 0
	if err := l.openLocked(); err != nil {
		l.errs++
		l.last = err.Error()
	}
	l.gcLocked()
}

// gcLocked 删除过期归档，再把总量压到上限（当前文件不动）。
func (l *Log) gcLocked() {
	now := l.now()
	files := l.listLocked()
	var total int64
	type fi struct {
		path string
		size int64
		mod  time.Time
	}
	var archives []fi
	for _, p := range files {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		total += st.Size()
		if p == l.Path() {
			continue
		}
		if now.Sub(st.ModTime()) > l.opt.MaxAge {
			_ = os.Remove(p)
			total -= st.Size()
			continue
		}
		archives = append(archives, fi{p, st.Size(), st.ModTime()})
	}
	sort.Slice(archives, func(i, j int) bool { return archives[i].mod.Before(archives[j].mod) })
	for _, a := range archives {
		if total <= l.opt.MaxTotalBytes {
			break
		}
		if err := os.Remove(a.path); err == nil {
			total -= a.size
		}
	}
}

func (l *Log) listLocked() []string {
	ents, err := os.ReadDir(l.opt.Dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if n == baseName || (strings.HasPrefix(n, "audit-") && strings.HasSuffix(n, ".log")) {
			out = append(out, filepath.Join(l.opt.Dir, n))
		}
	}
	return out
}

// loadRecentLocked 从当前文件 + 最近归档载入最近 n 条（坏行跳过）。
// 轮转可能刚把最后几条移到归档、当前文件为空 —— 因此不能只看当前文件。
func (l *Log) loadRecentLocked(n int) {
	files := l.listLocked()
	type fi struct {
		path string
		mod  time.Time
	}
	var fs []fi
	for _, p := range files {
		if st, err := os.Stat(p); err == nil {
			fs = append(fs, fi{p, st.ModTime()})
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].mod.After(fs[j].mod) })
	var all []Entry
	for idx, f := range fs {
		if idx >= 4 { // 最多回看 4 个文件（≤40MB），避免启动时扫全部历史
			break
		}
		entries := readEntries(f.path)
		all = append(entries, all...)
		if len(all) >= n {
			break
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	l.ring = all
}

func readEntries(path string) []Entry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out []Entry
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// Recent 返回最近 n 条（新的在前）。
func (l *Log) Recent(n int) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.ring) {
		n = len(l.ring)
	}
	out := make([]Entry, 0, n)
	for i := len(l.ring) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, l.ring[i])
	}
	return out
}

// Stats 返回统计快照。
func (l *Log) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	var bytes int64
	files := l.listLocked()
	for _, p := range files {
		if st, err := os.Stat(p); err == nil {
			bytes += st.Size()
		}
	}
	return Stats{
		Path: l.Path(), Files: len(files), Bytes: bytes, Entries: l.total,
		InMemory: len(l.ring), WriteErrors: l.errs,
		MaxAgeDays: int(l.opt.MaxAge.Hours() / 24), MaxTotalMB: l.opt.MaxTotalBytes >> 20,
		LastErr: l.last,
	}
}

// Close 同步并关闭文件。
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	_ = l.f.Sync()
	err := l.f.Close()
	l.f = nil
	return err
}
