// Package journald 实现系统日志采集（FR-101 / FR-901）。
//
// 边界（D-017，硬约束）：
//   - **只读**：本包只运行 journalctl 读取日志；**绝不自我授权**
//     （不会 usermod -aG、不会 setfacl、不会请求 root、不会改任何系统配置）；
//   - 没有权限时**不启动**，把「未授权（待平台授权 R1）」如实上报给上层置灰。
//
// 实现：`journalctl -o json --follow` 子进程（禁用 CGO，故不使用 sd-journal 的 C 接口）。
//
// 已知陷阱（2026-10-14 真机实测，必须处理）：
// `journalctl` 在**没有权限时不报错**，只静默返回自己的少量日志（实测 0 行，退出码 0）。
// 因此本包不能把「命令成功」当作「已在采集」——必须用
// 「授权探测（由上层传入）+ 首条条目断言 + 计数」把空转显式标记为降级，
// 否则会出现"界面显示正常、实际 0 条系统日志"的假象。
package journald

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options 是采集器配置。
type Options struct {
	Bin        string      // journalctl 路径（默认 "journalctl"）
	Args       []string    // 额外参数（调用方负责加 -u / -p / --since 等）
	Authorized func() bool // 授权探测（由能力层 R1 提供；nil = 一律视为未授权）
	AuthReason func() string
	Buffer     int           // 交付通道容量（默认 1024）
	RestartMin time.Duration // 退出后重试起点（默认 2s）
	RestartMax time.Duration // 重试上限（默认 30s）
	SilentWarn time.Duration // 启动后多久仍 0 条就报「静默/可能未授权」（默认 20s）
	Recheck    time.Duration // 未授权时的复检间隔（默认 30s）——平台授权后无需重启即可开始采集
	MaxLine    int           // 单条 JSON 行上限（默认 1 MiB）
	Handler    func(raw []byte)
	OnSelfLog  func(level, msg string, kv ...string)
	Logger     *slog.Logger
}

// Collector 是 journald 采集器（含看护子进程的 supervisor）。
type Collector struct {
	opts Options

	mu       sync.Mutex
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	doneRun  chan struct{} // supervise 的生命周期
	doneWait chan struct{} // 「等待授权」协程的生命周期
	running  bool

	entries      atomic.Uint64
	systemish    atomic.Uint64 // 非本应用用户的条目（用于判断"真的读到了系统日志"）
	restarts     atomic.Uint64
	dropped      atomic.Uint64
	lastEntry    atomic.Int64 // unix
	lastErrMu    sync.Mutex
	lastErr      string
	silentWarned atomic.Bool
	startedAt    time.Time
	authorized   atomic.Bool
	authReason   atomic.Value // string
	waiting      atomic.Bool  // 正在等平台授权（D-014：授权到位自动开）
	waitCancel   context.CancelFunc
}

// New 构造采集器。
func New(opts Options) *Collector {
	if opts.Bin == "" {
		opts.Bin = "journalctl"
	}
	if opts.Buffer <= 0 {
		opts.Buffer = 1024
	}
	if opts.RestartMin <= 0 {
		opts.RestartMin = 2 * time.Second
	}
	if opts.RestartMax <= 0 {
		opts.RestartMax = 30 * time.Second
	}
	if opts.SilentWarn <= 0 {
		opts.SilentWarn = 20 * time.Second
	}
	if opts.Recheck <= 0 {
		opts.Recheck = 30 * time.Second
	}
	if opts.MaxLine <= 0 {
		opts.MaxLine = 1 << 20
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	c := &Collector{opts: opts}
	c.authReason.Store("")
	return c
}

// Authorized 返回当前授权判定（供上层展示）。
func (c *Collector) Authorized() bool { return c.authorized.Load() }

// Start 启动采集（含「等待授权」状态机）。
//
// 行为（D-014 / D-017）：
//   - 已授权 → 立即开始读取；
//   - 未授权 → **进程继续运行**并每 Recheck 复检一次（返回 ErrNotAuthorized 供上层知情），
//     平台一旦授权（加入 systemd-journal 组 / 加 ACL）**无需重启或重装即自动开始采集**；
//   - 全程**只读**：绝不加组、不改 ACL、不改 rsyslog/journald 配置。
func (c *Collector) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running {
		return errors.New("采集器已在运行")
	}
	auth := c.opts.Authorized != nil && c.opts.Authorized()
	c.authorized.Store(auth)
	c.authReason.Store(c.reasonText(auth))

	if !auth {
		waitCtx, waitCancel := context.WithCancel(ctx)
		c.waitCancel = waitCancel
		done := make(chan struct{})
		c.doneWait = done
		c.running = true
		c.waiting.Store(true)
		c.selfLog("warn", "journald 采集未启动：无读取权限（待平台授权 R1；本应用不会自我授权）；将每 "+
			c.opts.Recheck.String()+" 复检一次，授权后自动开始采集", "capability", "R1")
		go c.waitForAuth(waitCtx, done)
		return ErrNotAuthorized
	}
	if err := c.startLocked(ctx); err != nil {
		return err
	}
	c.selfLog("info", "journald 采集已启动（只读；不修改系统配置）",
		"bin", c.opts.Bin, "args", strings.Join(c.opts.Args, " "))
	return nil
}

// startLocked 在已持有 c.mu 的前提下启动看护循环。
func (c *Collector) startLocked(ctx context.Context) error {
	if _, err := exec.LookPath(c.opts.Bin); err != nil {
		c.authReason.Store("journalctl 不可用: " + err.Error())
		return fmt.Errorf("journalctl 不可用: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	done := make(chan struct{})
	c.doneRun = done
	c.startedAt = time.Now()
	c.running = true
	c.waiting.Store(false)
	go c.supervise(runCtx, done)
	return nil
}

// waitForAuth 在未授权状态下周期复检；授权到位即自动开始采集（D-014：不做一次性门控）。
func (c *Collector) waitForAuth(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(c.opts.Recheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		auth := c.opts.Authorized != nil && c.opts.Authorized()
		c.authorized.Store(auth)
		c.authReason.Store(c.reasonText(auth))
		if !auth {
			continue
		}
		c.mu.Lock()
		if !c.running {
			c.mu.Unlock()
			return
		}
		err := c.startLocked(context.WithoutCancel(ctx))
		c.mu.Unlock()
		if err != nil {
			c.selfLog("warn", "已获授权但 journald 采集启动失败", "err", err.Error())
			continue
		}
		c.selfLog("info", "检测到平台已授权：journald 采集自动开始（无需重启/重装）", "capability", "R1")
		return // 生命周期已交棒给 supervise（doneRun），本协程的 done 由 defer 关闭
	}
}

// reasonText 生成给用户看的原因文案。
func (c *Collector) reasonText(auth bool) string {
	if auth {
		return ""
	}
	if c.opts.AuthReason != nil {
		if r := c.opts.AuthReason(); r != "" {
			return r
		}
	}
	return "无读取权限（待平台授权 R1：请平台声明式授权 / 将应用用户加入 systemd-journal 组）"
}

// Stop 停止采集（等待授权或正在采集均适用）并等待收敛。
func (c *Collector) Stop() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	cancel, waitCancel := c.cancel, c.waitCancel
	doneRun, doneWait := c.doneRun, c.doneWait
	c.running = false
	c.waiting.Store(false)
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if waitCancel != nil {
		waitCancel()
	}
	for _, d := range []chan struct{}{doneRun, doneWait} {
		if d == nil {
			continue
		}
		select {
		case <-d:
		case <-time.After(3 * time.Second):
		}
	}
}

// ErrNotAuthorized 表示未获得读取系统日志的授权（本应用不做自我授权）。
var ErrNotAuthorized = errors.New("未授权读取系统日志（待平台授权 R1）")

func (c *Collector) supervise(ctx context.Context, done chan struct{}) {
	defer close(done)
	backoff := c.opts.RestartMin
	for {
		if ctx.Err() != nil {
			return
		}
		start := time.Now()
		err := c.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		c.restarts.Add(1)
		if err != nil {
			c.setErr(err.Error())
			c.selfLog("warn", "journald 读取中断，将重试", "err", err.Error())
		}
		// 长时间正常运行（>5 分钟）后重置退避，避免"偶发一次失败就永久慢退避"
		if time.Since(start) > 5*time.Minute {
			backoff = c.opts.RestartMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < c.opts.RestartMax {
			backoff *= 2
			if backoff > c.opts.RestartMax {
				backoff = c.opts.RestartMax
			}
		}
	}
}

func (c *Collector) runOnce(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.opts.Bin, c.opts.Args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return err
	}
	c.mu.Lock()
	c.cmd = cmd
	c.mu.Unlock()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), c.opts.MaxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		// ★ 复制一份：Scanner 会复用底层数组，而记录会异步进入管线
		raw := make([]byte, len(line))
		copy(raw, line)
		c.entries.Add(1)
		if !isOwnEntry(raw) {
			c.systemish.Add(1)
		}
		c.lastEntry.Store(time.Now().Unix())
		if c.opts.Handler != nil {
			c.opts.Handler(raw)
		}
	}
	scanErr := sc.Err()
	waitErr := cmd.Wait()
	if scanErr != nil {
		return fmt.Errorf("读取 journalctl 输出失败: %w", scanErr)
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fmt.Errorf("journalctl 退出: %v (%s)", waitErr, firstLine(msg))
		}
		return fmt.Errorf("journalctl 退出: %w", waitErr)
	}
	return nil
}

// Status 返回采集状态（供 /api/v1/sources 展示）。
func (c *Collector) Status() map[string]any {
	c.mu.Lock()
	running := c.running
	started := c.startedAt
	c.mu.Unlock()
	waiting := c.waiting.Load()
	active := running && !waiting
	reason, _ := c.authReason.Load().(string)
	last := c.lastEntry.Load()
	silentFor := int64(-1)
	if last > 0 {
		silentFor = time.Now().Unix() - last
	}
	// ★ 静默失败防护：已授权、命令在跑、但超过 SilentWarn 仍 0 条 →
	//   如实标记 degraded（真机实测：未授权时 journalctl 不报错、只返回 0 条）
	degraded := false
	degradeReason := ""
	if active && !started.IsZero() && c.entries.Load() == 0 && time.Since(started) > c.opts.SilentWarn {
		degraded = true
		degradeReason = "journalctl 未返回任何条目（可能未授权或本机无新日志）"
		if !c.silentWarned.Swap(true) {
			c.selfLog("warn", "journald 采集疑似空转", "reason", degradeReason)
		}
	}
	return map[string]any{
		"kind":            "journald",
		"path":            "journald://system",
		"enabled":         active,
		"waiting":         waiting,
		"authorized":      c.authorized.Load(),
		"reason":          reason,
		"args":            strings.Join(c.opts.Args, " "),
		"entries":         c.entries.Load(),
		"system_entries":  c.systemish.Load(),
		"restarts":        c.restarts.Load(),
		"silent_for_sec":  silentFor,
		"degraded":        degraded,
		"degraded_reason": degradeReason,
		"last_error":      c.errString(),
	}
}

func (c *Collector) setErr(s string) {
	c.lastErrMu.Lock()
	c.lastErr = s
	c.lastErrMu.Unlock()
}

func (c *Collector) errString() string {
	c.lastErrMu.Lock()
	defer c.lastErrMu.Unlock()
	return c.lastErr
}

func (c *Collector) selfLog(level, msg string, kv ...string) {
	c.opts.Logger.Log(context.Background(), levelOf(level), msg, toArgs(kv)...)
	if c.opts.OnSelfLog != nil {
		c.opts.OnSelfLog(level, msg, kv...)
	}
}

func levelOf(l string) slog.Level {
	switch l {
	case "error":
		return slog.LevelError
	case "warn":
		return slog.LevelWarn
	case "debug":
		return slog.LevelDebug
	default:
		return slog.LevelInfo
	}
}

func toArgs(kv []string) []any {
	out := make([]any, 0, len(kv))
	for _, v := range kv {
		out = append(out, v)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// isOwnEntry 判断该条是否由本应用用户产生（用于"是否真的读到系统日志"的统计）。
// 只做轻量字符串判断，避免为统计引入完整 JSON 解析开销。
func isOwnEntry(raw []byte) bool {
	return strings.Contains(string(raw), `"SYSLOG_IDENTIFIER":"log-server-for-tos"`) ||
		strings.Contains(string(raw), `"_COMM":"log-server-for-tos"`)
}
