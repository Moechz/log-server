// Package vlstore 是 Store 接口的 **VictoriaLogs** 实现（主选引擎，D-007）。
//
// 只用标准库 HTTP 调用 VL 的官方接口，**不引入任何外部依赖**（V6/可复现友好）：
//   - 写入：POST /insert/jsonline   （NDJSON）
//   - 健康：GET  /health
//   - 查询：GET  /select/logsql/query
//   - 容量：直接统计 storageDataPath 的磁盘占用 + statfs
//
// 契约遵守（TECH-DESIGN §5.3）：C1 至少一次 / C2 幂等友好 / C6 Usage 廉价（缓存）/
// C8 流式 / C9 UTC / C10 不向上暴露引擎细节。
package vlstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
	"github.com/Moechz/log-server/backend/internal/store"
)

const (
	maxQueryLimitDefault = 1000 // 契约 C4：Limit 硬上限 1000
	usageCacheTTL        = 30 * time.Second
)

// Store 实现 store.Store。
type Store struct {
	cfg  store.Config
	base string
	hc   *http.Client

	usageMu     sync.Mutex
	usageCached store.Usage
	usageAt     time.Time
}

// New 创建 VL 存储适配器。
func New() *Store { return &Store{} }

// Open 打开连接（不阻塞；VL 可能稍后才就绪，探测由 Ping 负责）。
func (s *Store) Open(_ context.Context, cfg store.Config) error {
	if cfg.Addr == "" {
		return fmt.Errorf("vlstore: 缺少 Addr")
	}
	s.cfg = cfg
	s.base = "http://" + cfg.Addr
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	s.hc = &http.Client{Timeout: timeout}
	return nil
}

// Close 无状态，直接返回。
func (s *Store) Close() error { return nil }

// Ping 检查 VL 是否就绪。
func (s *Store) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("vlstore: 存储引擎不可达: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("vlstore: /health 返回 %d", resp.StatusCode)
	}
	return nil
}

// Capabilities 自述能力（D-007：VL 非 SQL 引擎，不支持 JOIN/子查询）。
func (s *Store) Capabilities() store.Capabilities {
	return store.Capabilities{
		Engine:              "victorialogs",
		FullTextSearch:      true, // 默认全字段索引
		RegexSearch:         true,
		FieldSearch:         true,
		NativeTTL:           true,  // -retentionPeriod
		NativeAggregate:     false, // 无物化视图（总览走应用层计数器，见 TECH-DESIGN §6.5）
		SQL:                 false,
		MaxRetentionDays:    3650,
		PerProfileRetention: false, // 只有全局 -retentionPeriod；分档需应用层清理任务（未实现）
	}
}

// Write 至少一次写入（C1/C2）。raw 行**逐字节不改**（D-005①）。
func (s *Store) Write(ctx context.Context, batch []*model.Record) error {
	if len(batch) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range batch {
		if r == nil {
			continue
		}
		if err := r.Validate(); err != nil {
			return fmt.Errorf("vlstore: 记录校验失败: %w", err)
		}
		if err := enc.Encode(r.VLLog()); err != nil {
			return fmt.Errorf("vlstore: 序列化失败: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.base+"/insert/jsonline", bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/stream+json")

	resp, err := s.hc.Do(req)
	if err != nil {
		return fmt.Errorf("vlstore: 写入失败（上层应保留队列不 ack）: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("vlstore: 写入被拒绝 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// Query 检索。VL 无游标概念 → 以 _time 边界实现「翻页」（TECH-DESIGN §6.x / 契约 C4）。
func (s *Store) Query(ctx context.Context, q store.Query) (*store.Result, error) {
	limit := q.Limit
	if limit <= 0 || limit > maxQueryLimitDefault {
		limit = maxQueryLimitDefault
	}
	// ★ 显式按时间倒序：VL 默认返回顺序是“任意”（官方文档明说），
	//   而检索展示与游标翻页（NextCursor = 本页最后一条 _time）都依赖时间序。
	lq := buildLogsQL(q, 0) + " | sort by (_time) desc | limit " + strconv.Itoa(limit)

	vals := url.Values{}
	vals.Set("query", lq)
	vals.Set("limit", strconv.Itoa(limit))
	if !q.Range.From.IsZero() {
		vals.Set("start", q.Range.From.UTC().Format(time.RFC3339Nano))
	}
	if !q.Range.To.IsZero() {
		vals.Set("end", q.Range.To.UTC().Format(time.RFC3339Nano))
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.base+"/select/logsql/query?"+vals.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vlstore: 查询失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("vlstore: 查询被拒绝 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	res := &store.Result{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var lastTime string
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			continue // 解析失败不中断整批（C1 精神）
		}
		res.Logs = append(res.Logs, m)
		if ts, ok := m["_time"].(string); ok {
			lastTime = ts
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("vlstore: 读取结果流失败: %w", err)
	}
	res.TookMS = time.Since(start).Milliseconds()
	res.Truncated = len(res.Logs) >= limit
	if res.Truncated && lastTime != "" {
		res.NextCursor = lastTime // 上层把 NextCursor 作为下一次的 Range.To
	}
	return res, nil
}

// Histogram 用 logsql 管道做时间分桶统计。
func (s *Store) Histogram(ctx context.Context, q store.Query, interval time.Duration) ([]store.BucketCount, error) {
	if interval <= 0 {
		interval = time.Minute
	}
	lq := fmt.Sprintf("%s | stats by (_time:%s) count() as c",
		buildLogsQL(q, 0), fmtInterval(interval))

	vals := url.Values{}
	vals.Set("query", lq)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.base+"/select/logsql/stats_query?"+vals.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vlstore: 直方图查询失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("vlstore: 直方图被拒绝 %d", resp.StatusCode)
	}

	var out []store.BucketCount
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			continue
		}
		ts, _ := row["_time"].(string)
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue
		}
		out = append(out, store.BucketCount{Bucket: t.UTC(), Count: toInt64(row["c"])})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// VL 返回的桶是乱序的 → 按时间升序，前端可直接绘制
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket.Before(out[j].Bucket) })
	return out, nil
}

// Usage 容量统计（契约 C6：可缓存，不得每次全量统计）。
func (s *Store) Usage(ctx context.Context) (store.Usage, error) {
	s.usageMu.Lock()
	if time.Since(s.usageAt) < usageCacheTTL && s.usageAt != (time.Time{}) {
		u := s.usageCached
		s.usageMu.Unlock()
		return u, nil
	}
	s.usageMu.Unlock()

	u := store.Usage{}
	if s.cfg.StoragePath != "" {
		u.TotalBytes = dirSize(s.cfg.StoragePath)
		var st syscall.Statfs_t
		if err := syscall.Statfs(s.cfg.StoragePath, &st); err == nil {
			u.DiskTotalBytes = st.Blocks * uint64(st.Bsize)
			u.DiskFreeBytes = st.Bavail * uint64(st.Bsize)
		}
	}
	if u.DiskTotalBytes > 0 {
		used := u.DiskTotalBytes - u.DiskFreeBytes
		u.WatermarkPct = float64(used) / float64(u.DiskTotalBytes) * 100
	}

	s.usageMu.Lock()
	s.usageCached, s.usageAt = u, time.Now()
	s.usageMu.Unlock()
	return u, nil
}

// fmtInterval 把时长格式化为 VictoriaLogs 能识别的紧凑写法（1m / 1h / 1d）。
func fmtInterval(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// Context 按时间点取前后 N 条（FR-406），与 VL 官方 UI 同口径。
func (s *Store) Context(ctx context.Context, q store.Query, pivot time.Time, before, after int) (store.ContextResult, error) {
	var res store.ContextResult
	base := buildLogsQL(q, 0)

	if before > 0 {
		lq := base + " | sort by (_time) desc | limit " + strconv.Itoa(before)
		logs, err := s.runQuery(ctx, lq, time.Time{}, pivot)
		if err != nil {
			return res, err
		}
		// desc → 正序，便于按“上一行 → pivot”顺序阅读
		for i, j := 0, len(logs)-1; i < j; i, j = i+1, j-1 {
			logs[i], logs[j] = logs[j], logs[i]
		}
		res.Before = logs
	}
	if after > 0 {
		lq := base + " | sort by (_time) asc | limit " + strconv.Itoa(after)
		logs, err := s.runQuery(ctx, lq, pivot.Add(time.Nanosecond), time.Time{})
		if err != nil {
			return res, err
		}
		res.After = logs
	}
	return res, nil
}

// runQuery 执行一次 LogsQL 查询（仅供 Context 使用；start/end 为空则不限制）。
func (s *Store) runQuery(ctx context.Context, lq string, start, end time.Time) ([]map[string]any, error) {
	vals := url.Values{}
	vals.Set("query", lq)
	if !start.IsZero() {
		vals.Set("start", start.UTC().Format(time.RFC3339Nano))
	}
	if !end.IsZero() {
		vals.Set("end", end.UTC().Format(time.RFC3339Nano))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.base+"/select/logsql/query?"+vals.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vlstore: 上下文查询失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("vlstore: 上下文查询被拒绝 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out, sc.Err()
}

// EnsureProfile VL 无 per-stream 保留配置接口 → 返回 ErrUnsupported，
// 上层改用「全局 retentionPeriod + 条件删除任务」（TECH-DESIGN §6.4 方案 A/B 待 dev 验证）。
func (s *Store) EnsureProfile(context.Context, store.RetentionProfile) error {
	return store.ErrUnsupported
}

// ── 内部工具 ──────────────────────────────────────────────────────────

// buildLogsQL 把我们的 Query 翻译为 LogsQL（**引擎细节只存在于本包**）。
func buildLogsQL(q store.Query, limit int) string {
	var parts []string
	if q.Text != "" {
		parts = append(parts, quoteWord(q.Text))
	}
	for _, c := range q.Classes {
		parts = append(parts, fmt.Sprintf("ls_classes:%s", quoteWord(c)))
	}
	for _, k := range q.SourceKinds {
		parts = append(parts, fmt.Sprintf("ls_source_kind:%s", quoteWord(k)))
	}
	for _, h := range q.Hosts {
		parts = append(parts, fmt.Sprintf("ls_host:%s", quoteWord(h)))
	}
	for _, a := range q.Apps {
		parts = append(parts, fmt.Sprintf("ls_app:%s", quoteWord(a)))
	}
	if q.MinSeverity != nil {
		parts = append(parts, fmt.Sprintf("ls_severity_number:<=%d", *q.MinSeverity))
	}
	if q.IsAudit != nil {
		parts = append(parts, fmt.Sprintf("ls_is_audit:%t", *q.IsAudit))
	}
	if q.OnlyTrustedTime {
		parts = append(parts, "ls_time_trusted:true")
	}
	if len(parts) == 0 {
		parts = append(parts, "*")
	}
	s := strings.Join(parts, " ")
	if limit > 0 {
		s += fmt.Sprintf(" | limit %d", limit)
	}
	return s
}

func quoteWord(s string) string {
	if strings.ContainsAny(s, " \t\"'|:") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

// dirSize 统计目录占用（用量小、可缓存，故直接遍历）。
func dirSize(root string) uint64 {
	var total uint64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}
