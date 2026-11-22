package spool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

// fakeWriter 是可控故障的存储写入器（只实现 spool 需要的 Write）。
type fakeWriter struct {
	mu         sync.Mutex
	recs       []*model.Record
	writes     int
	failFirst  int
	failAlways bool
	delay      time.Duration
}

func (f *fakeWriter) Write(ctx context.Context, batch []*model.Record) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	if f.failAlways {
		return errors.New("fake: 存储不可用")
	}
	if f.failFirst > 0 {
		f.failFirst--
		return errors.New("fake: 暂时失败")
	}
	f.recs = append(f.recs, batch...)
	return nil
}

func (f *fakeWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recs)
}

func (f *fakeWriter) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func (f *fakeWriter) snapshot() []*model.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*model.Record, len(f.recs))
	copy(out, f.recs)
	return out
}

func mkRecord(i int) *model.Record {
	sev := 6
	return &model.Record{
		Raw:        fmt.Sprintf("<134>Nov 22 10:00:00 host%d app[%d]: hello %d", i%3, i, i),
		IngestTime: time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC),
		Source: model.Source{
			Kind: model.SrcRemoteSyslog, IP: "10.0.0.5", Port: 514,
			Protocol: "udp", Device: "switch", Collector: "c1",
		},
		Encoding: "utf-8", RetentionProfile: "ops-14d",
		EventTime:   time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC),
		TimeTrusted: true, Host: fmt.Sprintf("host%d", i%3), App: "app",
		Level: "info", SeverityNumber: &sev, Format: "rfc5424",
		ClassPrimary: "system", Classes: []string{"system"}, Msg: fmt.Sprintf("hello %d", i),
		User: map[string]any{"seq": float64(i), "tag": "t"},
	}
}

func newTestSpool(t *testing.T, dir string, w Writer, maxBytes int64) *Spool {
	t.Helper()
	s, err := Open(Config{
		Dir: dir, MaxBytes: maxBytes, Store: w,
		BatchSize: 16, WriteTimeout: time.Second,
		RetryMin: 5 * time.Millisecond, RetryMax: 40 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func waitFor(t *testing.T, d time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func startSpool(t *testing.T, s *Spool) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	return cancel
}

func closeSpool(t *testing.T, s *Spool, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
	defer c()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSpool_StoreFailureNoLoss：存储前几次失败 → 退避重试，最终全部写入且顺序不变。
func TestSpool_StoreFailureNoLoss(t *testing.T) {
	w := &fakeWriter{failFirst: 3}
	s := newTestSpool(t, t.TempDir(), w, 0)
	cancel := startSpool(t, s)
	defer closeSpool(t, s, cancel)

	const n = 50
	for i := 0; i < n; i++ {
		if err := s.Enqueue(mkRecord(i)); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	waitFor(t, 3*time.Second, "50 条全部写入", func() bool { return w.count() == n })

	got := w.snapshot()
	for i, r := range got {
		if r.Msg != fmt.Sprintf("hello %d", i) {
			t.Fatalf("顺序错乱：第 %d 条是 %q", i, r.Msg)
		}
	}
	if st := s.Stats(); st.Retried == 0 {
		t.Fatalf("应记录重试次数，实际 %+v", st)
	}
	if st := s.Stats(); st.Depth != 0 || st.Dropped != 0 {
		t.Fatalf("写完后队列应为空且无丢弃，实际 %+v", st)
	}
}

// TestSpool_ReopenReplaysUnacked：存储不可用时关闭，重新打开后必须重放，不丢。
func TestSpool_ReopenReplaysUnacked(t *testing.T) {
	dir := t.TempDir()
	bad := &fakeWriter{failAlways: true}
	s1 := newTestSpool(t, dir, bad, 0)
	cancel := startSpool(t, s1)
	const n = 20
	for i := 0; i < n; i++ {
		if err := s1.Enqueue(mkRecord(i)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	time.Sleep(30 * time.Millisecond) // 让写循环至少失败一次
	closeSpool(t, s1, cancel)

	good := &fakeWriter{}
	s2 := newTestSpool(t, dir, good, 0)
	cancel2 := startSpool(t, s2)
	defer closeSpool(t, s2, cancel2)
	waitFor(t, 3*time.Second, "重放 20 条", func() bool { return good.count() == n })
	if got := good.snapshot(); len(got) != n || got[0].Msg != "hello 0" {
		t.Fatalf("重放内容异常：%d 条，首条 %q", len(got), got[0].Msg)
	}
}

// TestSpool_QueueFullDropsAndCounts：达到上限时返回 ErrFull 并计数，不阻塞。
func TestSpool_QueueFullDropsAndCounts(t *testing.T) {
	s := newTestSpool(t, t.TempDir(), &fakeWriter{}, 2048)
	full := false
	for i := 0; i < 100; i++ {
		if err := s.Enqueue(mkRecord(i)); errors.Is(err, ErrFull) {
			full = true
			break
		}
	}
	if !full {
		t.Fatal("期望触发 ErrFull")
	}
	if st := s.Stats(); st.Dropped == 0 {
		t.Fatalf("丢弃计数未增长：%+v", st)
	}
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSpool_BackoffCoalesces：存储持续失败时，退避不能被新消息"唤醒"成每条一写。
func TestSpool_BackoffCoalesces(t *testing.T) {
	w := &fakeWriter{failAlways: true}
	s, err := Open(Config{
		Dir: t.TempDir(), Store: w, BatchSize: 64, WriteTimeout: 200 * time.Millisecond,
		RetryMin: 30 * time.Millisecond, RetryMax: 60 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel := startSpool(t, s)
	defer closeSpool(t, s, cancel)

	for i := 0; i < 60; i++ {
		if err := s.Enqueue(mkRecord(i)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		time.Sleep(2 * time.Millisecond) // 约 120ms 的持续流量
	}
	time.Sleep(300 * time.Millisecond)
	if n := w.writeCount(); n > 15 {
		t.Fatalf("退避未生效：%d 次写尝试（60 条消息，期望 <=15）", n)
	}
}

// TestSpool_CorruptFrameSkipped：坏帧被跳过并计数，不永久阻塞队列。
func TestSpool_CorruptFrameSkipped(t *testing.T) {
	w := &fakeWriter{}
	s := newTestSpool(t, t.TempDir(), w, 0)
	if err := s.q.Push([]byte("{not-json")); err != nil {
		t.Fatalf("Push bad frame: %v", err)
	}
	cancel := startSpool(t, s)
	defer closeSpool(t, s, cancel)
	if err := s.Enqueue(mkRecord(1)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, 2*time.Second, "好帧写入", func() bool { return w.count() == 1 })
	waitFor(t, time.Second, "坏帧计数", func() bool { return s.Stats().Corrupt == 1 })
}

// TestSpool_CloseNoLoss：关闭时未写完的记录必须仍在队列里（不丢），而不是被丢弃。
func TestSpool_CloseNoLoss(t *testing.T) {
	w := &fakeWriter{delay: 20 * time.Millisecond}
	s := newTestSpool(t, t.TempDir(), w, 0)
	cancel := startSpool(t, s)
	const n = 100
	for i := 0; i < n; i++ {
		if err := s.Enqueue(mkRecord(i)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	cancel()
	ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
	defer c()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st := s.Stats()
	if int64(w.count())+st.Depth != n {
		t.Fatalf("关闭后记录数不守恒：写入 %d + 队列 %d != %d", w.count(), st.Depth, n)
	}
}

// TestFrameRoundTrip：帧格式必须可无损往返（含全部字段），且版本号受控。
func TestFrameRoundTrip(t *testing.T) {
	sev := 3
	pid := 42
	r := mkRecord(7)
	r.PRI = &sev
	r.Facility = &sev
	r.PID = &pid
	r.SeverityNumber = &sev
	r.EventTimeRaw = "2026-11-22 10:00:00"
	r.TimeInferred = true
	r.IsAudit = true
	r.Dup = true
	r.Seq = 99
	r.ParseError = "x"
	r.Tags = []string{"ssh", "brute"}

	b1, err := encodeFrame(r)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	dec, err := decodeFrame(b1)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b2, err := encodeFrame(dec)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("往返后帧不一致:\n%s\n%s", b1, b2)
	}
	// 关键字段抽样核对（时间用 Equal，避免 monotonic/location 干扰）。
	if dec.Raw != r.Raw || dec.Host != r.Host || dec.Msg != r.Msg ||
		dec.Source.Kind != r.Source.Kind || !dec.EventTime.Equal(r.EventTime) ||
		!dec.IngestTime.Equal(r.IngestTime) || dec.Seq != r.Seq || !dec.IsAudit ||
		!reflect.DeepEqual(dec.Classes, r.Classes) || !reflect.DeepEqual(dec.User, r.User) {
		t.Fatalf("字段丢失: %+v", dec)
	}
	if _, err := decodeFrame([]byte(`{"v":999,"r":{}}`)); err == nil {
		t.Fatal("未知版本应报错")
	}
}

// TestSpool_StatsShape：Stats 字段名/类型稳定（前端与诊断包依赖）。
func TestSpool_StatsShape(t *testing.T) {
	s := newTestSpool(t, t.TempDir(), &fakeWriter{}, 0)
	if err := s.Enqueue(mkRecord(1)); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Depth != 1 || st.Bytes <= 0 || st.MaxBytes <= 0 || st.Pushed != 1 {
		t.Fatalf("统计异常：%+v", st)
	}
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	_ = s.Close(ctx)
}

func BenchmarkEnqueue(b *testing.B) {
	s, err := Open(Config{
		Dir: b.TempDir(), MaxBytes: 4 << 30, Store: &fakeWriter{},
		BatchSize: 512, PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = s.Close(ctx)
	}()
	r := mkRecord(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Enqueue(r); err != nil {
			b.Fatal(err)
		}
	}
}
