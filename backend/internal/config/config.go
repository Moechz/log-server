// Package config 负责加载运行时配置。
//
// 优先级（高 → 低）：环境变量（log-server-for-tos.env） > config.json > 内置默认值。
// 依据：打包指南坑 11f 规则 3 —— 实际路径写进服务 env，config.json 只留兜底值。
package config

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ResourceMode 资源档位（D-009）。
type ResourceMode string

const (
	ModeLow         ResourceMode = "low"
	ModeStandard    ResourceMode = "standard"
	ModePerformance ResourceMode = "performance"
)

// Config 是应用的全部可配置项。
type Config struct {
	ResourceMode ResourceMode

	HTTPAddr string
	HTTPPort int

	SyslogUDPPort int
	SyslogTCPPort int
	SyslogTLSPort int
	TLSCertFile   string
	TLSKeyFile    string

	// AuthMode / AuthAdmins 来自 env（log-server-for-tos.env）：tos = 要求 TOS 会话；off = 仅本地开发
	AuthMode   string
	AuthAdmins []string

	// 系统日志（journald）采集：auto = 有权限才启动（默认）；on = 强制（无权限则如实降级）；off = 关闭
	// ★ 本应用**只读**：没有权限时绝不自我授权（加组/改 ACL 都在 D-017 黑名单）
	JournaldEnabled string
	JournaldSince   string   // journalctl --since（默认 "now"：只收新日志，避免重放历史）
	JournaldUnits   []string // 仅采集这些 unit（空 = 全部）
	JournaldExtra   []string // 追加给 journalctl 的额外参数（高级用法）

	SNMPTrapPort int

	// 接收面准入与限速（FR-603/604）：CIDR 列表（空 allow = 全放行）+ 每来源每秒上限。
	ReceiveAllow  []string
	ReceiveDeny   []string
	ReceiveMaxEPS int
	ReceiveMaxBPS int64

	VLogsAddr     string
	VLogsPort     int
	VLogsStorage  string
	RetentionDays int
	MaxDiskBytes  int64

	DataDir   string
	QueueMax  int64
	ConfigDir string

	// 入库磁盘队列（WAL，D-030）：采集/接收先落这里，再批量写存储。
	// 0 = 按队列目录所在卷剩余空间的 10% 兜底；默认 1 GiB。
	IngestQueueMax int64
	IngestQueueDir string
}

// Default 返回内置默认值（与 packaging/templates/log-server-for-tos.env.example 保持一致）。
func Default() Config {
	return Config{
		ResourceMode:    ModeLow,
		HTTPAddr:        "127.0.0.1",
		HTTPPort:        15280,
		JournaldEnabled: "auto",
		JournaldSince:   "now",
		SyslogUDPPort:   514,
		SyslogTCPPort:   514,
		SyslogTLSPort:   6514,
		SNMPTrapPort:    15162,
		VLogsAddr:       "127.0.0.1",
		VLogsPort:       19428,
		VLogsStorage:    "/Volume1/@apps/log-server-for-tos/data/engine",
		RetentionDays:   14,
		MaxDiskBytes:    20 << 30, // 20 GiB
		DataDir:         "/Volume1/@apps/log-server-for-tos/data",
		QueueMax:        1 << 30, // 1 GiB（转发目标队列）
		IngestQueueMax:  1 << 30, // 1 GiB（入库队列，实际会按卷剩余收紧）
	}
}

// Load 读取配置文件与环境变量并合并。
// 文件不存在不算错误（安装期尚未生成时也应能启动）。
func Load(envFile, jsonFile string) (Config, error) {
	cfg := Default()

	if envFile != "" {
		if err := applyEnvFile(&cfg, envFile); err != nil && !os.IsNotExist(err) {
			return cfg, fmt.Errorf("读取 env 文件失败: %w", err)
		}
	}
	applyProcessEnv(&cfg)

	if jsonFile != "" {
		if err := applyJSON(&cfg, jsonFile); err != nil && !os.IsNotExist(err) {
			return cfg, fmt.Errorf("读取 config.json 失败: %w", err)
		}
	}
	return cfg, cfg.Validate()
}

// Validate 做启动前校验（FR-308：路径强校验）。
func (c *Config) Validate() error {
	switch c.ResourceMode {
	case ModeLow, ModeStandard, ModePerformance:
	default:
		return fmt.Errorf("未知资源档位 %q（应为 low/standard/performance）", c.ResourceMode)
	}
	for name, p := range map[string]int{
		"HTTPPort": c.HTTPPort, "SyslogUDPPort": c.SyslogUDPPort,
		"SyslogTCPPort": c.SyslogTCPPort, "SyslogTLSPort": c.SyslogTLSPort,
		"SNMPTrapPort": c.SNMPTrapPort, "VLogsPort": c.VLogsPort,
	} {
		if p < 1 || p > 65535 {
			return fmt.Errorf("%s=%d 不是合法端口", name, p)
		}
	}
	if c.DataDir == "" || !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("DataDir 必须是绝对路径，当前为 %q", c.DataDir)
	}
	if c.VLogsStorage == "" || !filepath.IsAbs(c.VLogsStorage) {
		return fmt.Errorf("VLogsStorage 必须是绝对路径，当前为 %q", c.VLogsStorage)
	}
	// 硬约束：运行时数据不得落在系统分区（打包指南坑 11f）
	for _, p := range []string{c.DataDir, c.VLogsStorage} {
		if isSystemPath(p) {
			return fmt.Errorf("拒绝在系统分区写入运行时数据: %s（必须落在 /Volume* 数据卷）", p)
		}
		if IsTmpfs(p) {
			return fmt.Errorf("拒绝把运行时数据放在 tmpfs（重启即丢）: %s", p)
		}
	}
	return nil
}

// isSystemPath 判断是否系统分区路径（安装期以外不允许写）。
func isSystemPath(p string) bool {
	for _, bad := range []string{"/var/lib/", "/etc/", "/usr/", "/bin/", "/sbin/", "/boot/", "/root/"} {
		if strings.HasPrefix(p, bad) {
			return true
		}
	}
	return p == "/"
}

// IsTmpfs 通过 /proc/mounts 判断路径是否位于 tmpfs（FR-308 强校验项之一）。
func IsTmpfs(path string) bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false // 非 Linux 或读不到时不阻塞启动
	}
	defer f.Close()

	best, fstype := "", ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		mnt := strings.ReplaceAll(fields[1], `\040`, " ")
		if path == mnt || strings.HasPrefix(path, strings.TrimSuffix(mnt, "/")+"/") {
			if len(mnt) >= len(best) {
				best, fstype = mnt, fields[2]
			}
		}
	}
	return fstype == "tmpfs"
}

func applyEnvFile(c *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		c.set(k, v)
	}
	return sc.Err()
}

func applyProcessEnv(c *Config) {
	for _, e := range os.Environ() {
		k, v, ok := strings.Cut(e, "=")
		if !ok || !strings.HasPrefix(k, "LOGSERVER_") && !strings.HasPrefix(k, "VLOGS_") {
			continue
		}
		c.set(k, v)
	}
}

// set 把「键 → 字段」的映射集中在一处，便于审计与测试。
func (c *Config) set(key, val string) {
	switch key {
	case "LOGSERVER_HTTP_ADDR":
		c.HTTPAddr = val
	case "LOGSERVER_HTTP_PORT":
		c.HTTPPort = atoi(val, c.HTTPPort)
	case "LOGSERVER_SYSLOG_UDP_PORT":
		c.SyslogUDPPort = atoi(val, c.SyslogUDPPort)
	case "LOGSERVER_SYSLOG_TCP_PORT":
		c.SyslogTCPPort = atoi(val, c.SyslogTCPPort)
	case "LOGSERVER_SYSLOG_TLS_PORT":
		c.SyslogTLSPort = atoi(val, c.SyslogTLSPort)
	case "LOGSERVER_SYSLOG_TLS_CERT":
		c.TLSCertFile = val
	case "LOGSERVER_SYSLOG_TLS_KEY":
		c.TLSKeyFile = val
	case "LOGSERVER_SNMP_TRAP_PORT":
		c.SNMPTrapPort = atoi(val, c.SNMPTrapPort)
	case "LOGSERVER_RECEIVE_ALLOW_CIDRS":
		c.ReceiveAllow = splitCSV(val)
	case "LOGSERVER_RECEIVE_DENY_CIDRS":
		c.ReceiveDeny = splitCSV(val)
	case "LOGSERVER_RECEIVE_MAX_EPS_PER_SOURCE":
		c.ReceiveMaxEPS = atoi(val, c.ReceiveMaxEPS)
	case "LOGSERVER_RECEIVE_MAX_BYTES_PER_SEC":
		c.ReceiveMaxBPS = atoi64(val, c.ReceiveMaxBPS)
	case "LOGSERVER_DATA_DIR":
		c.DataDir = val
		c.ConfigDir = filepath.Join(val, "config")
	case "LOGSERVER_QUEUE_MAX_BYTES":
		c.QueueMax = atoi64(val, c.QueueMax)
	case "LOGSERVER_INGEST_QUEUE_MAX_BYTES":
		c.IngestQueueMax = atoi64(val, c.IngestQueueMax)
	case "LOGSERVER_INGEST_QUEUE_DIR":
		c.IngestQueueDir = val
	case "LOGSERVER_RESOURCE_MODE":
		c.ResourceMode = ResourceMode(val)
	case "LOGSERVER_JOURNALD_ENABLED":
		c.JournaldEnabled = val
	case "LOGSERVER_JOURNALD_SINCE":
		c.JournaldSince = val
	case "LOGSERVER_JOURNALD_UNITS":
		c.JournaldUnits = splitCSV(val)
	case "LOGSERVER_JOURNALD_EXTRA_ARGS":
		c.JournaldExtra = splitFields(val)
	case "LOGSERVER_AUTH":
		c.AuthMode = val
	case "LOGSERVER_AUTH_ADMINS":
		parts := strings.Split(val, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		c.AuthAdmins = out
	case "VLOGS_HTTP_ADDR":
		c.VLogsAddr = val
	case "VLOGS_HTTP_PORT":
		c.VLogsPort = atoi(val, c.VLogsPort)
	case "VLOGS_STORAGE_PATH":
		c.VLogsStorage = val
	case "VLOGS_RETENTION_PERIOD":
		c.RetentionDays = parseRetention(val, c.RetentionDays)
	case "VLOGS_RETENTION_MAX_DISK_SPACE":
		c.MaxDiskBytes = parseBytes(val, c.MaxDiskBytes)
	}
}

// jsonShape 是 config.json 的顶层结构（只覆盖 env 未提供时的兜底值）。
type jsonShape struct {
	ResourceMode string `json:"resource_mode"`
	Storage      struct {
		Driver    string `json:"driver"`
		DSN       string `json:"dsn"`
		MaxDiskGB int64  `json:"max_disk_gb"`
	} `json:"storage"`
	RetentionProfiles []struct {
		Name string `json:"name"`
		Days int    `json:"days"`
	} `json:"retention_profiles"`
}

func applyJSON(c *Config, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var j jsonShape
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	if j.ResourceMode != "" && c.ResourceMode == ModeLow {
		c.ResourceMode = ResourceMode(j.ResourceMode)
	}
	if j.Storage.MaxDiskGB > 0 && c.MaxDiskBytes == Default().MaxDiskBytes {
		c.MaxDiskBytes = j.Storage.MaxDiskGB << 30
	}
	return nil
}

func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitFields 按空白切分（用于 EXTRA_ARGS 这类透传参数）。
func splitFields(v string) []string {
	return strings.Fields(v)
}

// ── 小工具 ────────────────────────────────────────────────────────────
func atoi(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func atoi64(s string, def int64) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		return n
	}
	return def
}

// parseRetention 解析 "14d" / "8w" / "6M" / "1y"，返回天数。
func parseRetention(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	unit := s[len(s)-1]
	n := atoi(s[:len(s)-1], 0)
	if n <= 0 {
		return def
	}
	switch unit {
	case 'd', 'D':
		return n
	case 'w', 'W':
		return n * 7
	case 'M':
		return n * 30
	case 'y', 'Y':
		return n * 365
	default:
		return atoi(s, def)
	}
}

// parseBytes 解析 "20GiB" / "512MiB" / "1073741824"。
func parseBytes(s string, def int64) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
	} {
		if strings.HasSuffix(s, suf.s) {
			mult = suf.m
			s = strings.TrimSuffix(s, suf.s)
			break
		}
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && n > 0 {
		return n * mult
	}
	return def
}
