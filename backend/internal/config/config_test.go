package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRetention(t *testing.T) {
	cases := map[string]int{"14d": 14, "8w": 56, "6M": 180, "1y": 365, "30": 30, "": 0}
	for in, want := range cases {
		if got := parseRetention(in, 0); got != want {
			t.Errorf("parseRetention(%q) = %d, 期望 %d", in, got, want)
		}
	}
}

func TestParseBytes(t *testing.T) {
	cases := map[string]int64{
		"20GiB": 20 << 30, "512MiB": 512 << 20, "1024": 1024, "1G": 1 << 30, "": 0,
	}
	for in, want := range cases {
		if got := parseBytes(in, 0); got != want {
			t.Errorf("parseBytes(%q) = %d, 期望 %d", in, got, want)
		}
	}
}

func TestValidate_RejectsSystemPath(t *testing.T) {
	c := Default()
	c.DataDir = "/var/lib/log-server-for-tos/data" // 打包指南坑 11f：会撑爆系统分区
	c.VLogsStorage = "/Volume1/@apps/log-server-for-tos/data/engine"
	if err := c.Validate(); err == nil {
		t.Fatal("必须拒绝把运行时数据写到系统分区 /var/lib")
	}
}

func TestValidate_RejectsRelativePath(t *testing.T) {
	c := Default()
	c.DataDir = "data" // 相对路径
	if err := c.Validate(); err == nil {
		t.Fatal("必须拒绝相对路径")
	}
}

func TestLoad_EnvOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, "log-server-for-tos.env")
	body := "LOGSERVER_HTTP_PORT=15300\nLOGSERVER_SYSLOG_UDP_PORT=1514\nVLOGS_RETENTION_PERIOD=180d\nLOGSERVER_RESOURCE_MODE=standard\n"
	if err := os.WriteFile(env, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(env, "")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if c.HTTPPort != 15300 {
		t.Errorf("HTTPPort = %d, 期望 15300", c.HTTPPort)
	}
	if c.SyslogUDPPort != 1514 {
		t.Errorf("SyslogUDPPort = %d, 期望 1514（冲突时可改高端口）", c.SyslogUDPPort)
	}
	if c.RetentionDays != 180 {
		t.Errorf("RetentionDays = %d, 期望 180", c.RetentionDays)
	}
	if c.ResourceMode != ModeStandard {
		t.Errorf("ResourceMode = %q, 期望 standard", c.ResourceMode)
	}
}

func TestLoad_MissingEnvFileIsNotFatal(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "nonexistent.env"), "")
	if err != nil {
		t.Fatalf("env 文件不存在不应导致失败: %v", err)
	}
	if c.HTTPPort != 15280 {
		t.Errorf("应回退默认端口 15280，得到 %d", c.HTTPPort)
	}
}

func TestLoad_AuthFromEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "log-server-for-tos.env")
	content := "LOGSERVER_AUTH=off\nLOGSERVER_AUTH_ADMINS=alice, bob\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p, "")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.AuthMode != "off" {
		t.Errorf("AuthMode 应从 env 文件读到 off，得到 %q", cfg.AuthMode)
	}
	if len(cfg.AuthAdmins) != 2 || cfg.AuthAdmins[0] != "alice" || cfg.AuthAdmins[1] != "bob" {
		t.Errorf("AuthAdmins 解析错误: %v", cfg.AuthAdmins)
	}
}
