// Package forward 实现日志转发（M5 / FR-501~508）。
//
// 核心保证：
//   - **至少一次**：先入磁盘队列，投递成功才 Ack（契约 C1）；失败指数退避重试
//   - **透明转发**：默认重新封装头部但 **payload 无损**（D-004/D-005）
//   - **默认不丢**：队列有上限，到上限才丢弃并计数（FR-305 水位保护）
package forward

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
	"github.com/Moechz/log-server/backend/internal/queue"
)

// Protocol 转发协议。
type Protocol string

const (
	ProtoUDP  Protocol = "udp"
	ProtoTCP  Protocol = "tcp"
	ProtoTLS  Protocol = "tls"
	ProtoHTTP Protocol = "http"
)

// TLSOptions TLS 相关配置（FR-506）。
type TLSOptions struct {
	CAFile             string `json:"ca_file,omitempty"`
	CertFile           string `json:"cert_file,omitempty"`
	KeyFile            string `json:"key_file,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
	PinnedSHA256       string `json:"pinned_sha256,omitempty"` // 证书指纹固定
}

// Filter 是转发过滤规则（FR-503）。
type Filter struct {
	Classes     []string `json:"classes,omitempty"`
	SourceKinds []string `json:"source_kinds,omitempty"`
	Hosts       []string `json:"hosts,omitempty"`
	Apps        []string `json:"apps,omitempty"`
	MinSeverity *int     `json:"min_severity,omitempty"`
	IncludeText string   `json:"include_text,omitempty"`
	ExcludeText string   `json:"exclude_text,omitempty"`
	OnlyAudit   bool     `json:"only_audit,omitempty"`
}

// Empty 表示"不过滤（全转发）"。
func (f Filter) Empty() bool {
	return len(f.Classes) == 0 && len(f.SourceKinds) == 0 && len(f.Hosts) == 0 &&
		len(f.Apps) == 0 && f.MinSeverity == nil && f.IncludeText == "" &&
		f.ExcludeText == "" && !f.OnlyAudit
}

// Match 判断一条记录是否命中过滤。
func (f Filter) Match(r *model.Record) bool {
	if r == nil {
		return false
	}
	if f.Empty() {
		return true
	}
	if f.OnlyAudit && !r.IsAudit {
		return false
	}
	if len(f.Classes) > 0 && !contains(f.Classes, r.ClassPrimary) {
		return false
	}
	if len(f.SourceKinds) > 0 && !contains(f.SourceKinds, string(r.Source.Kind)) {
		return false
	}
	if len(f.Hosts) > 0 && !contains(f.Hosts, r.Host) {
		return false
	}
	if len(f.Apps) > 0 && !contains(f.Apps, r.App) {
		return false
	}
	if f.MinSeverity != nil {
		sev := 7
		if r.SeverityNumber != nil {
			sev = *r.SeverityNumber
		}
		if sev > *f.MinSeverity {
			return false
		}
	}
	if f.IncludeText != "" && !strings.Contains(r.Raw, f.IncludeText) {
		return false
	}
	if f.ExcludeText != "" && strings.Contains(r.Raw, f.ExcludeText) {
		return false
	}
	return true
}

// Target 是一个转发目标（FR-501）。
type Target struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Protocol Protocol   `json:"protocol"`
	Addr     string     `json:"addr"`
	TLS      TLSOptions `json:"tls,omitempty"`
	Filter   Filter     `json:"filter,omitempty"`
	Enabled  bool       `json:"enabled"`
}

// Status 是目标运行状态（供总览 FR-507）。
type Status struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Addr        string      `json:"addr"`
	Protocol    Protocol    `json:"protocol"`
	Enabled     bool        `json:"enabled"`
	Sent        uint64      `json:"sent"`
	Failed      uint64      `json:"failed"`
	LastOK      *time.Time  `json:"last_ok,omitempty"`
	LastErr     string      `json:"last_err,omitempty"`
	LastErrAt   *time.Time  `json:"last_err_at,omitempty"`
	Queue       queue.Stats `json:"queue"`
	Healthy     bool        `json:"healthy"`
	FilterEmpty bool        `json:"filter_empty"`
}

// Config 是 Manager 配置。
type Config struct {
	QueueRoot string // 每目标一个子目录
	MaxBytes  int64
	Timeout   time.Duration
	BatchMax  int
	Logger    *slog.Logger
	// OnSelfLog 用于记录转发自身的异常（防回环：仅写自身日志，不参与转发）
	OnSelfLog func(level, msg string, kv ...string)
}

// Manager 管理全部转发目标。
type Manager struct {
	cfg Config

	mu      sync.RWMutex
	targets map[string]*worker

	wg      sync.WaitGroup
	started bool
	// cancel 由 Start 创建：Close 必须能**自己**停掉发送循环，
	// 否则调用方 defer 顺序一错（先 Close 后 cancel）就会死锁。
	cancel context.CancelFunc
}

type worker struct {
	tgt  Target
	sink Sink
	q    *queue.Queue

	mu        sync.Mutex
	sent      uint64
	failed    uint64
	lastOK    time.Time
	lastErr   string
	lastErrAt time.Time
}

// NewManager 创建管理器。
func NewManager(cfg Config) *Manager {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.BatchMax <= 0 {
		cfg.BatchMax = 128
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Manager{cfg: cfg, targets: map[string]*worker{}}
}

// Apply 应用用户配置（幂等：新增/更新/删除）。
// ★ 已存在且配置未变的目标**保留队列**（升级不丢积压）。
func (m *Manager) Apply(targets []Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	seen := map[string]bool{}
	for _, t := range targets {
		if t.ID == "" {
			return fmt.Errorf("转发目标缺少 id")
		}
		if !validProtocol(t.Protocol) {
			return fmt.Errorf("目标 %s 协议非法: %q", t.ID, t.Protocol)
		}
		if t.Addr == "" {
			return fmt.Errorf("目标 %s 缺少地址", t.ID)
		}
		seen[t.ID] = true

		if w, ok := m.targets[t.ID]; ok {
			if sameTarget(w.tgt, t) { // 完全一致 → 不动
				continue
			}
			// 配置变化：重建 sink（保留队列目录）
			_ = w.sink.Close()
			sink, err := newSink(t, m.cfg.Timeout)
			if err != nil {
				return fmt.Errorf("目标 %s 构建发送端失败: %w", t.ID, err)
			}
			w.sink = sink
			w.tgt = t
			m.cfg.Logger.Info("转发目标已更新", "id", t.ID, "addr", t.Addr)
			continue
		}
		q, err := queue.Open(queue.Config{
			Dir:      m.queueDir(t.ID),
			MaxBytes: m.cfg.MaxBytes,
		})
		if err != nil {
			return fmt.Errorf("目标 %s 打开队列失败: %w", t.ID, err)
		}
		sink, err := newSink(t, m.cfg.Timeout)
		if err != nil {
			_ = q.Close()
			return fmt.Errorf("目标 %s 构建发送端失败: %w", t.ID, err)
		}
		m.targets[t.ID] = &worker{tgt: t, sink: sink, q: q}
		m.cfg.Logger.Info("转发目标已添加", "id", t.ID, "proto", t.Protocol, "addr", t.Addr)
	}

	for id, w := range m.targets {
		if seen[id] {
			continue
		}
		_ = w.sink.Close()
		_ = w.q.Close()
		delete(m.targets, id)
		m.cfg.Logger.Info("转发目标已移除", "id", id)
	}
	return nil
}

// Enqueue 把一条记录扇出到所有匹配目标（不阻塞调用方）。
func (m *Manager) Enqueue(r *model.Record) {
	if r == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, w := range m.targets {
		if !w.tgt.Enabled || !w.tgt.Filter.Match(r) {
			continue
		}
		payload, err := encode(w.tgt.Protocol, r)
		if err != nil {
			w.noteErr(err)
			selfLog(m.cfg, "warn", "转发编码失败", "id", w.tgt.ID, "err", err.Error())
			continue
		}
		if err := w.q.Push(payload); err != nil {
			if errors.Is(err, queue.ErrFull) {
				selfLog(m.cfg, "warn", "转发队列已满，丢弃一条", "id", w.tgt.ID, "addr", w.tgt.Addr)
			} else {
				selfLog(m.cfg, "error", "转发入队失败", "id", w.tgt.ID, "err", err.Error())
			}
		}
	}
}

// Start 为每个目标启动发送协程。
// 注意：Start 会派生一个可取消的子 context，由 Close 负责取消 —— 调用方无需自己 cancel。
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	ctx, m.cancel = context.WithCancel(ctx)
	workers := make([]*worker, 0, len(m.targets))
	for _, w := range m.targets {
		workers = append(workers, w)
	}
	m.mu.Unlock()

	for _, w := range workers {
		m.wg.Add(1)
		go m.loop(ctx, w)
	}
}

// loop 是单目标的发送循环（成功才 Ack；失败指数退避）。
func (m *Manager) loop(ctx context.Context, w *worker) {
	defer m.wg.Done()
	backoff := 200 * time.Millisecond
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		m.mu.RLock()
		enabled := w.tgt.Enabled
		m.mu.RUnlock()
		if !enabled {
			sleepCtx(ctx, time.Second)
			continue
		}

		msgs, err := w.q.Pop(m.cfg.BatchMax)
		if err != nil {
			if !errors.Is(err, queue.ErrEmpty) {
				selfLog(m.cfg, "warn", "读转发队列失败", "id", w.tgt.ID, "err", err.Error())
			}
			backoff = 200 * time.Millisecond
			sleepCtx(ctx, 300*time.Millisecond)
			continue
		}

		sctx, cancel := context.WithTimeout(ctx, m.cfg.Timeout)
		err = w.sink.Send(sctx, msgs)
		cancel()
		if err != nil {
			w.noteErr(err)
			w.q.Retry()
			selfLog(m.cfg, "warn", "转发投递失败（保留队列稍后重试）",
				"id", w.tgt.ID, "addr", w.tgt.Addr, "err", err.Error(), "n", fmt.Sprint(len(msgs)))
			sleepCtx(ctx, backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		if err := w.q.Ack(len(msgs)); err != nil {
			selfLog(m.cfg, "warn", "转发 Ack 失败", "id", w.tgt.ID, "err", err.Error())
		}
		w.noteOK(uint64(len(msgs)))
		backoff = 200 * time.Millisecond
	}
}

// Targets 返回当前目标配置。
func (m *Manager) Targets() []Target {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Target, 0, len(m.targets))
	for _, w := range m.targets {
		out = append(out, w.tgt)
	}
	return out
}

// Stats 返回全部目标状态。
func (m *Manager) Stats() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Status, 0, len(m.targets))
	now := time.Now()
	for _, w := range m.targets {
		w.mu.Lock()
		st := Status{
			ID: w.tgt.ID, Name: w.tgt.Name, Addr: w.tgt.Addr,
			Protocol: w.tgt.Protocol, Enabled: w.tgt.Enabled,
			Sent: w.sent, Failed: w.failed,
			LastErr: w.lastErr, Queue: w.q.Stats(),
			FilterEmpty: w.tgt.Filter.Empty(),
		}
		if !w.lastOK.IsZero() {
			t := w.lastOK
			st.LastOK = &t
		}
		if !w.lastErrAt.IsZero() {
			t := w.lastErrAt
			st.LastErrAt = &t
		}
		// 健康判定：30 秒内成功过 或 队列为空；且无近期错误
		st.Healthy = (st.LastOK != nil && now.Sub(*st.LastOK) < 30*time.Second) ||
			(st.Queue.Depth == 0 && st.LastErr == "")
		w.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// Close 停止并关闭全部目标（队列先落盘）。
// 幂等且自足：即使调用方未 cancel ctx，也能停掉所有发送循环。
func (m *Manager) Close() {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.targets {
		_ = w.sink.Close()
		_ = w.q.Close()
	}
}

func (w *worker) noteOK(n uint64) {
	w.mu.Lock()
	w.sent += n
	w.lastOK = time.Now()
	w.lastErr = ""
	w.mu.Unlock()
}

func (w *worker) noteErr(err error) {
	w.mu.Lock()
	w.failed++
	w.lastErr = err.Error()
	w.lastErrAt = time.Now()
	w.mu.Unlock()
}

func (m *Manager) queueDir(id string) string {
	return m.cfg.QueueRoot + "/" + sanitize(id)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "target"
	}
	return b.String()
}

// sameTarget 比较两个目标配置是否等价（Filter 含切片，不能用 ==）。
func sameTarget(a, b Target) bool {
	if a.ID != b.ID || a.Name != b.Name || a.Protocol != b.Protocol ||
		a.Addr != b.Addr || a.Enabled != b.Enabled || a.TLS != b.TLS {
		return false
	}
	return sameFilter(a.Filter, b.Filter)
}

func sameFilter(a, b Filter) bool {
	if a.IncludeText != b.IncludeText || a.ExcludeText != b.ExcludeText || a.OnlyAudit != b.OnlyAudit {
		return false
	}
	if !sameSlice(a.Classes, b.Classes) || !sameSlice(a.SourceKinds, b.SourceKinds) ||
		!sameSlice(a.Hosts, b.Hosts) || !sameSlice(a.Apps, b.Apps) {
		return false
	}
	if (a.MinSeverity == nil) != (b.MinSeverity == nil) {
		return false
	}
	if a.MinSeverity != nil && *a.MinSeverity != *b.MinSeverity {
		return false
	}
	return true
}

func sameSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validProtocol(p Protocol) bool {
	switch p {
	case ProtoUDP, ProtoTCP, ProtoTLS, ProtoHTTP:
		return true
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func selfLog(cfg Config, level, msg string, kv ...string) {
	if cfg.OnSelfLog != nil {
		cfg.OnSelfLog(level, msg, kv...)
	}
}

// ── 编码（透明转发：重新封装头部，payload 无损）─────────────────────────

// encode 按目标协议编码一条记录。
func encode(p Protocol, r *model.Record) ([]byte, error) {
	if p == ProtoHTTP {
		b, err := json.Marshal(r.VLLog())
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	return encodeRFC5424(r), nil
}

// encodeRFC5424 把记录重新封装为 RFC 5424（payload = 原始消息体，不改内容）。
func encodeRFC5424(r *model.Record) []byte {
	pri := 13 // user.notice 兜底
	if r.Facility != nil && r.SeverityNumber != nil {
		pri = *r.Facility*8 + *r.SeverityNumber
	} else if r.SeverityNumber != nil {
		pri = 1*8 + *r.SeverityNumber
	}
	ts := r.EventTime.UTC().Format(time.RFC3339Nano)
	host := orDash(r.Host)
	app := orDash(r.App)
	pid := "-"
	if r.PID != nil {
		pid = fmt.Sprint(*r.PID)
	}
	// 保留原始保留档案与分类，便于对端归类（放在 SD 里，同时保 payload 原样）
	sd := fmt.Sprintf("[logserver@0 class=\"%s\" retention=\"%s\" trusted=\"%t\"]",
		r.ClassPrimary, r.RetentionProfile, r.TimeTrusted)
	// MSG = 原始消息体（优先 Msg，无则用原文），**不做任何改写**
	msg := r.Msg
	if msg == "" {
		msg = r.Raw
	}
	return []byte(fmt.Sprintf("<%d>1 %s %s %s %s - %s %s", pri, ts, host, app, pid, sd, msg))
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ── Sink 实现 ─────────────────────────────────────────────────────────

// Sink 是投递端点。
type Sink interface {
	Send(ctx context.Context, payloads [][]byte) error
	Close() error
}

func newSink(t Target, timeout time.Duration) (Sink, error) {
	switch t.Protocol {
	case ProtoUDP:
		return &udpSink{addr: t.Addr}, nil
	case ProtoTCP:
		return &tcpSink{addr: t.Addr, timeout: timeout}, nil
	case ProtoTLS:
		cfg, err := buildTLS(t.TLS)
		if err != nil {
			return nil, err
		}
		return &tcpSink{addr: t.Addr, timeout: timeout, tls: cfg}, nil
	case ProtoHTTP:
		return &httpSink{addr: t.Addr, client: &http.Client{Timeout: timeout}}, nil
	default:
		return nil, fmt.Errorf("不支持的协议 %q", t.Protocol)
	}
}

// udpSink：UDP 无连接，逐条发送（UDP 会丢，属已知取舍，D-004）。
type udpSink struct {
	addr string
	mu   sync.Mutex
	conn net.Conn
}

func (s *udpSink) Send(_ context.Context, payloads [][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		c, err := net.Dial("udp", s.addr)
		if err != nil {
			return err
		}
		s.conn = c
	}
	for _, p := range payloads {
		if _, err := s.conn.Write(p); err != nil {
			_ = s.conn.Close()
			s.conn = nil
			return err
		}
	}
	return nil
}

func (s *udpSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

// tcpSink：octet-counting 帧（RFC 6587），一条连接复用；失败即断连重试。
type tcpSink struct {
	addr    string
	timeout time.Duration
	tls     *tls.Config

	mu   sync.Mutex
	conn net.Conn
}

func (s *tcpSink) Send(ctx context.Context, payloads [][]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		d := net.Dialer{Timeout: s.timeout}
		var c net.Conn
		var err error
		if s.tls != nil {
			c, err = tls.DialWithDialer(&d, "tcp", s.addr, s.tls)
		} else {
			c, err = d.DialContext(ctx, "tcp", s.addr)
		}
		if err != nil {
			return err
		}
		s.conn = c
	}
	deadline := time.Now().Add(s.timeout)
	_ = s.conn.SetWriteDeadline(deadline)
	for _, p := range payloads {
		// octet-counting：`<len> SP <msg>`
		frame := fmt.Sprintf("%d %s", len(p), p)
		if _, err := s.conn.Write([]byte(frame)); err != nil {
			_ = s.conn.Close()
			s.conn = nil
			return err
		}
	}
	return nil
}

func (s *tcpSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		err := s.conn.Close()
		s.conn = nil
		return err
	}
	return nil
}

// httpSink：NDJSON 批量 POST（Loki/ES/OpenObserve 兼容子集）。
type httpSink struct {
	addr   string
	client *http.Client
}

func (s *httpSink) Send(ctx context.Context, payloads [][]byte) error {
	var b strings.Builder
	for _, p := range payloads {
		b.Write(p)
		b.WriteByte('\n')
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.addr, strings.NewReader(b.String()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/stream+json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *httpSink) Close() error { return nil }

// buildTLS 构造 TLS 配置（支持自签 CA、双向证书、指纹固定、跳过校验）。
func buildTLS(o TLSOptions) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("读取 CA 失败: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA 文件不含有效证书")
		}
		cfg.RootCAs = pool
	}
	if o.CertFile != "" && o.KeyFile != "" {
		crt, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("加载客户端证书失败: %w", err)
		}
		cfg.Certificates = []tls.Certificate{crt}
	}
	if o.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	if o.PinnedSHA256 != "" {
		want := strings.ToLower(strings.ReplaceAll(o.PinnedSHA256, ":", ""))
		cfg.InsecureSkipVerify = true // 由下面的回调自行校验
		cfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("对端未提供证书")
			}
			sum := sha256.Sum256(rawCerts[0])
			got := hex.EncodeToString(sum[:])
			if got != want {
				return fmt.Errorf("证书指纹不匹配: got %s want %s", got, want)
			}
			return nil
		}
	}
	return cfg, nil
}
