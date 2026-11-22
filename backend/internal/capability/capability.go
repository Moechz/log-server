// Package capability 是**能力探测层**（D-014 / D-017）。
//
// 硬约束：
//   - 业务代码**不得**直接调用平台能力，必须经这里；
//   - 探测结果必须是**三态**（D-017）：官方已提供 / 不可用 / 我方应急自建；
//   - 能力未就绪时 UI 如实标注「待平台支持」，**不做任何绕过**。
package capability

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ID 是能力编号（对应对官方的需求 R1–R9 + TOS 鉴权）。
type ID string

const (
	CapSystemLogRead   ID = "R1"   // 系统日志读取权限（journald）
	CapLogPersistence  ID = "R2"   // 系统日志持久化落点
	CapAudit           ID = "R3"   // 主机审计能力（auditd）
	CapContainerLogs   ID = "R4"   // 容器日志只读出口
	CapPlatformHealth  ID = "R5"   // TOS 日志中心 / 硬件健康只读接口
	CapLowPorts        ID = "R6"   // 标准低端口（514/6514）能力
	CapAppDataBackup   ID = "R7"   // 应用数据备份/迁移契约
	CapMsgTypePersist  ID = "R8"   // 通知类型注册持久性
	CapDeclarativePerm ID = "R9"   // 声明式权限/能力框架（架构级建议）
	CapTOSAuth         ID = "AUTH" // TOS 账号鉴权
)

// State 是三态之一（D-017）。
type State string

const (
	StateAvailable   State = "available"   // 官方通道可用
	StateUnavailable State = "unavailable" // 不可用（UI 置灰 + 说明原因）
	StateSelfBuilt   State = "self_built"  // 我方应急自建（按 D-017 默认不会出现）
)

// Status 是一项能力的探测结果。
type Status struct {
	ID        ID        `json:"id"`
	Title     string    `json:"title"`
	State     State     `json:"state"`
	Reason    string    `json:"reason,omitempty"`   // 面向用户的原因说明
	Evidence  string    `json:"evidence,omitempty"` // 探测依据（进诊断包）
	MinTOS    string    `json:"min_tos,omitempty"`  // 需要的最低 TOS 版本
	CheckedAt time.Time `json:"checked_at"`
}

// Available 是给业务层用的便捷判断。
func (s Status) Available() bool { return s.State == StateAvailable }

// Probe 是单项探测函数。
// 注意：不要写成 (State, reason, evidence string) —— Go 会把三个名字都当作 string。
type Probe func(context.Context) (State, string, string)

// Registry 保存全部能力的探测函数与最近一次结果。
type Registry struct {
	mu     sync.RWMutex
	probes map[ID]probeEntry
	last   map[ID]Status
}

type probeEntry struct {
	title  string
	minTOS string
	fn     Probe
}

// New 构造注册表并登记全部已知能力。
func New() *Registry {
	r := &Registry{probes: map[ID]probeEntry{}, last: map[ID]Status{}}
	r.register(CapSystemLogRead, "系统日志读取（journald）", "TOS 7.x 待官方提供", probeJournalReadable)
	r.register(CapLogPersistence, "系统日志持久化落点", "TOS 7.x 待官方提供", probeLogPersistence)
	r.register(CapAudit, "主机审计能力（auditd / eBPF）", "TOS 7.x 待官方提供", probeAudit)
	r.register(CapContainerLogs, "容器日志只读出口", "TOS 7.x 待官方提供", probeContainerLogs)
	r.register(CapPlatformHealth, "TOS 日志中心 / 硬件健康只读接口", "TOS 7.x 待官方提供", probePlatformHealth)
	r.register(CapLowPorts, "标准低端口（514/6514）绑定能力", "", probeLowPorts)
	r.register(CapAppDataBackup, "应用数据备份/迁移契约", "待官方明确", probeUnknown("官方尚未明确应用数据是否纳入系统备份"))
	r.register(CapMsgTypePersist, "通知类型注册持久性", "待官方明确", probeMsgType)
	r.register(CapDeclarativePerm, "声明式权限/能力框架", "建议平台抽象", probeUnknown("平台尚未提供声明式权限框架"))
	r.register(CapTOSAuth, "TOS 账号与会话（复用）", "TOS 7.x", probeTOSAuth)
	return r
}

func (r *Registry) register(id ID, title, minTOS string, fn Probe) {
	r.probes[id] = probeEntry{title: title, minTOS: minTOS, fn: fn}
}

// Check 立即执行一轮探测。
func (r *Registry) Check(ctx context.Context) []Status {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Status, 0, len(r.probes))
	for id, e := range r.probes {
		st, reason, ev := StateUnavailable, "", ""
		if e.fn != nil {
			st, reason, ev = e.fn(ctx)
		}
		s := Status{
			ID: id, Title: e.title, State: st, Reason: reason,
			Evidence: ev, MinTOS: e.minTOS, CheckedAt: time.Now().UTC(),
		}
		r.last[id] = s
		out = append(out, s)
	}
	return out
}

// Snapshot 返回最近一次探测结果（不触发探测）。
func (r *Registry) Snapshot() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Status, 0, len(r.last))
	for _, s := range r.last {
		out = append(out, s)
	}
	return out
}

// Get 取单项。
func (r *Registry) Get(id ID) (Status, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.last[id]
	return s, ok
}

// Start 周期性复检（D-014 要求周期性）。ctx 取消即停止。
func (r *Registry) Start(ctx context.Context, every time.Duration) {
	r.Check(ctx)
	if every <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.Check(ctx)
			}
		}
	}()
}

// ── 具体探测器（全部只读；不做任何绕过，只如实报告）──────────────────

func probeJournalReadable(_ context.Context) (State, string, string) {
	// ★ 真正的门槛是「能否读到 machine-id 子目录里的 journal 文件」。
	//   仅能列 /run/log/journal 是**假阳性**（该目录对其他用户可读，
	//   而真正的 ACL 在 /run/log/journal/<machine-id>/ 上，实测只授 adm 组）。
	mid, err := readMachineID()
	if err != nil {
		return StateUnavailable, "无法确定 machine-id", err.Error()
	}
	dir := "/run/log/journal/" + mid
	if _, err := os.Stat(dir); err != nil {
		// 回退：持久化目录（若平台做过持久化）
		dir = "/var/log/journal/" + mid
		if _, err := os.Stat(dir); err != nil {
			return StateUnavailable, "系统日志目录不存在或不可见", dir + ": " + err.Error()
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return StateUnavailable, "无权限读取系统日志（尚未获得平台授权）",
			"os.ReadDir(" + dir + "): " + err.Error()
	}

	// ★ 逐个尝试打开**全部** journal 文件，而不是只看第一个：
	//   真机踩坑（2026-10-14）：只给"已存在文件"加 ACL 时，目录可列、旧文件可读，
	//   但 journald 轮转后**新建的 journal 文件不继承 ACL** → 若只看第一个文件会得出
	//   错误结论；而"部分可读"对采集同样是坏状态（会丢最新日志）→ 必须如实上报不可用。
	total, readable := 0, 0
	firstErr := ""
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".journal") {
			continue
		}
		total++
		f, err := os.Open(dir + "/" + e.Name())
		if err != nil {
			if firstErr == "" {
				firstErr = "os.Open(" + dir + "/" + e.Name() + "): " + err.Error()
			}
			continue
		}
		_ = f.Close()
		readable++
	}
	switch {
	case total == 0:
		return StateUnavailable, "日志目录中无 journal 文件", dir
	case readable == 0:
		return StateUnavailable, "无权限读取系统日志文件（尚未获得平台授权）", firstErr
	case readable < total:
		return StateUnavailable,
			fmt.Sprintf("仅部分 journal 文件可读（%d/%d）：新建文件不可读（请平台用**组授权**或为目录补 default ACL）", readable, total),
			firstErr
	}
	return StateAvailable, "", fmt.Sprintf("可读取 %s 下全部 %d 个 journal 文件", dir, total)
}

// readMachineID 读取 /etc/machine-id（回退 /var/lib/dbus/machine-id）。
func readMachineID() (string, error) {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("未找到 machine-id")
}

func probeLogPersistence(_ context.Context) (State, string, string) {
	// 官方通道：/var/log 不是 tmpfs（重启不丢）。
	target, err := os.Readlink("/var/log")
	if err != nil {
		target = "/var/log"
	}
	if strings.HasPrefix(target, "/tmp") {
		return StateUnavailable, "系统日志位于内存（重启即丢）", "/var/log -> " + target
	}
	return StateAvailable, "", "/var/log 指向 " + target
}

func probeAudit(_ context.Context) (State, string, string) {
	if _, err := exec.LookPath("auditctl"); err != nil {
		return StateUnavailable, "系统未提供主机审计能力（auditd 未安装）", "auditctl 不在 PATH"
	}
	out, err := exec.Command("systemctl", "is-active", "auditd").Output()
	if err != nil || strings.TrimSpace(string(out)) != "active" {
		return StateUnavailable, "审计服务未运行", "systemctl is-active auditd => " + strings.TrimSpace(string(out))
	}
	return StateAvailable, "", "auditd 由平台提供且运行中"
}

func probeContainerLogs(_ context.Context) (State, string, string) {
	if _, err := os.ReadDir("/var/lib/docker/containers"); err != nil {
		return StateUnavailable, "无法读取容器日志目录（尚未获得平台授权）",
			"os.ReadDir(/var/lib/docker/containers): " + err.Error()
	}
	return StateAvailable, "", "可列出 /var/lib/docker/containers"
}

func probePlatformHealth(_ context.Context) (State, string, string) {
	// 官方通道：应提供只读接口（CLI JSON 或本地 HTTP）。
	for _, bin := range []string{"ter_smartfan", "ter_storage_device_tool"} {
		if _, err := exec.LookPath(bin); err == nil {
			// 存在工具但当前无机器可读输出 → 仍视为未就绪（如实报告）
			return StateUnavailable, "平台提供了健康工具但无机器可读接口（待官方提供）",
				bin + " 存在；未提供 JSON/HTTP 只读接口"
		}
	}
	return StateUnavailable, "未发现平台健康只读接口", "未找到 ter_* 健康工具"
}

// capNetBindServiceBit 是 CAP_NET_BIND_SERVICE 在位图中的位号（内核 5.x 起为 10）。
const capNetBindServiceBit = 10

func probeLowPorts(_ context.Context) (State, string, string) {
	// 必须**真正检查 CapEff 的 bit10**，而不是「能读到 CapEff 就算可用」。
	// 依据：systemd 单元里我们只保留 CapabilityBoundingSet=CAP_NET_BIND_SERVICE。
	eff, err := effectiveCaps()
	if err != nil {
		return StateUnavailable, "无法确认低端口能力", err.Error()
	}
	if eff&(uint64(1)<<capNetBindServiceBit) == 0 {
		return StateUnavailable, "当前进程未持有 CAP_NET_BIND_SERVICE（无法绑定 514/6514）",
			fmt.Sprintf("CapEff=%#x，缺少 bit%d", eff, capNetBindServiceBit)
	}
	return StateAvailable, "", fmt.Sprintf("CapEff=%#x 含 CAP_NET_BIND_SERVICE（bit%d）", eff, capNetBindServiceBit)
}

// effectiveCaps 解析 /proc/self/status 的 CapEff 十六进制位图。
func effectiveCaps() (uint64, error) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "CapEff:") {
			continue
		}
		hexs := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
		v, err := strconv.ParseUint(hexs, 16, 64)
		if err != nil {
			return 0, fmt.Errorf("解析 CapEff=%q 失败: %w", hexs, err)
		}
		return v, nil
	}
	return 0, fmt.Errorf("未找到 CapEff")
}

func probeMsgType(_ context.Context) (State, string, string) {
	dir := "/etc/notifications/msgType"
	if _, err := os.Stat(dir); err != nil {
		return StateUnavailable, "平台通知类型目录不可用", dir + ": " + err.Error()
	}
	// 注：即使可读，升级后是否保留仍待官方明确 → 保持 unavailable，UI 如实标注
	return StateUnavailable, "目录存在，但升级后是否保留待官方明确", dir + " 可访问"
}

// probeTOSAuth 检查 TOS 会话复用是否可用（官方文档 §8.10）。
//
// 判定：文档已提供 cookie 机制（TMSESSNAME / X-Csrf-Token）→ 机制可用；
// 但"服务端校验会话有效性"与"查询当前用户/角色"的端点**未在文档中给出** →
// 当前实现只做存在性 + CSRF 一致性校验，故附带明确说明。
func probeTOSAuth(_ context.Context) (State, string, string) {
	return StateAvailable,
		"会话复用已实现（官方文档 §8.10）；服务端校验/取用户端点未公开，当前仅做存在性 + CSRF 一致性校验",
		"cookie: TMSESSNAME / X-Csrf-Token；两通道（标准 Cookie、自定义 Cookie 头）均已支持"
}

func probeUnknown(reason string) Probe {
	return func(context.Context) (State, string, string) {
		return StateUnavailable, reason, "官方尚未确认"
	}
}
