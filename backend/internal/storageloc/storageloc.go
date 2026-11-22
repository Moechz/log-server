// Package storageloc 管理**可配置的存储位置**（FR-307 / D-023）。
//
// 设计（重启生效）：
//   - 用户选择的引擎目录持久化在 <DataDir>/config/storage.json（小引导文件，留在安装期数据卷）；
//   - 主进程启动时读取它，把它作为 VictoriaLogs 的存储路径；
//   - 独立引擎服务在启动时通过 `log-server-for-tos --print-engine-path` 解析同一份配置，
//     因此「保存新位置 → 重启应用」两个进程都会落到新目录，无需任何特权操作。
//
// 路径策略：只允许 /Volume* 数据卷下的绝对路径，拒绝系统分区与 tmpfs（FR-308）。
package storageloc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ConfigFile 返回引导配置文件路径。
func ConfigFile(dataDir string) string {
	return filepath.Join(dataDir, "config", "storage.json")
}

type doc struct {
	EngineDir string `json:"engine_dir"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Load 读取用户选择的引擎目录；文件缺失/损坏时回退 fallback（env 默认值）。
func Load(file, fallback string) string {
	b, err := os.ReadFile(file)
	if err != nil {
		return fallback
	}
	var d doc
	if json.Unmarshal(b, &d) != nil || strings.TrimSpace(d.EngineDir) == "" {
		return fallback
	}
	return d.EngineDir
}

// Save 原子写入引擎目录配置。
func Save(file, engineDir string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(doc{
		EngineDir: engineDir,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// ── 路径策略（FR-308）───────────────────────────────────────────────

// Validate 校验目标目录：绝对路径、位于 /Volume* 数据卷、非系统分区、非 tmpfs。
func Validate(p string) error {
	roots, _ := filepath.Glob("/Volume*")
	return validateUnder(p, roots)
}

func validateUnder(p string, roots []string) error {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		return fmt.Errorf("路径必须是绝对路径：%q", p)
	}
	p = filepath.Clean(p)
	if isSystemPath(p) {
		return fmt.Errorf("拒绝在系统分区写入运行时数据：%s（必须落在 /Volume* 数据卷）", p)
	}
	ok := false
	for _, r := range roots {
		r = filepath.Clean(r)
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("存储位置必须位于已挂载的数据卷（/Volume*）下：%s", p)
	}
	if isTmpfs(p) {
		return fmt.Errorf("拒绝把运行时数据放在 tmpfs（重启即丢）：%s", p)
	}
	return nil
}

func isSystemPath(p string) bool {
	for _, bad := range []string{"/var/lib/", "/etc/", "/usr/", "/bin/", "/sbin/", "/boot/", "/root/", "/proc/", "/sys/", "/dev/"} {
		if strings.HasPrefix(p, bad) {
			return true
		}
	}
	return p == "/"
}

func isTmpfs(path string) bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n, _ := f.Read(buf)
	best, fstype := "", ""
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		fields := strings.Fields(line)
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

// ── 目录浏览（供 UI 选择存储位置）───────────────────────────────────

// DirEntry 是浏览结果中的一项。
type DirEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	IsVolume bool   `json:"is_volume"`
	FreeKB   uint64 `json:"free_kb"`
}

// Volumes 返回已挂载的数据卷根目录（/Volume1、/Volume2 ...）。
func Volumes() []DirEntry {
	roots, _ := filepath.Glob("/Volume*")
	out := make([]DirEntry, 0, len(roots))
	for _, r := range roots {
		fi, err := os.Stat(r)
		if err != nil || !fi.IsDir() {
			continue
		}
		_, free, _ := DiskUsage(r)
		out = append(out, DirEntry{Name: filepath.Base(r), Path: r, IsVolume: true,
			Writable: isWritable(r) == nil, FreeKB: free / 1024})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// List 列出 path 下的子目录（非递归，跳过隐藏项与平台 @apps）；path 为空时返回数据卷根。
func List(path string) ([]DirEntry, error) {
	if path == "" {
		return Volumes(), nil
	}
	if err := Validate(path); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取目录：%w", err)
	}
	out := make([]DirEntry, 0, len(ents))
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || e.Name() == "@apps" {
			continue
		}
		full := filepath.Join(path, e.Name())
		out = append(out, DirEntry{Name: e.Name(), Path: full, Writable: isWritable(full) == nil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DiskUsage 返回路径所在文件系统的总容量与可用字节。
func DiskUsage(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), nil
}

// isWritable 通过真实创建临时文件判断目录对当前服务用户是否可写。
func isWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".ls-wtest-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// EnsureWritable 校验并（必要时）创建目录，确保服务用户可写。
func EnsureWritable(dir string, create bool) error {
	if err := Validate(dir); err != nil {
		return err
	}
	if create {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("无法创建目录（请在 TOS 文件管理器把该共享文件夹授予本应用用户写权限）：%w", err)
		}
	}
	if err := isWritable(dir); err != nil {
		return fmt.Errorf("目录不可写：%s（请在 TOS 文件管理器把该共享文件夹授予本应用用户写权限）", dir)
	}
	return nil
}
