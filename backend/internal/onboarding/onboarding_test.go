package onboarding

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompleteLoadReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onboarding.json")
	if s := Load(path); s.Completed {
		t.Fatal("初始应为未完成")
	}
	if err := Complete(path, "alice"); err != nil {
		t.Fatal(err)
	}
	s := Load(path)
	if !s.Completed || s.CompletedBy != "alice" || s.CompletedAt.IsZero() {
		t.Fatalf("状态异常: %+v", s)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("状态文件应为 0600: %v", err)
	}
	if err := Reset(path); err != nil {
		t.Fatal(err)
	}
	if s := Load(path); s.Completed {
		t.Fatal("重置后应为未完成")
	}
}

func TestLoadCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onboarding.json")
	if err := os.WriteFile(path, []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := Load(path); s.Completed {
		t.Fatal("损坏文件应回退为未完成")
	}
}

func TestCompleteTimestampUTC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onboarding.json")
	_ = Complete(path, "bob")
	if s := Load(path); s.CompletedAt.Location() != time.UTC {
		t.Fatalf("时间应为 UTC: %v", s.CompletedAt)
	}
}
