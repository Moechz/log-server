// Package alert 实现告警规则引擎（M8 / FR-801~807）。
//
// 内置规则覆盖 DEV-REQUIREMENTS 附录 D 的清单；支持频率/阈值规则与抑制聚合，
// 避免告警风暴（FR-804）。
package alert

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

// Level 告警级别（对齐 syslog severity：数值越小越严重）。
type Level int

const (
	LevelCrit Level = 2 // crit
	LevelWarn Level = 4 // warning
	LevelInfo Level = 6 // info
)

// Label 返回中文标签。
func (l Level) Label() string {
	switch l {
	case LevelCrit:
		return "严重"
	case LevelWarn:
		return "警告"
	default:
		return "提示"
	}
}

// Rule 是一条告警规则。
type Rule struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Category  string        `json:"category"` // 内存/磁盘/硬件/阵列/内核/服务/安全/容器/应用/管道
	Pattern   string        `json:"pattern"`  // 正则（对原文匹配）
	Level     Level         `json:"level"`
	Enabled   bool          `json:"enabled"`
	Window    time.Duration `json:"window,omitempty"`    // 频率窗口（0 = 命中即告警）
	Threshold int           `json:"threshold,omitempty"` // 窗口内阈值
	Suppress  time.Duration `json:"suppress,omitempty"`  // 抑制期（同键不重复告警）

	re *regexp.Regexp
}

// Alert 是一条告警实例。
type Alert struct {
	ID        string    `json:"id"`
	RuleID    string    `json:"rule_id"`
	RuleName  string    `json:"rule_name"`
	Category  string    `json:"category"`
	Level     Level     `json:"level"`
	LevelText string    `json:"level_text"`
	Summary   string    `json:"summary"`
	Host      string    `json:"host"`
	App       string    `json:"app"`
	At        time.Time `json:"at"`
	Count     int       `json:"count"`
	State     string    `json:"state"` // firing | resolved
	SampleRaw string    `json:"sample_raw"`

	// FR-804：人工确认（不改变 firing 状态，仅标记“已看到/已处理中”）
	Acked   bool       `json:"acked,omitempty"`
	AckedAt *time.Time `json:"acked_at,omitempty"`
	AckedBy string     `json:"acked_by,omitempty"`
}

// Silence 是一条静默规则（FR-804）：在 Until 之前，匹配 scope 的告警不再产生、不再通知。
// RuleID/Host/App 为空即“不限”；三者全空 = 全局维护窗口。
type Silence struct {
	ID          string    `json:"id"`
	RuleID      string    `json:"rule_id,omitempty"`
	Host        string    `json:"host,omitempty"`
	App         string    `json:"app,omitempty"`
	Until       time.Time `json:"until"`
	Reason      string    `json:"reason,omitempty"`
	By          string    `json:"by,omitempty"`
	Maintenance bool      `json:"maintenance,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Match 判断该静默是否命中给定告警维度。
func (s Silence) Match(ruleID, host, app string) bool {
	if s.RuleID != "" && s.RuleID != ruleID {
		return false
	}
	if s.Host != "" && s.Host != host {
		return false
	}
	if s.App != "" && s.App != app {
		return false
	}
	return true
}

// Config 引擎配置。
type Config struct {
	RecentMax       int             // 保留最近多少条告警
	SuppressBy      time.Duration   // 默认抑制期（规则未指定时）
	OnAlert         func(Alert)     // 新告警回调（用于写 selflog / 通知中心）
	OnSilenceChange func([]Silence) // 静默/维护窗口变化回调（用于持久化）
}

// Engine 是规则引擎。
type Engine struct {
	cfg Config

	mu       sync.Mutex
	rules    map[string]Rule
	active   map[string]*Alert // dedupKey → 当前活跃告警
	recent   []Alert
	window   map[string][]time.Time // ruleID → 命中时间戳（频率规则用）
	seq      uint64
	hits     map[string]uint64
	silences []Silence
	silSeq   uint64
}

// New 创建引擎。
func New(cfg Config) *Engine {
	if cfg.RecentMax <= 0 {
		cfg.RecentMax = 500
	}
	if cfg.SuppressBy <= 0 {
		cfg.SuppressBy = 5 * time.Minute
	}
	return &Engine{
		cfg: cfg, rules: map[string]Rule{}, active: map[string]*Alert{},
		window: map[string][]time.Time{}, hits: map[string]uint64{},
	}
}

// Add 添加/更新规则（编译失败即返回错误，不影响已有规则）。
func (e *Engine) Add(rules ...Rule) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range rules {
		if r.ID == "" {
			return fmt.Errorf("规则缺少 id")
		}
		if r.Pattern == "" {
			return fmt.Errorf("规则 %s 缺少 pattern", r.ID)
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return fmt.Errorf("规则 %s 正则非法: %w", r.ID, err)
		}
		r.re = re
		if r.Level == 0 {
			r.Level = LevelWarn
		}
		if r.Window <= 0 && r.Threshold > 0 {
			r.Window = time.Minute
		}
		e.rules[r.ID] = r
	}
	return nil
}

// Rules 返回当前规则。
func (e *Engine) Rules() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Rule, 0, len(e.rules))
	for _, r := range e.rules {
		out = append(out, r)
	}
	return out
}

// Evaluate 对一条记录求值（O(规则数)，正则已在编译期准备）。
func (e *Engine) Evaluate(r *model.Record) {
	if r == nil {
		return
	}
	target := r.Raw
	if r.Msg != "" {
		target = r.Raw // 始终对**原文**匹配，避免解析失败漏报
	}

	e.mu.Lock()
	var fired []Alert
	for _, rule := range e.rules {
		if !rule.Enabled || rule.re == nil {
			continue
		}
		if !rule.re.MatchString(target) {
			continue
		}
		e.hits[rule.ID]++
		if rule.Threshold > 0 {
			// 频率规则：窗口内计数达标才告警
			now := time.Now()
			ts := append(e.window[rule.ID], now)
			cut := now.Add(-rule.Window)
			keep := ts[:0]
			for _, t := range ts {
				if t.After(cut) {
					keep = append(keep, t)
				}
			}
			e.window[rule.ID] = keep
			if len(keep) < rule.Threshold {
				continue
			}
			e.window[rule.ID] = nil // 触发后重置，避免持续刷屏
		}
		key := rule.ID + "|" + r.Host + "|" + r.App
		now := time.Now()
		// FR-804：静默 / 维护窗口内不产生新告警、不通知（仍计入 hits 与现有告警计数）
		if e.silencedLocked(rule.ID, r.Host, r.App, now) {
			if a, ok := e.active[key]; ok {
				a.Count++
			}
			continue
		}
		if a, ok := e.active[key]; ok {
			suppress := rule.Suppress
			if suppress <= 0 {
				suppress = e.cfg.SuppressBy
			}
			if now.Sub(a.At) < suppress {
				a.Count++ // 抑制期内只累加计数
				continue
			}
		}
		e.seq++
		a := Alert{
			ID: fmt.Sprintf("A%06d", e.seq), RuleID: rule.ID, RuleName: rule.Name,
			Category: rule.Category, Level: rule.Level, LevelText: rule.Level.Label(),
			Summary: summarize(r, rule), Host: r.Host, App: r.App,
			At: now, Count: 1, State: "firing", SampleRaw: truncate(r.Raw, 512),
		}
		e.active[key] = &a
		e.recent = append(e.recent, a)
		if len(e.recent) > e.cfg.RecentMax {
			e.recent = e.recent[len(e.recent)-e.cfg.RecentMax:]
		}
		fired = append(fired, a)
	}
	e.mu.Unlock()

	if e.cfg.OnAlert != nil {
		for _, a := range fired {
			e.cfg.OnAlert(a)
		}
	}
}

// Active 返回当前活跃告警（按时间倒序）。
func (e *Engine) Active() []Alert {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Alert, 0, len(e.active))
	for _, a := range e.active {
		out = append(out, *a)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].At.After(out[i].At) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Recent 返回最近告警（新的在前）。
func (e *Engine) Recent(n int) []Alert {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n <= 0 || n > len(e.recent) {
		n = len(e.recent)
	}
	out := make([]Alert, 0, n)
	for i := len(e.recent) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, e.recent[i])
	}
	return out
}

// Stats 汇总（按级别计数）。
type Stats struct {
	Active      int               `json:"active"`
	Acked       int               `json:"acked"`
	ByLevel     map[string]int    `json:"by_level"`
	Total       int               `json:"total"`
	RuleHits    map[string]uint64 `json:"rule_hits"`
	Silences    int               `json:"silences"`
	Maintenance bool              `json:"maintenance"`
}

// Stats 返回统计。
func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	st := Stats{Active: len(e.active), ByLevel: map[string]int{}, Total: len(e.recent), RuleHits: map[string]uint64{}}
	for _, a := range e.active {
		st.ByLevel[a.Level.Label()]++
		if a.Acked {
			st.Acked++
		}
	}
	for k, v := range e.hits {
		st.RuleHits[k] = v
	}
	for _, s := range e.silences {
		if s.Until.After(now) {
			st.Silences++
			if s.Maintenance {
				st.Maintenance = true
			}
		}
	}
	return st
}

// Resolve 手动解除一条活跃告警。
func (e *Engine) Resolve(ruleID, host string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, a := range e.active {
		if a.RuleID == ruleID && a.Host == host {
			delete(e.active, k)
			return true
		}
	}
	return false
}

func summarize(r *model.Record, rule Rule) string {
	parts := []string{rule.Name}
	if r.Host != "" {
		parts = append(parts, r.Host)
	}
	if r.App != "" {
		parts = append(parts, r.App)
	}
	return strings.Join(parts, " · ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// neverMatch 是 RE2 合法的"永不匹配"表达式（用于由其他层触发的规则）。
// 注意：Go 的 regexp 是 RE2，**不支持 lookahead**（`(?!)` 会编译失败）。
const neverMatch = `[^\s\S]`

// BuiltinRules 返回内置规则集（DEV-REQUIREMENTS 附录 D）。
func BuiltinRules() []Rule {
	type def struct {
		id, name, cat, pat string
		level              Level
		window             time.Duration
		threshold          int
	}
	defs := []def{
		{"oom", "内存不足（OOM）", "内存", `(?i)Out of memory|oom-kill|Killed process`, LevelCrit, 0, 0},
		{"disk-full", "磁盘空间耗尽", "磁盘", `(?i)No space left on device`, LevelCrit, 0, 0},
		{"fs-ro", "文件系统转为只读", "磁盘", `(?i)Remounting filesystem read-only|Read-only file system`, LevelCrit, 0, 0},
		{"io-error", "磁盘 I/O 错误", "硬件", `(?i)I/O error|medium error|blk_update_request`, LevelCrit, 0, 0},
		{"mce", "硬件机器检查异常", "硬件", `(?i)Machine Check Exception|EDAC .*UE|Uncorrectable error`, LevelCrit, 0, 0},
		{"raid-degraded", "RAID 降级/磁盘故障", "阵列", `(?i)Disk failure on|raid.*degraded|array.*degraded|PD .*is not ONLINE`, LevelCrit, 0, 0},
		{"kernel-panic", "内核崩溃/挂死", "内核", `(?i)Kernel panic|hung_task|soft lockup|Call Trace`, LevelCrit, 0, 0},
		{"svc-failed", "服务启动失败/反复重启", "服务", `(?i)Failed with result|start request repeated too quickly|Main process exited`, LevelWarn, 0, 0},
		{"core-dump", "进程崩溃", "服务", `(?i)core dumped|segfault at`, LevelWarn, 0, 0},
		{"ssh-brute", "SSH 爆破嫌疑", "安全", `(?i)Failed password|Invalid user|authentication failure`, LevelWarn, 5 * time.Minute, 10},
		{"ssh-accept", "SSH 登录成功（需人工确认来源）", "安全", `(?i)Accepted (password|publickey)`, LevelInfo, 0, 0},
		{"sudo-denied", "提权被拒", "安全", `(?i)sudo:.*NOT in sudoers|incorrect password attempts`, LevelWarn, 0, 0},
		{"acct-change", "账户或组变更", "安全", `(?i)new user:|useradd|usermod|userdel|groupadd|gpasswd`, LevelWarn, 0, 0},
		{"container-oom", "容器被 OOM 终止", "容器", `(?i)OOMKilled|Evicted|CrashLoopBackOff|Liveness probe failed`, LevelWarn, 0, 0},
		{"http-5xx", "HTTP 5xx", "应用", `(?i)"\s+5\d\d\s`, LevelWarn, 0, 0},
		{"cert-expiry", "证书即将/已过期", "应用", `(?i)certificate (has )?expired|certificate will expire`, LevelWarn, 0, 0},
		{"fw-block", "防火墙拦截", "安全", `(?i)UFW BLOCK|iptables.*DROP|firewalld.*REJECT`, LevelInfo, 0, 0},
		{"audit-lost", "审计丢事件", "管道", `(?i)audit:.*lost \d+`, LevelCrit, 0, 0},
		{"collector-silent", "采集源静默（由采集层触发）", "管道", neverMatch, LevelWarn, 0, 0},
		{"fwd-backlog", "转发队列积压（由转发层触发）", "管道", neverMatch, LevelWarn, 0, 0},
	}
	out := make([]Rule, 0, len(defs))
	for _, d := range defs {
		r := Rule{
			ID: d.id, Name: d.name, Category: d.cat, Pattern: d.pat,
			Level: d.level, Enabled: true, Window: d.window, Threshold: d.threshold,
		}
		// 由其他层触发的规则默认不参与文本匹配
		if d.pat == neverMatch {
			r.Enabled = false
		}
		out = append(out, r)
	}
	return out
}
