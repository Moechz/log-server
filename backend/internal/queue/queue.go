// Package queue 实现磁盘持久化队列（至少一次语义，契约 C1）。
//
// 设计：分段文件 + 读位点；Push 追加、Pop 读取不推进、Ack 才推进并删除已消费段。
// 崩溃恢复：只需读位点文件；未 Ack 的数据在下次启动时被重放（不丢，可能重复 → 打 dup 标记）。
package queue

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrFull 队列达到容量上限（上层据此丢弃或降级，FR-305 水位保护）。
var ErrFull = errors.New("queue: 已达容量上限")

// ErrEmpty 当前无数据。
var ErrEmpty = errors.New("queue: 无数据")

// ErrClosed 队列已关闭（关闭后不再接受写入，避免与 Close 竞态）。
var ErrClosed = errors.New("queue: 已关闭")

// Config 队列配置。
type Config struct {
	Dir          string // 队列目录（数据卷）
	MaxBytes     int64  // 上限（0 = 不限）
	SegmentBytes int64  // 单段大小（默认 64MB）
}

// Stats 队列统计（供总览与告警）。
type Stats struct {
	Depth     int64         `json:"depth"`
	Bytes     int64         `json:"bytes"`
	OldestAge time.Duration `json:"oldest_age"`
	Pushed    uint64        `json:"pushed"`
	Acked     uint64        `json:"acked"`
	Retried   uint64        `json:"retried"`
	Dropped   uint64        `json:"dropped"`
	Segments  int           `json:"segments"`
	LastErr   string        `json:"last_err,omitempty"`
}

// Queue 是并发安全的磁盘队列。
type Queue struct {
	cfg Config

	mu      sync.Mutex
	wf      *os.File
	wSeq    int64
	wSize   int64
	rSeq    int64
	rOff    int64
	total   int64
	bytes   int64 // 缓存的总字节数（避免每次 Push 扫描目录）
	oldest  time.Time
	pushed  uint64
	acked   uint64
	retried uint64
	dropped uint64
	lastErr string

	// 状态文件（读位点）落盘节流：每条消息都写临时文件 + rename 会成为吞吐瓶颈。
	// 状态**落后**只会导致已 Ack 的记录被重放（重复，允许）；状态不会超前，故不会丢。
	lastState time.Time
	closed    bool
}

// stateSaveInterval 是 Push 路径上状态落盘的最小间隔。
const stateSaveInterval = time.Second

// Open 打开（或创建）队列目录并恢复位点。
func Open(cfg Config) (*Queue, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("queue: 缺少 Dir")
	}
	if cfg.SegmentBytes <= 0 {
		cfg.SegmentBytes = 64 << 20
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, err
	}
	q := &Queue{cfg: cfg}
	if err := q.loadState(); err != nil {
		return nil, err
	}
	return q, nil
}

// Close 关闭写句柄并持久化位点。
func (q *Queue) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	if q.wf != nil {
		_ = q.wf.Sync()
		_ = q.wf.Close()
		q.wf = nil
	}
	return q.saveState()
}

func (q *Queue) segPath(seq int64) string {
	return filepath.Join(q.cfg.Dir, fmt.Sprintf("seg-%012d.log", seq))
}

func (q *Queue) statePath() string { return filepath.Join(q.cfg.Dir, "state") }

// loadState 从磁盘恢复读写位点；无状态文件时扫描已有段推断。
func (q *Queue) loadState() error {
	b, err := os.ReadFile(q.statePath())
	if err == nil {
		parts := strings.Fields(string(b))
		if len(parts) == 3 {
			q.rSeq, _ = strconv.ParseInt(parts[0], 10, 64)
			q.rOff, _ = strconv.ParseInt(parts[1], 10, 64)
			q.wSeq, _ = strconv.ParseInt(parts[2], 10, 64)
		}
	}
	segs := q.listSegments()
	if len(segs) == 0 {
		q.wSeq = 1
		q.rSeq = 1
		q.rOff = 0
		q.bytes = 0
		return nil
	}
	if q.wSeq == 0 {
		q.wSeq = segs[len(segs)-1]
	}
	if q.rSeq == 0 {
		q.rSeq = segs[0]
	}
	// 读位点若指向不存在的段 → 前移到第一个存在的段
	if !q.segExists(q.rSeq) {
		q.rSeq = segs[0]
		q.rOff = 0
	}
	// 重算深度、字节数与最老时间
	q.bytes = 0
	for _, s := range segs {
		if s < q.rSeq {
			continue
		}
		fi, err := os.Stat(q.segPath(s))
		if err != nil {
			continue
		}
		start := int64(0)
		if s == q.rSeq {
			start = q.rOff
		}
		if start < 0 {
			start = 0
		}
		if start > fi.Size() {
			start = fi.Size()
		}
		// 当前段只统计未消费部分（rOff 之前已被 Ack）。
		q.bytes += fi.Size() - start
		n, err := countRecords(q.segPath(s), start)
		if err != nil {
			continue
		}
		q.total += n
		if q.oldest.IsZero() && fi.ModTime().Before(time.Now()) {
			q.oldest = fi.ModTime()
		}
	}
	if q.wSeq < q.rSeq {
		q.wSeq = q.rSeq
	}
	return nil
}

func (q *Queue) saveState() error {
	body := fmt.Sprintf("%d %d %d\n", q.rSeq, q.rOff, q.wSeq)
	tmp := q.statePath() + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, q.statePath()); err != nil {
		return err
	}
	q.lastState = time.Now()
	return nil
}

// maybeSaveStateLocked 按间隔节流状态落盘（调用方持锁）。
// force=true 用于关闭/段删除等必须立即持久化的场景。
func (q *Queue) maybeSaveStateLocked(force bool) {
	if !force && time.Since(q.lastState) < stateSaveInterval {
		return
	}
	_ = q.saveState()
}

// advanceWriterPastDeletedLocked 在读位点越过写段时，把写指针推到读位点。
//
// 典型竞态：Push 与 Ack 并发时，Ack 把**当前正在写的段**整段删除并 rSeq++，
// 但 wSeq 仍指向已删除的段号；下一次 Push 会重新创建同名段，而 Pop 从新 rSeq 开始
// → 新写入的记录永远读不到（**静默丢数据**）。转发/入库队列都受影响。
// 调用方必须持锁。
func (q *Queue) advanceWriterPastDeletedLocked() {
	if q.wSeq >= q.rSeq {
		return
	}
	if q.wf != nil {
		_ = q.wf.Close()
		q.wf = nil
	}
	q.wSeq = q.rSeq
	q.wSize = 0
}

func (q *Queue) listSegments() []int64 {
	ents, err := os.ReadDir(q.cfg.Dir)
	if err != nil {
		return nil
	}
	var out []int64
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "seg-") || !strings.HasSuffix(n, ".log") {
			continue
		}
		seq, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(n, "seg-"), ".log"), 10, 64)
		if err == nil {
			out = append(out, seq)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (q *Queue) segExists(seq int64) bool {
	_, err := os.Stat(q.segPath(seq))
	return err == nil
}

// Push 追加一条消息（长度前缀帧）。
func (q *Queue) Push(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if len(b) > 8<<20 {
		return fmt.Errorf("queue: 单条超过 8MB")
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return ErrClosed
	}
	q.advanceWriterPastDeletedLocked()
	if q.cfg.MaxBytes > 0 && q.bytes >= q.cfg.MaxBytes {
		q.dropped++
		return ErrFull
	}
	if err := q.ensureWriter(); err != nil {
		return err
	}
	if q.wSize >= q.cfg.SegmentBytes {
		_ = q.wf.Close()
		q.wf = nil
		q.wSeq++
		q.wSize = 0
		if err := q.ensureWriter(); err != nil {
			return err
		}
	}
	// 单次写入完整帧（4 字节长度 + 负载）：两个 syscall 变一个，3000 EPS 下差异明显。
	frame := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(b)))
	copy(frame[4:], b)
	startSize := q.wSize
	if _, err := q.wf.Write(frame); err != nil {
		q.lastErr = err.Error()
		// 回滚半截帧：否则损坏的帧会永久卡住 readRecords（后续写入都在它之后）。
		_ = q.wf.Truncate(startSize)
		_, _ = q.wf.Seek(startSize, 0)
		q.wSize = startSize
		return err
	}
	q.wSize += int64(len(frame))
	q.total++
	q.bytes += int64(len(frame))
	q.pushed++
	if q.oldest.IsZero() {
		q.oldest = time.Now()
	}
	q.maybeSaveStateLocked(false)
	return nil
}

func (q *Queue) ensureWriter() error {
	if q.wf != nil {
		return nil
	}
	f, err := os.OpenFile(q.segPath(q.wSeq), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fi, _ := f.Stat()
	q.wf = f
	if fi != nil {
		q.wSize = fi.Size()
	}
	return nil
}

// Pop 读取最多 max 条（**不推进位点**，需 Ack 确认）。
func (q *Queue) Pop(max int) ([][]byte, error) {
	if max <= 0 {
		max = 128
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.total == 0 {
		return nil, ErrEmpty
	}
	segs := q.listSegments()
	out := make([][]byte, 0, max)
	seq, off := q.rSeq, q.rOff
	for _, s := range segs {
		if s < seq || len(out) >= max {
			continue
		}
		start := int64(0)
		if s == seq {
			start = off
		}
		recs, err := readRecords(q.segPath(s), start, max-len(out))
		if err != nil {
			q.lastErr = err.Error()
			return out, err
		}
		out = append(out, recs...)
		if len(out) >= max {
			break
		}
	}
	if len(out) == 0 {
		return nil, ErrEmpty
	}
	return out, nil
}

// Ack 确认已消费 n 条（推进读位点；整段消费完则删除段文件）。
func (q *Queue) Ack(n int) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	remaining := n
	for remaining > 0 {
		segs := q.listSegments()
		if len(segs) == 0 {
			break
		}
		path := q.segPath(q.rSeq)
		fi, err := os.Stat(path)
		if err != nil {
			// 段不见了：跳到下一个
			q.rSeq++
			q.rOff = 0
			q.advanceWriterPastDeletedLocked()
			if len(segs) > 0 && q.rSeq <= segs[len(segs)-1] {
				continue
			}
			break
		}
		size := fi.Size()
		if q.rOff >= size {
			// 当前段已读完 → 从下一个段开始（并删除本段）
			_ = os.Remove(path)
			q.rSeq++
			q.rOff = 0
			q.advanceWriterPastDeletedLocked()
			continue
		}
		recs, err := readRecords(path, q.rOff, remaining)
		if err != nil || len(recs) == 0 {
			break
		}
		// 计算这些记录占用的字节跨度
		totalLen := int64(0)
		for _, r := range recs {
			totalLen += int64(4 + len(r))
		}
		q.rOff += totalLen
		q.bytes -= totalLen
		if q.bytes < 0 {
			q.bytes = 0
		}
		remaining -= len(recs)
		q.acked += uint64(len(recs))
		q.total -= int64(len(recs))
		if q.total < 0 {
			q.total = 0
		}
		if q.rOff >= size {
			_ = os.Remove(path)
			q.rSeq++
			q.rOff = 0
			q.advanceWriterPastDeletedLocked()
		}
	}
	if q.total == 0 {
		q.oldest = time.Time{}
	}
	_ = q.saveState()
	return nil
}

// Retry 记录一次重试（统计用）。
func (q *Queue) Retry() {
	q.mu.Lock()
	q.retried++
	q.mu.Unlock()
}

// Stats 返回统计快照。
func (q *Queue) Stats() Stats {
	q.mu.Lock()
	defer q.mu.Unlock()
	st := Stats{
		Depth: q.total, Bytes: q.bytes, Pushed: q.pushed, Acked: q.acked,
		Retried: q.retried, Dropped: q.dropped, Segments: len(q.listSegments()),
		LastErr: q.lastErr,
	}
	if !q.oldest.IsZero() {
		st.OldestAge = time.Since(q.oldest)
	}
	return st
}

// ── 帧读写 ────────────────────────────────────────────────────────────

func readRecords(path string, from int64, max int) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(from, 0); err != nil {
		return nil, err
	}
	var out [][]byte
	var hdr [4]byte
	for len(out) < max {
		if _, err := readFull(f, hdr[:]); err != nil {
			break // 尾部不完整 → 留到下轮
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 || n > 8<<20 {
			break // 损坏：停止（保留数据，等待人工/上层处理）
		}
		body := make([]byte, n)
		if _, err := readFull(f, body); err != nil {
			break
		}
		out = append(out, body)
	}
	return out, nil
}

func countRecords(path string, from int64) (int64, error) {
	recs, err := readRecords(path, from, 1<<30)
	return int64(len(recs)), err
}

func readFull(f *os.File, b []byte) (int, error) {
	n, err := f.Read(b)
	if err == nil && n < len(b) {
		// 用 io.ReadFull 语义补齐
		rest := b[n:]
		for len(rest) > 0 {
			m, err2 := f.Read(rest)
			n += m
			if err2 != nil {
				return n, err2
			}
			rest = rest[m:]
		}
	}
	return n, err
}
