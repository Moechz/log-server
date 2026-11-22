package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/ingest/journald"
	"github.com/Moechz/log-server/backend/internal/model"
	"github.com/Moechz/log-server/backend/internal/pipeline"
	"github.com/Moechz/log-server/backend/internal/selflog"
)

// TestIngestManagerPauseResume 验证 FR-1107：暂停期间不读取、恢复后从位点续采（不丢）。
func TestIngestManagerPauseResume(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "app.log")
	if err := os.WriteFile(logFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var got atomic.Int64
	deliver := func(*model.Record) { got.Add(1) }
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	self := selflog.New(64)
	proc := pipeline.New(pipeline.DefaultConfig())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := newIngestManager(ctx, dir, proc, deliver, self, lg, journald.Options{}, false)
	mgr.Start([]sourceSpec{{Path: logFile, Enabled: true}})
	defer mgr.Stop()

	appendLine := func(s string) {
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s + "\n")
		_ = f.Close()
	}
	waitFor := func(n int64, d time.Duration) bool {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if got.Load() >= n {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}

	appendLine("hello 1")
	if !waitFor(1, 6*time.Second) {
		t.Fatal("启动后未采集到文件日志")
	}
	items, paused := mgr.Sources()
	if paused || len(items) != 1 {
		t.Fatalf("Sources 状态异常: paused=%v items=%d", paused, len(items))
	}

	// 暂停：不应继续读取
	mgr.SetPaused(true)
	if !mgr.Paused() {
		t.Fatal("SetPaused(true) 后应为暂停")
	}
	base := got.Load()
	appendLine("hello 2 (during pause)")
	time.Sleep(2500 * time.Millisecond)
	if got.Load() != base {
		t.Fatalf("暂停期间不应采集：got=%d base=%d", got.Load(), base)
	}

	// 恢复：应从未读位点续采到暂停期间写入的那条（不丢）
	mgr.SetPaused(false)
	if !waitFor(base+1, 6*time.Second) {
		t.Fatalf("恢复后未续采：got=%d want>=%d", got.Load(), base+1)
	}
}

// TestIngestManagerApplyReplacesSources 验证 FR-102：运行时替换采集源集合。
func TestIngestManagerApplyReplacesSources(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	for _, f := range []string{a, b} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lg := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	mgr := newIngestManager(context.Background(), dir, pipeline.New(pipeline.DefaultConfig()),
		func(*model.Record) {}, selflog.New(64), lg, journald.Options{}, false)
	mgr.Start([]sourceSpec{{Path: a, Enabled: true}})
	defer mgr.Stop()

	if items, _ := mgr.Sources(); len(items) != 1 {
		t.Fatalf("应有 1 个采集源，得到 %d", len(items))
	}
	mgr.Apply([]sourceSpec{{Path: b, Enabled: true}})
	items, _ := mgr.Sources()
	if len(items) != 1 || items[0]["path"] != b {
		t.Fatalf("Apply 后应只剩 b：%+v", items)
	}
	// 停用的源不应启动
	mgr.Apply([]sourceSpec{{Path: a, Enabled: false}})
	if items, _ := mgr.Sources(); len(items) != 0 {
		t.Fatalf("停用的源不应出现：%+v", items)
	}
}
