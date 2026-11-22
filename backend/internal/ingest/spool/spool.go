// Package spool 是采集/接收主路径的**磁盘暂存队列**（WAL）。
//
// 背景：此前 receive/file/journald 统一走一个 4096 容量的内存 channel，满了直接丢；
// 存储引擎故障时批次只留在内存、每条新记录都触发一次写超时，进程重启则全部丢失。
// 这使 "at-least-once / 重启不丢" 只在转发侧成立。
//
// 现在的路径：
//
//	deliver → spool.Enqueue（同步追加到 <DataDir>/queue/ingest，返回即已落盘）
//	        → 写循环 Pop → 解码 → store.Write → 成功才 Ack
//
// 语义：
//   - 存储引擎停机：记录留在磁盘队列，恢复后自动补写（受 MaxBytes 上限保护）；
//   - 进程正常重启/升级：Enqueue 已落盘，重启后自动重放；
//   - kill -9 / 断电：最多丢 OS 页缓存里尚未刷盘的最后一小段（queue.Close 会 Sync）；
//   - 重复：崩溃后未 Ack 的记录会重放（至少一次），不静默丢弃。
package spool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
	"github.com/Moechz/log-server/backend/internal/queue"
)

var (
	// ErrClosed 表示 spool 已关闭（调用方按丢弃计数并告警）。
	ErrClosed = errors.New("spool: 已关闭")
	// ErrFull 表示队列达到容量上限（包装 queue.ErrFull，便于上层判断）。
	ErrFull = queue.ErrFull
)

// Writer 是 spool 对存储层的最小依赖（只写，不查询），便于单测注入故障。
type Writer interface {
	Write(ctx context.Context, batch []*model.Record) error
}

// Config 是 spool 配置。
type Config struct {
	Dir          string                                        // 队列目录（默认 <DataDir>/queue/ingest）
	MaxBytes     int64                                         // 上限（0 = 不限；实际会按卷剩余空间收紧，见 CapMaxBytes）
	SegmentBytes int64                                         // 单段大小（默认 64 MB）
	Store        Writer                                        // 存储写入器（必填）
	Logger       *slog.Logger                                  // 可空
	SelfLog      func(level, module, msg string, kv ...string) // 可空

	BatchSize    int           // 每批最多写多少条（默认 512）
	WriteTimeout time.Duration // 单批写超时（默认 10s）
	RetryMin     time.Duration // 失败退避起点（默认 1s）
	RetryMax     time.Duration // 失败退避上限（默认 30s）
	PollInterval time.Duration // 空队列轮询间隔（默认 200ms）
}

// Stats 是队列与写循环的统计（暴露给总览/诊断包）。
type Stats struct {
	Depth        int64  `json:"depth"`
	Bytes        int64  `json:"bytes"`
	MaxBytes     int64  `json:"max_bytes"`
	OldestAgeSec int64  `json:"oldest_age_sec"`
	Pushed       uint64 `json:"pushed"`
	Acked        uint64 `json:"acked"`
	Retried      uint64 `json:"retried"`
	Dropped      uint64 `json:"dropped"`
	Corrupt      uint64 `json:"corrupt"`
	WriteErrors  uint64 `json:"write_errors"`
	Segments     int    `json:"segments"`
	LastErr      string `json:"last_err,omitempty"`
	Closed       bool   `json:"closed"`
}

// Spool 是并发安全的磁盘暂存队列。
type Spool struct {
	cfg Config
	q   *queue.Queue

	closed  atomic.Bool
	corrupt atomic.Uint64
	wrErrs  atomic.Uint64
	lastErr atomic.Value // string

	cancel   context.CancelFunc
	notify   chan struct{}
	done     chan struct{}
	started  atomic.Bool
	maxBytes int64
}

// Open 打开（或创建）队列目录并恢复位点。
func Open(cfg Config) (*Spool, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("spool: 缺少 Dir")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("spool: 缺少 Store")
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 512
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	if cfg.RetryMin <= 0 {
		cfg.RetryMin = time.Second
	}
	if cfg.RetryMax < cfg.RetryMin {
		cfg.RetryMax = 30 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("spool: 创建队列目录失败: %w", err)
	}
	// 容量按卷剩余空间收紧（默认不超过剩余 10%，小卷至少留一半），避免队列把数据卷吃满。
	maxBytes := CapMaxBytes(cfg.Dir, cfg.MaxBytes)
	q, err := queue.Open(queue.Config{Dir: cfg.Dir, MaxBytes: maxBytes, SegmentBytes: cfg.SegmentBytes})
	if err != nil {
		return nil, fmt.Errorf("spool: 打开队列失败: %w", err)
	}
	return &Spool{cfg: cfg, q: q, notify: make(chan struct{}, 1), maxBytes: maxBytes}, nil
}

// Start 启动写循环（幂等）。parent 取消后循环退出，未 Ack 的记录留在磁盘。
func (s *Spool) Start(parent context.Context) {
	if s.closed.Load() {
		return
	}
	if !s.started.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})

	if st := s.q.Stats(); st.Depth > 0 {
		s.logf("info", "入库队列已恢复，开始补写",
			"depth", fmt.Sprint(st.Depth), "bytes", fmt.Sprint(st.Bytes),
			"oldest_age_sec", fmt.Sprint(int64(st.OldestAge.Seconds())))
	}
	go s.loop(ctx)
}

// Enqueue 同步追加一条记录到磁盘队列；返回 nil 表示已落盘（进程崩溃后可重放）。
// 不阻塞在存储引擎上：存储故障由写循环退避重试，不会影响接收线程。
func (s *Spool) Enqueue(r *model.Record) error {
	if s.closed.Load() {
		return ErrClosed
	}
	b, err := encodeFrame(r)
	if err != nil {
		return err
	}
	if err := s.q.Push(b); err != nil {
		return err
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return nil
}

func (s *Spool) loop(ctx context.Context) {
	defer close(s.done)

	backoff := s.cfg.RetryMin
	for {
		if ctx.Err() != nil {
			return
		}
		msgs, err := s.q.Pop(s.cfg.BatchSize)
		if err != nil {
			if errors.Is(err, queue.ErrEmpty) {
				if !s.wait(ctx, s.cfg.PollInterval) {
					return
				}
				continue
			}
			s.setErr(err)
			s.logf("warn", "读取入库队列失败", "err", err.Error())
			if !s.wait(ctx, time.Second) {
				return
			}
			continue
		}

		batch := make([]*model.Record, 0, len(msgs))
		bad := 0
		for _, m := range msgs {
			r, derr := decodeFrame(m)
			if derr != nil {
				bad++
				s.setErr(derr)
				continue
			}
			batch = append(batch, r)
		}
		if bad > 0 {
			s.corrupt.Add(uint64(bad))
			s.logf("error", "入库队列存在无法解码的帧，已跳过（避免阻塞队列）", "bad", fmt.Sprint(bad))
		}

		if len(batch) > 0 {
			wctx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
			werr := s.cfg.Store.Write(wctx, batch)
			cancel()
			if werr != nil {
				s.wrErrs.Add(1)
				s.setErr(werr)
				s.q.Retry()
				s.logf("warn", "写入存储失败（队列保留，退避后重试）",
					"err", werr.Error(), "n", fmt.Sprint(len(batch)), "backoff", backoff.String())
				// 退避期间**不接受** notify 唤醒：否则持续来日志会让每个消息都触发一次
				// 存储写尝试（每批 10s 超时），退避形同虚设。
				if !s.waitTimer(ctx, backoff) {
					return
				}
				backoff *= 2
				if backoff > s.cfg.RetryMax {
					backoff = s.cfg.RetryMax
				}
				continue
			}
		}

		// 成功（或只有坏帧）才推进读位点；Ack 失败只会导致重放（重复），不会丢。
		if err := s.q.Ack(len(msgs)); err != nil {
			s.setErr(err)
			s.logf("warn", "推进入库队列位点失败（将重放）", "err", err.Error())
		}
		backoff = s.cfg.RetryMin
	}
}

// waitTimer 只按时间等待（退避用，不允许被新消息提前唤醒）。
func (s *Spool) waitTimer(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// wait 等待 d；被 notify 提前唤醒或被 ctx 取消时返回（空队列轮询用）。
func (s *Spool) wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.notify:
		return true
	case <-t.C:
		return true
	}
}

// Close 停止写循环并同步队列文件。ctx 只约束“等待当前批次写完”的时间。
// 未写完的记录留在队列里，下次启动自动重放（不会丢）。
func (s *Spool) Close(ctx context.Context) error {
	s.closed.Store(true)
	if s.cancel != nil {
		s.cancel()
	}
	if s.done != nil {
		select {
		case <-s.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.q.Close()
}

// Stats 返回统计快照。
func (s *Spool) Stats() Stats {
	st := s.q.Stats()
	out := Stats{
		Depth: st.Depth, Bytes: st.Bytes, MaxBytes: s.maxBytes,
		OldestAgeSec: int64(st.OldestAge.Seconds()),
		Pushed:       st.Pushed, Acked: st.Acked, Retried: st.Retried, Dropped: st.Dropped,
		Segments: st.Segments, LastErr: st.LastErr, Closed: s.closed.Load(),
	}
	if out.LastErr == "" {
		if v := s.lastErr.Load(); v != nil {
			out.LastErr, _ = v.(string)
		}
	}
	out.Corrupt = s.corrupt.Load()
	out.WriteErrors = s.wrErrs.Load()
	return out
}

func (s *Spool) setErr(err error) {
	if err == nil {
		return
	}
	s.lastErr.Store(err.Error())
}

func (s *Spool) logf(level, msg string, kv ...string) {
	if s.cfg.Logger != nil {
		args := kvToAny(kv)
		switch level {
		case "error":
			s.cfg.Logger.Error(msg, args...)
		case "warn":
			s.cfg.Logger.Warn(msg, args...)
		default:
			s.cfg.Logger.Info(msg, args...)
		}
	}
	if s.cfg.SelfLog != nil {
		s.cfg.SelfLog(level, "spool", msg, kv...)
	}
}

func kvToAny(kv []string) []any {
	out := make([]any, 0, len(kv))
	for _, v := range kv {
		out = append(out, v)
	}
	return out
}

// Dir 返回队列目录（供诊断/文档展示）。
func (s *Spool) Dir() string { return s.cfg.Dir }
