// Package aggregate 提供**应用层滚动窗口计数器**（FR-1105）。
//
// 为什么重要：VictoriaLogs 没有物化视图，**总览页的唯一数据源就是这里**
// （TECH-DESIGN §6.5）。总览**绝不直接扫原始日志**（FR-1204 硬约束）。
package aggregate

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

// Row 是一个聚合行。
type Row struct {
	Bucket         time.Time `json:"bucket"`
	ClassPrimary   string    `json:"class_primary"`
	SourceKind     string    `json:"source_kind"`
	Host           string    `json:"host"`
	App            string    `json:"app"`
	SeverityNumber int       `json:"severity_number"`
	Count          int64     `json:"count"`
	Bytes          int64     `json:"bytes"`
}

type series struct {
	count int64
	bytes int64
}

type bucket struct {
	at     time.Time
	series map[string]*series
}

// Counters 是分钟桶的滚动窗口计数器（固定内存，与日志量解耦）。
type Counters struct {
	mu      sync.Mutex
	buckets map[int64]*bucket // key = unix 分钟
	window  time.Duration
	missed  int64 // 因桶淘汰而丢弃的计数（用于诊断）
}

// New 创建窗口为 window 的计数器（建议 24h）。
func New(window time.Duration) *Counters {
	if window <= 0 {
		window = 24 * time.Hour
	}
	return &Counters{buckets: map[int64]*bucket{}, window: window}
}

func key(class, kind, host, app string, sev int) string {
	return class + "\x1f" + kind + "\x1f" + host + "\x1f" + app + "\x1f" + itoa(sev)
}

// Add 记录一条日志（O(1)，不涉及 IO）。
func (c *Counters) Add(r *model.Record) {
	if r == nil {
		return
	}
	sev := -1
	if r.SeverityNumber != nil {
		sev = *r.SeverityNumber
	}
	k := key(r.ClassPrimary, string(r.Source.Kind), r.Host, r.App, sev)
	at := r.IngestTime.Truncate(time.Minute).UTC()
	mk := at.Unix() / 60

	c.mu.Lock()
	defer c.mu.Unlock()

	b, ok := c.buckets[mk]
	if !ok {
		b = &bucket{at: at, series: map[string]*series{}}
		c.buckets[mk] = b
		c.evictLocked(at)
	}
	s, ok := b.series[k]
	if !ok {
		s = &series{}
		b.series[k] = s
	}
	s.count++
	s.bytes += int64(len(r.Raw))
}

// evictLocked 淘汰窗口外的桶（调用方持锁）。
func (c *Counters) evictLocked(now time.Time) {
	cutoff := now.Add(-c.window).Unix() / 60
	for mk, b := range c.buckets {
		if mk < cutoff {
			for _, s := range b.series {
				c.missed += s.count
			}
			delete(c.buckets, mk)
		}
	}
}

// Snapshot 返回窗口内的聚合行。
func (c *Counters) Snapshot(since time.Time) []Row {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []Row
	for _, b := range c.buckets {
		if b.at.Before(since) {
			continue
		}
		for k, s := range b.series {
			p := strings.Split(k, "\x1f")
			out = append(out, Row{
				Bucket: b.at, ClassPrimary: p[0], SourceKind: p[1],
				Host: p[2], App: p[3], SeverityNumber: atoi(p[4]),
				Count: s.count, Bytes: s.bytes,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Bucket.Equal(out[j].Bucket) {
			return out[i].Bucket.Before(out[j].Bucket)
		}
		return out[i].Count > out[j].Count
	})
	return out
}

// Series 返回按 bucket 归并的总数（用于趋势图）。
func (c *Counters) Series(since time.Time) map[time.Time]int64 {
	out := map[time.Time]int64{}
	for _, r := range c.Snapshot(since) {
		out[r.Bucket] += r.Count
	}
	return out
}

// TopN 按指定维度取前 N（维度：class_primary | source_kind | host | app | severity）。
func (c *Counters) TopN(since time.Time, field string, n int) []Term {
	sum := map[string]int64{}
	for _, r := range c.Snapshot(since) {
		var v string
		switch field {
		case "class_primary":
			v = r.ClassPrimary
		case "source_kind":
			v = r.SourceKind
		case "host":
			v = r.Host
		case "app":
			v = r.App
		case "severity_number":
			v = itoa(r.SeverityNumber)
		default:
			return nil
		}
		if v == "" {
			v = "(unknown)"
		}
		sum[v] += r.Count
	}
	out := make([]Term, 0, len(sum))
	for k, v := range sum {
		out = append(out, Term{Value: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

// Totals 汇总（按分类与严重级别）。
type Totals struct {
	Total        int64            `json:"total"`
	Bytes        int64            `json:"bytes"`
	ByClass      map[string]int64 `json:"by_class"`
	BySeverity   map[string]int64 `json:"by_severity"`
	ByClassBytes map[string]int64 `json:"by_class_bytes"`
	Missed       int64            `json:"missed"` // 窗口淘汰计数（诊断用）
}

// Totals 返回窗口内的汇总。
func (c *Counters) Totals(since time.Time) Totals {
	t := Totals{ByClass: map[string]int64{}, BySeverity: map[string]int64{}, ByClassBytes: map[string]int64{}}
	for _, r := range c.Snapshot(since) {
		t.Total += r.Count
		t.Bytes += r.Bytes
		cl := r.ClassPrimary
		if cl == "" {
			cl = "(unknown)"
		}
		t.ByClass[cl] += r.Count
		t.ByClassBytes[cl] += r.Bytes
		t.BySeverity[severityLabel(r.SeverityNumber)] += r.Count
	}
	c.mu.Lock()
	t.Missed = c.missed
	c.mu.Unlock()
	return t
}

// Term 是 TOP N 项。
type Term struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// Files 返回桶数量（诊断）。
func (c *Counters) Files() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buckets)
}

func severityLabel(n int) string {
	switch {
	case n < 0:
		return "unknown"
	case n <= 3:
		return "err"
	case n == 4:
		return "warning"
	case n <= 6:
		return "info"
	default:
		return "debug"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func atoi(s string) int {
	n, neg := 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		return -n
	}
	return n
}
