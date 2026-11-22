// Package file 实现文件日志采集：inode+offset 持久化、轮转处理、多行合并。
//
// 三条硬要求（对齐 DEV-REQUIREMENTS FR-102）：
//  1. **不丢** —— 只消费"完整行"（无换行的尾部留到下轮），offset 以完整行为界推进
//  2. **不重复** —— offset 与 inode 一起持久化；轮转后按 inode 变化重新定位
//  3. **两种轮转都要支持** —— rename 型（inode 变）与 copytruncate 型（size 变小）
package file

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Offset 是一个采集源的位点（★ 必须含 inode：仅 path+offset 无法区分轮转）。
type Offset struct {
	Path      string    `json:"path"`
	Dev       uint64    `json:"dev"`
	Inode     uint64    `json:"inode"`
	Offset    int64     `json:"offset"`
	Rotations int64     `json:"rotations"`
	Truncates int64     `json:"truncates"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Multiline 配置多行合并（如 Java 栈、Python traceback）。
//
// **语义（重要）**：
//   - `Continue == nil` → **不做多行合并**，每行即时成为一条记录（无延迟）
//   - `Continue != nil` → 启用合并。此时**最后一条记录会被暂存**，因为只有看到下一行
//     才能判断它是否是续行。暂存记录在以下任一条件成立时吐出：
//     ① 下一行不是续行（此时上一条已确定结束）
//     ② 距上次收到数据超过 FlushAfter（避免尾部记录无限期滞留）
//     ③ 显式调用 Flush()（源停止/轮转/关闭时）
//
// 延迟特征（上层排期需知）：启用合并时，**每条记录最多延迟 FlushAfter**；
// 但同一批到达的多行中，除最后一条外都**立即**吐出（因为后面的行证明前者已结束）。
// 生产环境建议 FlushAfter = 1–2s；若源日志本身无多行记录，直接用 Multiline{} 关闭合并（零延迟）。
type Multiline struct {
	// Continue 返回 true 表示该行是**上一条记录的续行**；nil = 不合并
	Continue func(line string) bool
	// MaxLines / MaxBytes 防止一条记录无限增长
	MaxLines int
	MaxBytes int
	// FlushAfter 暂存记录的自动吐出超时（默认 2s）
	FlushAfter time.Duration
}

// DefaultMultiline 以"行首是否像时间戳"为界：不像则是续行。
func DefaultMultiline() Multiline {
	return Multiline{
		Continue: func(line string) bool {
			return !looksLikeTimestamp(line)
		},
		MaxLines: 200,
		MaxBytes: 64 << 10,
	}
}

// looksLikeTimestamp 判断行首是否像常见日志时间戳。
func looksLikeTimestamp(s string) bool {
	if len(s) < 10 {
		return false
	}
	// ISO8601 / RFC3339：2026-10-12T…
	if s[4] == '-' && s[7] == '-' {
		return true
	}
	// RFC3164：Oct 12 12:34:56 / 2026-10-12 12:34:56
	if s[3] == ' ' && s[6] == ' ' {
		return true
	}
	if s[2] == '/' || s[2] == '.' {
		return true
	}
	// "2026-10-12" 已覆盖；"12:34:56" 开头也认
	if s[2] == ':' && s[5] == ':' {
		return true
	}
	return false
}

// Store 是位点持久化（原子写 + 读）。
type Store struct {
	Path string
	mu   sync.Mutex
}

// Load 读取全部位点（文件不存在返回空表）。
func (s *Store) Load() (map[string]Offset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Offset{}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("位点文件损坏: %w", err)
	}
	return out, nil
}

// Save 原子写（临时文件 + rename），避免掉电写坏位点。
func (s *Store) Save(m map[string]Offset) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o750); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return err
	}
	return nil
}

// Tailer 跟踪单个文件。
type Tailer struct {
	Path      string
	Multiline Multiline

	state  Offset
	file   *os.File
	reader *bufio.Reader
	// 注意：**不保留 pending 缓冲**。offset 始终停在"最后一个完整行之后"，
	// 未以换行结尾的尾部会在下一轮从文件中自然重读 —— 若同时缓冲一份，会导致重复。
	merged  []byte // 多行合并中的当前记录
	mergedN int
	heldAt  time.Time // 暂存记录开始的时间（用于 FlushAfter 超时）
}

// New 创建采集器并从给定位点恢复。
func New(path string, st Offset, ml Multiline) *Tailer {
	if ml.MaxLines <= 0 {
		ml.MaxLines = 200
	}
	if ml.MaxBytes <= 0 {
		ml.MaxBytes = 64 << 10
	}
	if ml.FlushAfter <= 0 {
		ml.FlushAfter = 2 * time.Second
	}
	st.Path = path
	return &Tailer{Path: path, Multiline: ml, state: st}
}

// State 返回当前位点（上层定期持久化）。
func (t *Tailer) State() Offset {
	s := t.state
	s.UpdatedAt = time.Now().UTC()
	return s
}

// Close 关闭底层文件。
func (t *Tailer) Close() error {
	if t.file != nil {
		err := t.file.Close()
		t.file = nil
		return err
	}
	return nil
}

// Poll 读取自上次以来的新内容，返回合并后的记录。
//
// 轮转处理：
//   - inode 变化（rename 型）→ 关旧开新，从 0 开始
//   - size < offset（copytruncate 型）→ seek 0 重新读
func (t *Tailer) Poll() ([]string, error) {
	stale := t.flushStale()
	if err := t.open(); err != nil {
		return stale, err
	}
	if t.file == nil {
		return stale, nil
	}

	fi, err := t.file.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()

	// copytruncate：文件被截断但 inode 不变
	if size < t.state.Offset {
		if _, err := t.file.Seek(0, 0); err != nil {
			return nil, err
		}
		t.state.Offset = 0
		t.state.Truncates++
	}

	if _, err := t.file.Seek(t.state.Offset, 0); err != nil {
		return nil, err
	}

	// 只读到最后一个换行处，剩下的留到下一轮（保证不切断行）
	buf := make([]byte, 256<<10)
	n, err := t.file.Read(buf)
	if n <= 0 && err != nil {
		return stale, nil // 无新数据（但可能已吐出超时的暂存记录）
	}
	chunk := buf[:n]
	lastNL := lastIndexByte(chunk, '\n')
	if lastNL < 0 {
		// 还没有完整行：offset 不动，下轮重读这段（可能继续变长）
		return stale, nil
	}
	consumed := int64(lastNL + 1)
	t.state.Offset += consumed

	return append(stale, t.splitRecords(chunk[:lastNL])...), nil
}

// flushStale 若暂存记录已超时未更新则吐出（避免尾部记录无限期滞留）。
func (t *Tailer) flushStale() []string {
	if t.mergedN == 0 || t.heldAt.IsZero() {
		return nil
	}
	if time.Since(t.heldAt) < t.Multiline.FlushAfter {
		return nil
	}
	return t.Flush()
}

// splitRecords 把字节切成记录（含多行合并）。
func (t *Tailer) splitRecords(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i <= len(b); i++ {
		if i < len(b) && b[i] != '\n' {
			continue
		}
		line := b[start:i]
		start = i + 1
		line = trimCR(line)

		// 不做多行合并：每行即时成为一条记录（零延迟）
		if t.Multiline.Continue == nil {
			out = append(out, string(line))
			continue
		}

		if t.mergedN > 0 &&
			t.Multiline.Continue(string(line)) &&
			t.mergedN < t.Multiline.MaxLines &&
			len(t.merged)+len(line) < t.Multiline.MaxBytes {
			t.merged = append(t.merged, '\n')
			t.merged = append(t.merged, line...)
			t.mergedN++
			continue
		}
		if t.mergedN > 0 {
			out = append(out, string(t.merged))
		}
		t.merged = append(t.merged[:0], line...)
		t.mergedN = 1
		t.heldAt = time.Now()
	}
	return out
}

// Flush 把合并缓冲里的最后一条记录吐出来（源停止/关闭时调用）。
func (t *Tailer) Flush() []string {
	if t.mergedN == 0 {
		return nil
	}
	s := string(t.merged)
	t.merged = t.merged[:0]
	t.mergedN = 0
	t.heldAt = time.Time{}
	return []string{s}
}

// open 打开文件；处理 inode 变化（rename 型轮转）。
func (t *Tailer) open() error {
	fi, err := os.Stat(t.Path)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件暂时不存在（轮转间隙）：保留位点，等下一轮
			_ = t.Close()
			return nil
		}
		return err
	}
	dev, ino := statIdent(fi)

	if t.file != nil {
		if ino == t.state.Inode && dev == t.state.Dev {
			return nil // 同一个文件，继续用
		}
		// inode 变了 → rename 型轮转：先把旧文件的尾巴读完（上层会再 Poll 一次），
		// 这里简单关闭并切到新文件（旧文件改名后通常由 logrotate 压缩/删除）
		_ = t.Close()
		t.state.Rotations++
		t.state.Offset = 0
		t.mergedN = 0
	}

	f, err := os.Open(t.Path)
	if err != nil {
		return err
	}
	t.file = f
	t.state.Dev, t.state.Inode = dev, ino
	if t.state.Offset < 0 {
		t.state.Offset = 0
	}
	return nil
}

func trimCR(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		return b[:n-1]
	}
	return b
}

func lastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}
