package audit

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendRecentAndMask(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	l.Append(Entry{Actor: "alice", Action: "config.export", Result: "ok"})
	l.Append(Entry{Actor: "bob", Action: "sources.put", Result: "ok",
		Detail: "https://user:secretpw@host/x token=ghp_abcdefghijklmnopqrst"})

	got := l.Recent(10)
	if len(got) != 2 {
		t.Fatalf("应有 2 条，得到 %d", len(got))
	}
	if got[0].Actor != "bob" || got[1].Actor != "alice" {
		t.Fatalf("应按时间倒序，得到 %+v", got)
	}
	if strings.Contains(got[0].Detail, "secretpw") || strings.Contains(got[0].Detail, "ghp_") {
		t.Fatalf("审计详情未脱敏: %q", got[0].Detail)
	}
	if fi, err := os.Stat(l.Path()); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("审计文件应为 0640: %v", err)
	}
}

func TestRotationAndReload(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	l, err := Open(Options{Dir: dir, MaxFileBytes: 512, MaxTotalBytes: 1 << 20, MaxAge: 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		l.Append(Entry{Action: "test.append", Detail: strings.Repeat("x", 80)})
	}
	st := l.Stats()
	if st.Files < 2 {
		t.Fatalf("应产生归档文件，stats=%+v", st)
	}
	_ = l.Close()

	l2, err := Open(Options{Dir: dir, MaxFileBytes: 512, MaxTotalBytes: 1 << 20, MaxAge: 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if got := l2.Recent(20); len(got) == 0 {
		t.Fatal("重开后应从当前文件载入最近记录")
	}
}

func TestGCExpiredArchive(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	l, err := Open(Options{Dir: dir, MaxAge: 24 * time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	old := filepath.Join(dir, "audit-20200101.000.log")
	if err := os.WriteFile(old, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(old, now.Add(-48*time.Hour), now.Add(-48*time.Hour))
	l.mu.Lock()
	l.gcLocked()
	l.mu.Unlock()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("过期归档应被删除")
	}
}

func TestConcurrentAppend(t *testing.T) {
	l, err := Open(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.Append(Entry{Actor: "a", Action: "test", Detail: "x"})
			}
		}(i)
	}
	wg.Wait()
	if st := l.Stats(); st.Entries != 400 {
		t.Fatalf("并发写入条数应为 400，得到 %d", st.Entries)
	}
	if len(l.Recent(1000)) > ringCap {
		t.Fatalf("内存环不应超过 %d", ringCap)
	}
}
