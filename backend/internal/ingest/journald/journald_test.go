package journald

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCollector_NotAuthorizedDoesNotStart(t *testing.T) {
	c := New(Options{
		Authorized: func() bool { return false },
		AuthReason: func() string { return "无权限读取系统日志（尚未获得平台授权）" },
	})
	err := c.Start(context.Background())
	if err != ErrNotAuthorized {
		t.Fatalf("未授权时必须拒绝启动（ErrNotAuthorized），得到 %v", err)
	}
	st := c.Status()
	if st["authorized"] != false || st["enabled"] != false {
		t.Errorf("未授权时状态应为 authorized=false/enabled=false，得到 %v", st)
	}
	if st["waiting"] != true {
		t.Errorf("未授权时应标记 waiting=true（授权后自动开始），得到 %v", st["waiting"])
	}
	if st["degraded"] != false {
		t.Errorf("仅等待授权不应标记 degraded（避免误报），得到 %v", st)
	}
	if st["reason"] == "" {
		t.Error("未授权时必须给出原因（供 UI 如实置灰）")
	}
	if c.Authorized() {
		t.Error("Authorized() 应为 false")
	}
}

func TestCollector_ReadsLinesAndRestarts(t *testing.T) {
	var got atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(Options{
		Bin:        "/bin/sh",
		Args:       []string{"-c", `printf '%s\n' '{"MESSAGE":"x","PRIORITY":"6"}'`},
		Authorized: func() bool { return true },
		RestartMin: 10 * time.Millisecond,
		RestartMax: 20 * time.Millisecond,
		Handler:    func(raw []byte) { got.Add(1) },
	})
	if err := c.Start(ctx); err != nil {
		t.Fatalf("已授权时应能启动: %v", err)
	}
	defer c.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for got.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got.Load() < 2 {
		t.Fatalf("应至少读到 2 条（含重启后），实际 %d", got.Load())
	}
	if st := c.Status(); st["enabled"] != true || st["authorized"] != true || st["waiting"] != false {
		t.Errorf("状态错误: %v", st)
	}
}

// 静默失败防护：已授权但 journalctl 不产出任何条目（真机上"无权限"就是这个表现）→ 必须标记 degraded。
func TestCollector_SilentDegraded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(Options{
		Bin:        "/bin/sh",
		Args:       []string{"-c", "sleep 5"},
		Authorized: func() bool { return true },
		SilentWarn: 50 * time.Millisecond,
	})
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.Stop()
	time.Sleep(200 * time.Millisecond)
	st := c.Status()
	if st["degraded"] != true {
		t.Errorf("授权但 0 条时应标记 degraded（防静默空转），状态: %v", st)
	}
	if st["degraded_reason"] == "" {
		t.Error("degraded 必须带原因")
	}
}

func TestCollector_MissingBinReportsReason(t *testing.T) {
	c := New(Options{Bin: "/nonexistent/journalctl", Authorized: func() bool { return true }})
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("journalctl 不存在时应报错")
	}
	if st := c.Status(); st["reason"] == "" {
		t.Error("应记录不可用原因")
	}
}

// D-014 关键行为：启动时未授权 → 保持运行并复检；**授权到位后无需重启即自动开始采集**。
func TestCollector_AutoStartsWhenAuthorizedLater(t *testing.T) {
	var auth atomic.Bool
	var got atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(Options{
		Bin:        "/bin/sh",
		Args:       []string{"-c", `printf '%s\n' '{"MESSAGE":"sys"}'`},
		Authorized: auth.Load,
		Recheck:    20 * time.Millisecond,
		RestartMin: 10 * time.Millisecond,
		RestartMax: 20 * time.Millisecond,
		Handler:    func([]byte) { got.Add(1) },
	})
	if err := c.Start(ctx); err != ErrNotAuthorized {
		t.Fatalf("初始未授权应返回 ErrNotAuthorized，得到 %v", err)
	}
	defer c.Stop()
	if st := c.Status(); st["waiting"] != true {
		t.Fatalf("应处于等待授权状态: %v", st)
	}
	if got.Load() != 0 {
		t.Fatal("未授权时不应产生任何条目")
	}

	auth.Store(true) // 模拟"平台完成授权"（现实中是加组/加 ACL，由平台做）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.Status(); st["enabled"] == true && st["waiting"] == false && got.Load() > 0 {
			return // ✓ 自动启用并开始采集
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("授权后应自动开始采集，状态: %v（条目 %d）", c.Status(), got.Load())
}
