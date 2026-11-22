// file.go —— 应用自身日志的**落盘**（FR-1101 / storage 页 "self-7d" 承诺的兑现）。
//
// 设计要点：
//   - 格式：JSON Lines（一行一条 Entry），便于 `tail -f`、grep 与后续工具处理；
//   - 防自噬（FR-1102）：**三层硬上限**——单文件 10 MB、总量 200 MB、保留 7 天；
//   - 由日志自己轮转（不依赖 logrotate：运行时数据在数据卷，系统工具通常不覆盖该路径）；
//   - **写入前脱敏**（红线 7）：字段按键名/值样式走 `mask` 包，凭据绝不落盘；
//   - 重启后追加到当天文件（不丢历史）；轮转/清理在写入路径与启动时各做一次。
package selflog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Moechz/log-server/backend/internal/mask"
)

// FileOptions 是落盘参数（默认值即 storage 页承诺的口径）。
type FileOptions struct {
	Dir           string        // 目录（通常 <DataDir>/logs）
	MaxFileBytes  int64         // 单文件上限，默认 10 MB
	MaxTotalBytes int64         // 总量上限，默认 200 MB
	MaxAge        time.Duration // 保留时长，默认 7 天
	Now           func() time.Time
}

// 默认值（与「存储」页保留策略 self-7d 一致）。
const (
	DefaultMaxFileBytes  = 10 << 20
	DefaultMaxTotalBytes = 200 << 20
	DefaultMaxAge        = 7 * 24 * time.Hour
	selfLogBaseName      = "app.log"
)

// FileStats 是落盘统计（供 API/UI 展示与验证）。
type FileStats struct {
	Path          string `json:"path"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	MaxFileBytes  int64  `json:"max_file_bytes"`
	MaxTotalBytes int64  `json:"max_total_bytes"`
	MaxAgeDays    int    `json:"max_age_days"`
	OldestUnix    int64  `json:"oldest_unix,omitempty"`
	Written       int64  `json:"written"`
	WriteErrors   int64  `json:"write_errors"`
	Dropped       int64  `json:"dropped"` // 因上限被清理/丢弃的条数（近似：文件删除数）
}

// FileSink 是自身日志的落盘写入器（并发安全）。
type FileSink struct {
	mu       sync.Mutex
	opt      FileOptions
	now      func() time.Time
	cur      *os.File
	curSize  int64
	written  int64
	errs     int64
	dropped  int64
	lastGC   time.Time
	dayStamp string
}

// OpenFileSink 打开（必要时创建）落盘目录并返回写入器。
// 目录不可用时返回错误，由调用方降级为「仅内存」（自身日志的落盘不应阻断主进程启动）。
func OpenFileSink(opt FileOptions) (*FileSink, error) {
	if opt.Dir == "" {
		return nil, fmt.Errorf("selflog: 落盘目录不能为空")
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
	s := &FileSink{opt: opt, now: opt.Now}
	s.gc() // 启动即清理一次：过期文件、超总量文件

	// 启动时的接续规则（★ 真机踩坑修正 2026-10-14：此前每次重启都会归档 app.log，
	// 等于"重启就切文件"，与"重启不丢历史、续写当天文件"的设计意图不符）：
	//   - app.log 属于**当天** → 直接**续写**（append），不归档；
	//   - app.log 属于更早的日期（进程停机跨天）→ 归档，另起当天的 app.log；
	//   - 不存在 → 新建。
	if fi, err := os.Stat(s.Path()); err == nil && fi.Size() > 0 {
		if isSameDay(fi.ModTime(), s.now()) {
			f, err := os.OpenFile(s.Path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
			if err != nil {
				return nil, err
			}
			s.cur, s.curSize = f, fi.Size()
			s.dayStamp = s.now().Format("20060102")
		} else if err := s.archiveCurrentLocked(); err != nil {
			return nil, err
		}
	}
	if err := s.rotateIfNeeded(0); err != nil {
		return nil, err
	}
	return s, nil
}

// recordError 记录一次写入失败（Add 路径调用；Write 内部已计一次，这里只兜底 panic 级情形）。
func (s *FileSink) recordError() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs++
}

// Path 返回当前文件路径。
func (s *FileSink) Path() string { return filepath.Join(s.opt.Dir, selfLogBaseName) }

// Write 写入一条（已脱敏）。返回错误仅供调用方统计，不中断业务。
func (s *FileSink) Write(e Entry) error {
	// 脱敏：字段键名与值都过一遍（Msg 也可能含 URL 凭据）
	safe := Entry{
		Ts:     e.Ts.UTC(),
		Level:  e.Level,
		Module: e.Module,
		Msg:    mask.Value(e.Msg),
		Fields: mask.Fields(e.Fields),
	}
	b, err := json.Marshal(safe)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rotateIfNeeded(int64(len(b))); err != nil {
		s.errs++
		return err
	}
	if s.cur == nil {
		s.errs++
		return fmt.Errorf("selflog: 落盘文件不可用")
	}
	n, err := s.cur.Write(b)
	s.curSize += int64(n)
	s.written++
	if err != nil {
		s.errs++
		return err
	}
	// 轻量 GC：每分钟最多一次（避免每次写入都扫目录）。注意此处已持锁 → 只能用 gcLocked。
	if s.now().Sub(s.lastGC) > time.Minute {
		s.gcLocked()
	}
	return nil
}

// Stats 返回落盘统计。
func (s *FileSink) Stats() FileStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, total, oldest := s.scan()
	return FileStats{
		Path:          s.Path(),
		Files:         files,
		Bytes:         total,
		MaxFileBytes:  s.opt.MaxFileBytes,
		MaxTotalBytes: s.opt.MaxTotalBytes,
		MaxAgeDays:    int(s.opt.MaxAge.Hours() / 24),
		OldestUnix:    oldest,
		Written:       s.written,
		WriteErrors:   s.errs,
		Dropped:       s.dropped,
	}
}

// Close 关闭当前文件。
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil
	}
	err := s.cur.Close()
	s.cur = nil
	return err
}

// ── 内部 ────────────────────────────────────────────────────────────

// rotateIfNeeded 在「当前文件将超单文件上限」或「跨天」时轮转：
// 把 `app.log` 归档为 `app-<日期>.<序号>.log`，再新建空的 `app.log`。
//
// 约定：**当前文件恒为 `app.log`**（排查时不必猜文件名），历史文件都带日期章。
func (s *FileSink) rotateIfNeeded(incoming int64) error {
	day := s.now().Format("20060102")
	need := s.cur == nil ||
		s.curSize+incoming > s.opt.MaxFileBytes ||
		(s.dayStamp != "" && s.dayStamp != day)
	if !need {
		return nil
	}
	if err := s.archiveCurrentLocked(); err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	s.cur, s.curSize, s.dayStamp = f, fi.Size(), day
	s.lastGC = s.now()
	s.gcLocked()
	return nil
}

// archiveCurrentLocked 需持锁调用：关闭并归档当前 app.log（空文件直接删掉，不留垃圾）。
func (s *FileSink) archiveCurrentLocked() error {
	if s.cur != nil {
		_ = s.cur.Close()
		s.cur = nil
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		return nil // 还没有当前文件
	}
	if fi.Size() == 0 {
		_ = os.Remove(s.Path())
		return nil
	}
	return os.Rename(s.Path(), filepath.Join(s.opt.Dir, s.archiveName(fi)))
}

// archiveName 生成归档名：app-<文件所属日期>.<序号>.log（序号取第一个未占用）。
func (s *FileSink) archiveName(fi os.FileInfo) string {
	day := fi.ModTime().Format("20060102")
	for i := 0; i < 1000; i++ {
		cand := fmt.Sprintf("app-%s.%03d.log", day, i)
		if _, err := os.Stat(filepath.Join(s.opt.Dir, cand)); os.IsNotExist(err) {
			return cand
		}
	}
	return fmt.Sprintf("app-%s.%d.log", day, s.now().UnixNano())
}

// isSameDay 判断两个时间是否为同一自然日（跨天轮转用）。
func isSameDay(t, ref time.Time) bool {
	y1, m1, d1 := t.Date()
	y2, m2, d2 := ref.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// gc 清理过期文件与超总量文件（调用方不持锁）。
func (s *FileSink) gc() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
}

type logFile struct {
	path string
	size int64
	mod  time.Time
}

// gcLocked 需持锁调用：先删过期，再从最旧开始删到总量达标（当前写入文件不删）。
func (s *FileSink) gcLocked() {
	list := s.list()
	cutoff := s.now().Add(-s.opt.MaxAge)
	alive := make([]logFile, 0, len(list))
	for _, f := range list {
		if f.mod.Before(cutoff) && f.path != s.Path() {
			if err := os.Remove(f.path); err == nil {
				s.dropped++
				continue
			}
		}
		alive = append(alive, f)
	}
	var total int64
	for _, f := range alive {
		total += f.size
	}
	for _, f := range alive { // 已按 mod 升序
		if total <= s.opt.MaxTotalBytes {
			break
		}
		if f.path == s.Path() {
			continue
		}
		if err := os.Remove(f.path); err == nil {
			total -= f.size
			s.dropped++
		}
	}
	s.lastGC = s.now()
}

// list 返回目录下的自日志文件（按修改时间升序）。
func (s *FileSink) list() []logFile {
	ents, err := os.ReadDir(s.opt.Dir)
	if err != nil {
		return nil
	}
	out := make([]logFile, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "app") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, logFile{path: filepath.Join(s.opt.Dir, e.Name()), size: fi.Size(), mod: fi.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod.Before(out[j].mod) })
	return out
}

// scan 统计文件数与总字节、最旧文件的修改时间。
func (s *FileSink) scan() (files int, total int64, oldestUnix int64) {
	list := s.list()
	for i, f := range list {
		total += f.size
		if i == 0 {
			oldestUnix = f.mod.Unix()
		}
	}
	return len(list), total, oldestUnix
}

// SetFileSink 让内存环形缓冲同时写盘（写入前自动脱敏）。
func (l *Log) SetFileSink(s *FileSink) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sink = s
}

// FileStats 返回落盘统计（未启用落盘时 FileStats{} 的 Path 为空）。
func (l *Log) FileStats() FileStats {
	l.mu.RLock()
	s := l.sink
	l.mu.RUnlock()
	if s == nil {
		return FileStats{}
	}
	return s.Stats()
}

// TailLines 把文件末尾最多 n 行（按行边界，不会截断一行）写出到 w。
//
// 实现：从文件尾部按块倒读，凑够 n 个换行即停——避免为"看最后 1000 行"而把 10 MB 全读进内存。
func TailLines(f *os.File, n int, w io.Writer) error {
	if n <= 0 {
		n = 1000
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	const block = 64 << 10
	var buf []byte
	pos := size
	for pos > 0 && bytes.Count(buf, []byte{'\n'}) <= n {
		readSize := int64(block)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		chunk := make([]byte, readSize)
		if _, err := f.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return err
		}
		buf = append(chunk, buf...)
	}
	lines := bytes.Split(buf, []byte{'\n'})
	// 末尾若为空行（文件以 \n 结尾）去掉，再取最后 n 行
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, ln := range lines {
		if len(ln) == 0 {
			continue
		}
		if _, err := w.Write(append(ln, '\n')); err != nil {
			return err
		}
	}
	return nil
}
