// Package selflog 实现**应用自身日志**（FR-1101/1102）。
//
// 两条防坑设计：
//   - 防自噬（FR-1102）：内存环形缓冲有**硬上限**（条数固定），绝不让自己的日志撑爆自己；
//   - 防回环（FR-1103）：本缓冲**不参与转发**，且不写入 VL 的普通流（由上层保证）。
package selflog

import (
	"sort"
	"sync"
	"time"
)

// Entry 是一条应用自身日志。
type Entry struct {
	Ts     time.Time         `json:"ts"`
	Level  string            `json:"level"`
	Module string            `json:"module"`
	Msg    string            `json:"msg"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Log 是固定容量的环形缓冲。
type Log struct {
	mu       sync.RWMutex
	buf      []Entry
	next     int
	full     bool
	capacity int
	total    int64
	dropped  int64
	byLevel  map[string]int64
	// sink 是可选落盘（见 file.go）；nil = 仅内存。写入在锁外进行，避免与 API 读锁互等。
	sink *FileSink
}

// New 创建容量为 capacity 条的环形缓冲（默认 2000）。
func New(capacity int) *Log {
	if capacity <= 0 {
		capacity = 2000
	}
	return &Log{buf: make([]Entry, capacity), capacity: capacity, byLevel: map[string]int64{}}
}

// Add 写入一条（O(1)；满则覆盖最旧 —— 这就是防自噬的硬上限）。
func (l *Log) Add(level, module, msg string, kv ...string) {
	e := Entry{Ts: time.Now().UTC(), Level: level, Module: module, Msg: msg}
	if len(kv) > 0 {
		e.Fields = make(map[string]string, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			e.Fields[kv[i]] = kv[i+1]
		}
	}
	l.mu.Lock()
	if l.full {
		l.dropped++
	}
	l.buf[l.next] = e
	l.next = (l.next + 1) % l.capacity
	if l.next == 0 {
		l.full = true
	}
	l.total++
	l.byLevel[level]++
	sink := l.sink
	l.mu.Unlock()

	// 落盘在锁外（FileSink 自己串行化；写失败不影响内存缓冲与业务）
	if sink != nil {
		if err := sink.Write(e); err != nil {
			sink.recordError()
		}
	}
}

// Recent 返回最近 n 条（新的在前）。
func (l *Log) Recent(n int) []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()

	size := l.next
	if l.full {
		size = l.capacity
	}
	if n <= 0 || n > size {
		n = size
	}
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		idx := l.next - 1 - i
		for idx < 0 {
			idx += l.capacity
		}
		out = append(out, l.buf[idx])
	}
	return out
}

// Stats 返回自身日志统计（用于总览与水位优先级）。
type Stats struct {
	Capacity int              `json:"capacity"`
	Used     int              `json:"used"`
	Total    int64            `json:"total"`
	Dropped  int64            `json:"dropped"`
	ByLevel  map[string]int64 `json:"by_level"`
}

// Stats 返回统计快照。
func (l *Log) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	used := l.next
	if l.full {
		used = l.capacity
	}
	by := make(map[string]int64, len(l.byLevel))
	for k, v := range l.byLevel {
		by[k] = v
	}
	// 排序用：保证输出稳定
	keys := make([]string, 0, len(by))
	for k := range by {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return Stats{Capacity: l.capacity, Used: used, Total: l.total, Dropped: l.dropped, ByLevel: by}
}

// MemBytes 估算内存占用（用于「水位告警时优先清理 self log」的决策）。
func (l *Log) MemBytes() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var n int64
	for i := range l.buf {
		e := &l.buf[i]
		n += int64(len(e.Level) + len(e.Module) + len(e.Msg))
		for k, v := range e.Fields {
			n += int64(len(k) + len(v))
		}
	}
	return n
}
