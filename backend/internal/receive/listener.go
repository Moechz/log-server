// Package receive 实现接收监听器：把设备发来的 syslog 交给上层处理器。
//
// 边界（D-017）：只做"收 + 切帧"，**不做解析**（解析在 pipeline），也不越权。
package receive

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Moechz/log-server/backend/internal/receive/syslog"
)

// SourceInfo 描述一条消息的来源（进"信封"段，D-005②）。
type SourceInfo struct {
	IP       string
	Port     int
	Protocol string // udp | tcp | tls
}

// Handler 处理一条原始消息（可能被上层解析失败，但**原始字节必须被保留**）。
type Handler func(raw []byte, src SourceInfo)

// Options 监听器配置。
type Options struct {
	UDPAddr     string // 如 0.0.0.0:514
	TCPAddr     string // 如 0.0.0.0:514
	TLSAddr     string // 如 0.0.0.0:6514（FR-602；未配置证书时不启动）
	TLSCertFile string // PEM 证书链
	TLSKeyFile  string // PEM 私钥
	Handler     Handler
	Logger      *slog.Logger
	MaxConns    int           // TCP 并发连接上限（FR-604）
	ReadTimeout time.Duration // 单连接读超时
	MaxFrame    int           // 单帧上限
	Policy      Policy        // 来源准入 + 每来源限速（FR-603/604）
}

// Stats 是监听器统计（供总览与告警使用）。
type Stats struct {
	UDPPackets   uint64
	TCPConns     uint64
	TLSConns     uint64
	TCPActive    int64
	TLSActive    int64
	Dropped      uint64 // 丢弃（畸形/超限/超并发）
	ACLDenied    uint64 // 来源准入拒绝（FR-603）
	RateLimited  uint64 // 每来源限速拒绝（FR-604）
	LastRecvUnix int64
}

// Server 同时承载 UDP、TCP 与 TLS 监听。
type Server struct {
	opts Options

	udpConn *net.UDPConn
	tcpLn   net.Listener
	tlsLn   net.Listener

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup

	policy  atomic.Pointer[Policy]
	limiter *sourceLimiter

	stats struct {
		udpPackets   atomic.Uint64
		tcpConns     atomic.Uint64
		tlsConns     atomic.Uint64
		tcpActive    atomic.Int64
		tlsActive    atomic.Int64
		dropped      atomic.Uint64
		aclDenied    atomic.Uint64
		rateLimited  atomic.Uint64
		lastRecvUnix atomic.Int64
	}
}

// New 创建监听器。
func New(opts Options) *Server {
	if opts.MaxConns <= 0 {
		opts.MaxConns = 256
	}
	if opts.ReadTimeout <= 0 {
		opts.ReadTimeout = 5 * time.Minute
	}
	if opts.MaxFrame <= 0 {
		opts.MaxFrame = syslog.MaxMessageSize
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{opts: opts}
	p := opts.Policy
	s.policy.Store(&p)
	s.limiter = newSourceLimiter(p.MaxEPS, p.MaxBPS)
	return s
}

// SetPolicy 热更新准入/限速策略（UI 保存后立即生效，无需重启）。
func (s *Server) SetPolicy(p Policy) {
	s.policy.Store(&p)
	if s.limiter != nil {
		s.limiter.set(p.MaxEPS, p.MaxBPS)
	}
}

// Policy 返回当前策略（副本）。
func (s *Server) Policy() Policy {
	if p := s.policy.Load(); p != nil {
		return *p
	}
	return Policy{}
}

// Start 启动 UDP 与 TCP（任一失败即返回错误；已启动的部分会被关闭）。
func (s *Server) Start(ctx context.Context) error {
	if s.opts.UDPAddr != "" {
		ua, err := net.ResolveUDPAddr("udp", s.opts.UDPAddr)
		if err != nil {
			return fmt.Errorf("解析 UDP 地址失败: %w", err)
		}
		c, err := net.ListenUDP("udp", ua)
		if err != nil {
			return fmt.Errorf("UDP 监听 %s 失败: %w", s.opts.UDPAddr, err)
		}
		s.udpConn = c
		s.wg.Add(1)
		go s.serveUDP(ctx)
		s.opts.Logger.Info("UDP 接收已启动", "addr", s.opts.UDPAddr)
	}

	if s.opts.TCPAddr != "" {
		ln, err := net.Listen("tcp", s.opts.TCPAddr)
		if err != nil {
			s.closeAll()
			return fmt.Errorf("TCP 监听 %s 失败: %w", s.opts.TCPAddr, err)
		}
		s.tcpLn = ln
		s.wg.Add(1)
		go s.serveStream(ctx, ln, "tcp")
		s.opts.Logger.Info("TCP 接收已启动", "addr", s.opts.TCPAddr)
	}

	// TLS（RFC 5425 syslog-over-TLS；FR-602）。
	// 证书缺失/不可读时**不阻塞启动** —— 只记警告，其余监听照常（能力探测口径 D-014）。
	if s.opts.TLSAddr != "" {
		switch {
		case s.opts.TLSCertFile == "" || s.opts.TLSKeyFile == "":
			s.opts.Logger.Warn("TLS 接收未启动：未配置证书/私钥（可在 log-server-for-tos.env 中设置）", "addr", s.opts.TLSAddr)
		default:
			crt, err := tls.LoadX509KeyPair(s.opts.TLSCertFile, s.opts.TLSKeyFile)
			if err != nil {
				s.opts.Logger.Warn("TLS 接收未启动：证书加载失败", "err", err,
					"cert", s.opts.TLSCertFile, "key", s.opts.TLSKeyFile)
				break
			}
			raw, err := net.Listen("tcp", s.opts.TLSAddr)
			if err != nil {
				s.closeAll()
				return fmt.Errorf("TLS 监听 %s 失败: %w", s.opts.TLSAddr, err)
			}
			s.tlsLn = tls.NewListener(raw, &tls.Config{
				Certificates: []tls.Certificate{crt},
				MinVersion:   tls.VersionTLS12,
			})
			s.wg.Add(1)
			go s.serveStream(ctx, s.tlsLn, "tls")
			s.opts.Logger.Info("TLS 接收已启动", "addr", s.opts.TLSAddr)
		}
	}
	return nil
}

// Close 停止接受新连接并等待在读协程收敛。
func (s *Server) Close() error {
	s.closeAll()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		return errors.New("监听器关闭超时")
	}
	return nil
}

// Stats 返回统计快照。
func (s *Server) Stats() Stats {
	return Stats{
		UDPPackets:   s.stats.udpPackets.Load(),
		TCPConns:     s.stats.tcpConns.Load(),
		TLSConns:     s.stats.tlsConns.Load(),
		TCPActive:    s.stats.tcpActive.Load(),
		TLSActive:    s.stats.tlsActive.Load(),
		Dropped:      s.stats.dropped.Load(),
		ACLDenied:    s.stats.aclDenied.Load(),
		RateLimited:  s.stats.rateLimited.Load(),
		LastRecvUnix: s.stats.lastRecvUnix.Load(),
	}
}

// TLSEnabled 报告 TLS 监听是否真的起来了（供总览/能力探测如实展示）。
func (s *Server) TLSEnabled() bool { return s.tlsLn != nil }

func (s *Server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.udpConn != nil {
		_ = s.udpConn.Close()
	}
	if s.tcpLn != nil {
		_ = s.tcpLn.Close()
	}
	if s.tlsLn != nil {
		_ = s.tlsLn.Close()
	}
}

func (s *Server) deliver(raw []byte, src SourceInfo) {
	if len(raw) == 0 {
		return
	}
	if ip, err := netip.ParseAddr(src.IP); err == nil {
		if !s.Policy().Allows(ip) {
			s.stats.aclDenied.Add(1)
			return
		}
		if !s.limiter.allow(ip, int64(len(raw))) {
			s.stats.rateLimited.Add(1)
			return
		}
	}
	s.stats.lastRecvUnix.Store(time.Now().Unix())
	if s.opts.Handler != nil {
		s.opts.Handler(raw, src)
	}
}

// serveUDP 每读一个数据报即交付（syslog over UDP 每条一报文）。
func (s *Server) serveUDP(ctx context.Context) {
	defer s.wg.Done()
	buf := make([]byte, 64<<10)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, addr, err := s.udpConn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || s.isClosed() {
				return
			}
			// 单次读失败不应终止整个监听（如 ICMP 触发的临时错误）
			s.stats.dropped.Add(1)
			continue
		}
		s.stats.udpPackets.Add(1)
		raw := make([]byte, n)
		copy(raw, buf[:n])
		src := SourceInfo{Protocol: "udp"}
		if addr != nil {
			src.IP, src.Port = addr.IP.String(), addr.Port
		}
		// UDP 可能把多行塞进一个报文 → 按行拆分（非透明帧）
		for _, frame := range splitLines(raw) {
			s.deliver(frame, src)
		}
	}
}

// serveStream 是 TCP/TLS 共用的接受循环（每连接一个协程）。
func (s *Server) serveStream(ctx context.Context, ln net.Listener, proto string) {
	defer s.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || s.isClosed() {
				return
			}
			s.stats.dropped.Add(1)
			continue
		}
		if a, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
			if ip, err := netip.ParseAddr(a.IP.String()); err == nil && !s.Policy().Allows(ip) {
				s.stats.aclDenied.Add(1)
				_ = conn.Close()
				continue
			}
		}
		if s.stats.tcpActive.Load()+s.stats.tlsActive.Load() >= int64(s.opts.MaxConns) {
			// 超并发：拒绝并计数（FR-604），不做静默排队
			s.stats.dropped.Add(1)
			_ = conn.Close()
			continue
		}
		if proto == "tls" {
			s.stats.tlsConns.Add(1)
			s.stats.tlsActive.Add(1)
		} else {
			s.stats.tcpConns.Add(1)
			s.stats.tcpActive.Add(1)
		}
		s.wg.Add(1)
		go s.handleConn(ctx, conn, proto)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn, proto string) {
	defer s.wg.Done()
	if proto == "tls" {
		defer s.stats.tlsActive.Add(-1)
	} else {
		defer s.stats.tcpActive.Add(-1)
	}
	defer conn.Close()

	src := SourceInfo{Protocol: proto}
	if a, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		src.IP, src.Port = a.IP.String(), a.Port
	}

	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(s.opts.ReadTimeout))
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if len(buf) > 8<<20 {
				// 缓冲异常增长：丢弃并计数，避免内存放大
				s.stats.dropped.Add(1)
				buf = buf[:0]
				continue
			}
			frames, rest, ferr := syslog.ReadFrames(buf, syslog.FramingAuto)
			if ferr != nil {
				s.stats.dropped.Add(1)
				buf = buf[:0]
				continue
			}
			for _, f := range frames {
				if len(f) > s.opts.MaxFrame {
					s.stats.dropped.Add(1)
					continue
				}
				s.deliver(f, src)
			}
			// 保留未消费部分（必须复制，buf 会被重用）
			buf = append(buf[:0], rest...)
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// splitLines 把可能含多行的 UDP 报文拆成单条。
func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\n' {
			continue
		}
		line := b[start:i]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) > 0 {
			out = append(out, line)
		}
		start = i + 1
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
