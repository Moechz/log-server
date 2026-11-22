package storageloc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateUnder(t *testing.T) {
	roots := []string{"/Volume1", "/Volume2"}
	cases := []struct {
		path string
		ok   bool
	}{
		{"/Volume1/log-server-for-tos/engine", true},
		{"/Volume1", true},
		{"/Volume2/Public/logs", true},
		{"/Volume3/x", false},                  // 卷不在 roots
		{"/volume1/x", false},                  // 大小写
		{"/Volume10/x", false},                 // 前缀不构成卷
		{"/var/lib/log-server-for-tos", false}, // 系统分区
		{"/etc/x", false},
		{"relative/path", false},
		{"", false},
		{"/Volume1/../etc/passwd", false}, // Clean 后为 /etc/passwd
		{"/Volume1//sub/", true},
	}
	for _, c := range cases {
		if err := validateUnder(c.path, roots); (err == nil) != c.ok {
			t.Errorf("validateUnder(%q) err=%v, want ok=%v", c.path, err, c.ok)
		}
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "config", "storage.json")

	if got := Load(f, "/default/engine"); got != "/default/engine" {
		t.Fatalf("缺文件应回退默认值，得到 %q", got)
	}
	if err := Save(f, "/Volume1/share/log-server-for-tos/engine"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := Load(f, "/default/engine"); got != "/Volume1/share/log-server-for-tos/engine" {
		t.Fatalf("读回不一致：%q", got)
	}
	if err := os.WriteFile(f, []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	if got := Load(f, "/default/engine"); got != "/default/engine" {
		t.Fatalf("损坏文件应回退，得到 %q", got)
	}
}

func TestListRejectsSystemPath(t *testing.T) {
	if _, err := List("/etc"); err == nil {
		t.Fatal("List(/etc) 应被拒绝")
	}
}
