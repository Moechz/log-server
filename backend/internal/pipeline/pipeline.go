// Package pipeline 把原始字节变成 model.Record：解析 → 归一化 → 分类。
//
// 对齐决策：D-005（三段式）、D-006（级别双字段）、D-019（时间闸门）、D-020（字段命名空间）。
// 硬规则：**解析失败也返回 Record（带 ParseError），绝不丢数据**（FR-206）。
package pipeline

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Moechz/log-server/backend/internal/encoding"
	"github.com/Moechz/log-server/backend/internal/model"
	"github.com/Moechz/log-server/backend/internal/receive"
	"github.com/Moechz/log-server/backend/internal/receive/syslog"
)

// Config 控制归一化与分类策略。
type Config struct {
	// TimeGate 时间闸门阈值（D-019）
	TimeGate time.Duration
	// DefaultRetention 无规则命中时的保留档案
	DefaultRetention string
	// AuditRetention 审计类用的保留档案
	AuditRetention string
}

// DefaultConfig 返回默认策略（与 packaging/templates 的 retention profile 命名一致）。
func DefaultConfig() Config {
	return Config{
		TimeGate:         model.TimeGateThreshold,
		DefaultRetention: "ops-14d",
		AuditRetention:   "security-180d",
	}
}

// Processor 是无状态处理器。
type Processor struct {
	cfg Config
}

// New 创建处理器。
func New(cfg Config) *Processor {
	if cfg.TimeGate <= 0 {
		cfg.TimeGate = model.TimeGateThreshold
	}
	if cfg.DefaultRetention == "" {
		cfg.DefaultRetention = "ops-14d"
	}
	if cfg.AuditRetention == "" {
		cfg.AuditRetention = "security-180d"
	}
	return &Processor{cfg: cfg}
}

// Process 把一条原始消息转成 Record（**永不返回 nil**）。
func (p *Processor) Process(raw []byte, src receive.SourceInfo, ingest time.Time) *model.Record {
	rec := &model.Record{
		Raw:        string(raw), // ① 原文逐字节保留
		IngestTime: ingest.UTC(),
		Source: model.Source{
			Kind:     kindOf(src.Protocol),
			IP:       src.IP,
			Port:     src.Port,
			Protocol: src.Protocol,
		},
	}

	// 编码探测与转 UTF-8（FR-204）：解析用 UTF-8 视图，原文（Raw）永不被改写。
	nz := encoding.Normalize(raw, "")
	rec.Encoding = nz.Name

	m, err := syslog.Parse(nz.UTF8)
	if err != nil {
		// 解析失败：原文仍入库，标记原因（FR-206）
		rec.ParseError = err.Error()
		rec.Msg = string(nz.UTF8)
		rec.Format = string(FormatPlain)
	} else {
		rec.PRI = ptr(m.PRI)
		rec.Facility = ptr(m.Facility)
		rec.Severity = ptr(m.Severity)
		rec.SeverityNumber = ptr(m.Severity) // syslog severity 与我们的归一值同域（D-006）
		rec.Host = m.Hostname
		rec.App = m.AppName
		rec.PID = ptrAtoi(m.ProcID)
		rec.Msg = m.Msg
		rec.Level = LevelName(m.Severity)
		rec.EventTimeRaw = m.TimestampRaw
		if m.Version > 0 {
			rec.Format = string(FormatRFC5424)
		} else {
			rec.Format = string(FormatRFC3164)
		}

		if !m.Timestamp.IsZero() {
			if m.HasYear {
				rec.EventTime = m.Timestamp.UTC()
			} else {
				// RFC3164 无年份 → 推断（D-019）
				rec.EventTime = model.InferYear(m.Timestamp, ingest.UTC()).UTC()
				rec.TimeInferred = true
			}
		}
	}

	if len(rec.User) == 0 && m != nil && len(m.StructuredData) > 0 {
		// RFC 5424 的结构化数据进"用户字段"（原样保留，不加前缀）
		rec.User = map[string]any{}
		for id, params := range m.StructuredData {
			for k, v := range params {
				rec.User[k] = v
			}
			_ = id
		}
	}

	return p.finalize(rec)
}

// ProcessFile 处理一条**文件采集**来的日志行（FR-102 / FR-201 / FR-204）。
//
// 与 Process 的差异：
//   - 来源是本地文件（Source.Kind = local_file，带 path）；
//   - 先做**格式自动识别**（JSON / logfmt / CLF / syslog / 纯文本），
//     结构化字段进"用户字段"（原样保留）并从常见键名提取派生字段（FR-202/203）；
//   - encHint 非空时按采集源声明的编码强制解码（遗留 GBK/Big5 日志建议显式声明）。
func (p *Processor) ProcessFile(raw []byte, path string, ingest time.Time, encHint string) *model.Record {
	rec := &model.Record{
		Raw:        string(raw),
		IngestTime: ingest.UTC(),
		Source:     model.Source{Kind: model.SrcLocalFile, Path: path},
	}
	nz := encoding.Normalize(raw, encHint)
	rec.Encoding = nz.Name
	text := string(nz.UTF8)
	rec.Format = string(DetectFormat(text))

	switch Format(rec.Format) {
	case FormatJSON:
		var m map[string]any
		if err := json.Unmarshal(nz.UTF8, &m); err == nil && m != nil {
			rec.User = m
			rec.Msg = text
			applyCommon(rec, lowerKV(m))
		} else {
			rec.Msg = text
		}
	case FormatLogfmt:
		rec.Msg = text
		if kv := parseLogfmt(text); kv != nil {
			rec.User = anyFromStrings(kv)
			applyCommon(rec, stringKV(kv)) // 有 msg/level/host/app/time 则覆盖上面的兜底值
		}
	case FormatCLF:
		if fields, ts, ok := parseCLF(text); ok {
			rec.User = fields
			if !ts.IsZero() {
				rec.EventTime = ts.UTC()
				rec.EventTimeRaw = ts.Format(clfTimeLayout)
			}
			// HTTP 状态码 → 级别（FR-202 的 Web 访问日志惯例）
			if s, ok2 := fields["status"].(string); ok2 {
				if n, err := strconv.Atoi(s); err == nil {
					rec.Level = httpStatusLevel(n)
					if sev, ok3 := model.SeverityFromText(rec.Level); ok3 {
						rec.SeverityNumber = &sev
					}
				}
			}
		}
		rec.Msg = text
	case FormatRFC5424, FormatRFC3164:
		// 文件里也可以出现 syslog 文本（如 /var/log/messages）→ 复用 syslog 解析
		if m, err := syslog.Parse(nz.UTF8); err == nil {
			rec.PRI = ptr(m.PRI)
			rec.Facility = ptr(m.Facility)
			rec.Severity = ptr(m.Severity)
			rec.SeverityNumber = ptr(m.Severity)
			rec.Host = m.Hostname
			rec.App = m.AppName
			rec.PID = ptrAtoi(m.ProcID)
			rec.Msg = m.Msg
			rec.Level = LevelName(m.Severity)
			rec.EventTimeRaw = m.TimestampRaw
			if !m.Timestamp.IsZero() {
				if m.HasYear {
					rec.EventTime = m.Timestamp.UTC()
				} else {
					rec.EventTime = model.InferYear(m.Timestamp, ingest.UTC()).UTC()
					rec.TimeInferred = true
				}
			}
			if len(m.StructuredData) > 0 {
				rec.User = map[string]any{}
				for _, params := range m.StructuredData {
					for k, v := range params {
						rec.User[k] = v
					}
				}
			}
		}
	default:
		rec.Msg = text
	}

	if rec.Msg == "" {
		rec.Msg = text // 任何分支都不允许空消息体（原文另在 Raw）
	}
	return p.finalize(rec)
}

// httpStatusLevel 把 HTTP 状态码映射为级别文本（访问日志场景）。
func httpStatusLevel(status int) string {
	switch {
	case status >= 500:
		return "err"
	case status >= 400:
		return "warning"
	default:
		return "info"
	}
}

// finalize 做时间闸门与分类（两条采集路径共用）。
func (p *Processor) finalize(rec *model.Record) *model.Record {
	// 时间闸门（D-019）：不可信时间回退为 ingest_time
	rec.ApplyTimeGate(p.cfg.TimeGate)

	// 分类（D-020 的 class_primary + 保留档案）
	rec.ClassPrimary, rec.IsAudit = classify(rec)
	if rec.IsAudit {
		rec.Classes = append(rec.Classes, rec.ClassPrimary, "audit")
		rec.RetentionProfile = p.cfg.AuditRetention
	} else {
		rec.Classes = append(rec.Classes, rec.ClassPrimary)
		rec.RetentionProfile = p.cfg.DefaultRetention
	}
	return rec
}

// kindOf 把传输协议映射为 source.kind。
func kindOf(protocol string) model.SourceKind {
	switch protocol {
	case "udp", "tcp", "tls":
		return model.SrcRemoteSyslog
	case "snmp_trap":
		return model.SrcSNMPTrap
	case "http":
		return model.SrcRemoteHTTP
	default:
		return model.SrcRemoteSyslog
	}
}

// auditApps 是需要按审计口径留存的程序。
var auditApps = map[string]bool{
	"sshd": true, "sudo": true, "su": true, "login": true,
	"passwd": true, "useradd": true, "usermod": true, "userdel": true,
	"groupadd": true, "gpasswd": true, "auditd": true, "systemd-logind": true,
}

// classify 给出主分类与是否审计类。
func classify(r *model.Record) (string, bool) {
	app := strings.ToLower(r.App)
	if auditApps[app] {
		return "auth", true
	}
	switch {
	case app == "kernel" || app == "audit":
		return "system", app == "audit"
	case strings.Contains(app, "ssh") || strings.Contains(app, "sudo"):
		return "auth", true
	}
	// PRI facility 4=auth / 10=authpriv → 审计类
	if r.Facility != nil && (*r.Facility == 4 || *r.Facility == 10) {
		return "auth", true
	}
	if r.Facility != nil && *r.Facility == 0 {
		return "system", false
	}
	return "system", false
}

// LevelName 把 syslog severity 映射为级别文本（D-006 的原始 level 文本）。
func LevelName(sev int) string {
	switch sev {
	case 0:
		return "emerg"
	case 1:
		return "alert"
	case 2:
		return "crit"
	case 3:
		return "err"
	case 4:
		return "warning"
	case 5:
		return "notice"
	case 6:
		return "info"
	case 7:
		return "debug"
	default:
		return ""
	}
}

func ptr(v int) *int { return &v }

func ptrAtoi(s string) *int {
	if s == "" {
		return nil
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return nil
		}
		n = n*10 + int(s[i]-'0')
	}
	return &n
}
