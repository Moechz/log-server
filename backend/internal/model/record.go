// Package model 定义日志记录模型（三段式，D-005）与落库序列化（D-020）、时间闸门（D-019）。
//
//	① 原文 raw —— 不可变，落库为 VictoriaLogs 的 _msg
//	② 信封    —— 采集时确定，落库为 ls_* 前缀字段
//	③ 派生    —— 可由原文 + 规则重建，落库为 ls_* 前缀字段
//
// 用户原始结构化字段**不加前缀、原样保留**，但 _time/_msg/_stream 只允许我们的代码写。
package model

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// SourceKind 采集来源种类（D-005 硬规则 7）。
type SourceKind string

const (
	SrcLocalFile    SourceKind = "local_file"
	SrcJournald     SourceKind = "journald"
	SrcContainer    SourceKind = "container"
	SrcAuditd       SourceKind = "auditd"
	SrcRemoteSyslog SourceKind = "remote_syslog"
	SrcRemoteHTTP   SourceKind = "remote_http"
	SrcSNMPTrap     SourceKind = "snmptrap"
	SrcSelf         SourceKind = "self"
	SrcPlatform     SourceKind = "platform"
)

// Source 是信封中的来源信息（无法从原文复原，必须显式记录）。
type Source struct {
	Kind      SourceKind
	IP        string
	Port      int
	Protocol  string
	TLSCN     string
	Device    string
	Path      string
	Collector string
}

// Record 是一条日志记录。
type Record struct {
	// ① 原文（不可变）
	Raw string

	// ② 信封
	IngestTime       time.Time
	Source           Source
	PRI              *int
	Facility         *int
	Severity         *int
	Encoding         string
	RetentionProfile string
	Dup              bool
	Seq              uint64

	// ③ 派生
	EventTime      time.Time
	TimeTrusted    bool
	TimeInferred   bool
	EventTimeRaw   string
	Host           string
	App            string
	PID            *int
	Level          string
	SeverityNumber *int
	Format         string
	ClassPrimary   string
	Classes        []string
	IsAudit        bool
	Tags           []string
	Msg            string
	ParseError     string

	// ④ 用户原始结构化字段（原样保留，不加前缀）
	User map[string]any
}

// FieldPrefix 是我们自己的字段前缀（D-020）。
const FieldPrefix = "ls_"

// TimeGateThreshold 是时间闸门阈值（D-019）；超过即认为 event_time 不可信。
const TimeGateThreshold = 24 * time.Hour

// ApplyTimeGate 执行时间闸门（D-019）。
//
// 返回落库用的 _time 与是否可信：
//   - |event - ingest| <= threshold  → 用 event，可信
//   - 否则                            → 用 ingest，不可信（避免设备时钟漂移导致数据落旧分区被立即删除，
//     也避免时钟超前导致永不淘汰的存储泄漏）
//
// 同时把原始解析值留在 EventTimeRaw 供检索（原文时间不丢）。
func (r *Record) ApplyTimeGate(threshold time.Duration) {
	if r.EventTime.IsZero() {
		r.TimeTrusted = true // 无声明时间：视为由 ingest_time 决定，不算「不可信」
		r.EventTime = r.IngestTime
		return
	}
	if threshold <= 0 {
		threshold = TimeGateThreshold
	}
	delta := r.EventTime.Sub(r.IngestTime)
	if delta < 0 {
		delta = -delta
	}
	if delta <= threshold {
		r.TimeTrusted = true
		return
	}
	r.TimeTrusted = false
	if r.EventTimeRaw == "" {
		r.EventTimeRaw = r.EventTime.Format(time.RFC3339Nano)
	}
	r.EventTime = r.IngestTime
}

// InferYear 处理 RFC3164 无年份的情况：取 ingest 年；若推断结果比 ingest 晚超过
// 2 天则年份 −1（跨年边界），并标记为「推断」。
func InferYear(monthDayTime time.Time, ingest time.Time) time.Time {
	t := time.Date(ingest.Year(), monthDayTime.Month(), monthDayTime.Day(),
		monthDayTime.Hour(), monthDayTime.Minute(), monthDayTime.Second(),
		monthDayTime.Nanosecond(), ingest.Location())
	if t.Sub(ingest) > 48*time.Hour {
		t = t.AddDate(-1, 0, 0)
	}
	return t
}

// ── 落库序列化（D-020）─────────────────────────────────────────────────

// reservedVLFields 是 VictoriaLogs 的保留字段，只允许我们的代码写。
var reservedVLFields = map[string]bool{
	"_time": true, "_msg": true, "_stream": true,
	"_stream_id": true,
}

// SanitizeUser 处理用户提供的结构化字段：
//  1. 剥离保留字段（_time/_msg/_stream 等）—— 防 _stream 基数爆炸与伪造（D-020 硬约束 1）
//  2. 用户字段若带我们的前缀 ls_ → 改写为 user_ 前缀（D-020 硬约束 2）
//
// 返回处理后的字段与两个计数（供告警/诊断使用）。
func SanitizeUser(in map[string]any) (out map[string]any, strippedReserved, renamedPrefix int) {
	if len(in) == 0 {
		return nil, 0, 0
	}
	out = make(map[string]any, len(in))
	for k, v := range in {
		if reservedVLFields[k] {
			strippedReserved++
			continue
		}
		if strings.HasPrefix(k, FieldPrefix) {
			out["user_"+k] = v
			renamedPrefix++
			continue
		}
		out[k] = v
	}
	return out, strippedReserved, renamedPrefix
}

// VLLog 把记录序列化为 VictoriaLogs 的写入形态（jsonline）。
//
// 输出字段：_time、_msg、以及全部 ls_* 前缀的我们的字段；用户字段原样并列。
func (r *Record) VLLog() map[string]any {
	out := make(map[string]any, 32+len(r.User))

	// ① 原文 → _msg。
	// JSON 字符串**无法承载非法 UTF-8**（Go 的 encoding/json 会把非法字节替换成 U+FFFD），
	// 所以：合法 UTF-8 → _msg 逐字节原文；非 UTF-8 → _msg 放 UTF-8 视图，
	// 原始字节另存 ls_raw_b64（base64）+ ls_raw_bytes + ls_raw_encoding。
	// 这样检索/展示可读，取证时又能从 base64 还原原始字节（D-031）。
	if utf8.ValidString(r.Raw) {
		out["_msg"] = r.Raw
	} else {
		msg := r.Msg
		if msg == "" {
			msg = strings.ToValidUTF8(r.Raw, "\uFFFD")
		}
		out["_msg"] = msg
		out["ls_raw_b64"] = base64.StdEncoding.EncodeToString([]byte(r.Raw))
		out["ls_raw_bytes"] = len(r.Raw)
		enc := r.Encoding
		if enc == "" {
			enc = "unknown"
		}
		out["ls_raw_encoding"] = enc
	}
	// 时间 → _time
	out["_time"] = r.EventTime.UTC().Format(time.RFC3339Nano)

	// ② 信封 → ls_*
	out["ls_ingest_time"] = r.IngestTime.UTC().Format(time.RFC3339Nano)
	out["ls_source_kind"] = string(r.Source.Kind)
	if r.Source.IP != "" {
		out["ls_source_ip"] = r.Source.IP
	}
	if r.Source.Port != 0 {
		out["ls_source_port"] = r.Source.Port
	}
	if r.Source.Protocol != "" {
		out["ls_protocol"] = r.Source.Protocol
	}
	if r.Source.TLSCN != "" {
		out["ls_tls_cn"] = r.Source.TLSCN
	}
	if r.Source.Device != "" {
		out["ls_device"] = r.Source.Device
	}
	if r.Source.Path != "" {
		out["ls_source_path"] = r.Source.Path
	}
	if r.Source.Collector != "" {
		out["ls_collector"] = r.Source.Collector
	}
	if r.PRI != nil {
		out["ls_pri"] = *r.PRI
	}
	if r.Facility != nil {
		out["ls_facility"] = *r.Facility
	}
	if r.Severity != nil {
		out["ls_severity"] = *r.Severity
	}
	if r.Encoding != "" {
		out["ls_encoding"] = r.Encoding
	}
	if r.RetentionProfile != "" {
		out["ls_retention_profile"] = r.RetentionProfile
	}
	if r.Dup {
		out["ls_dup"] = true
	}
	if r.Seq != 0 {
		out["ls_seq"] = r.Seq
	}

	// ③ 派生 → ls_*
	if r.Host != "" {
		out["ls_host"] = r.Host
	}
	if r.App != "" {
		out["ls_app"] = r.App
	}
	if r.PID != nil {
		out["ls_pid"] = *r.PID
	}
	if r.Level != "" {
		out["ls_level"] = r.Level
	}
	if r.SeverityNumber != nil {
		out["ls_severity_number"] = *r.SeverityNumber
	}
	if r.Format != "" {
		out["ls_format"] = r.Format
	}
	if r.ClassPrimary != "" {
		out["ls_class_primary"] = r.ClassPrimary
	}
	if len(r.Classes) > 0 {
		out["ls_classes"] = strings.Join(r.Classes, ",")
	}
	out["ls_is_audit"] = r.IsAudit
	if len(r.Tags) > 0 {
		out["ls_tags"] = strings.Join(r.Tags, ",")
	}
	if r.Msg != "" {
		out["ls_msg"] = r.Msg
	}
	if r.ParseError != "" {
		out["ls_parse_error"] = r.ParseError
	}
	out["ls_time_trusted"] = r.TimeTrusted
	if r.TimeInferred {
		out["ls_time_inferred"] = true
	}
	if r.EventTimeRaw != "" {
		out["ls_event_time_raw"] = r.EventTimeRaw
	}

	// ④ 用户字段（已清洗）
	if u, _, _ := SanitizeUser(r.User); len(u) > 0 {
		for k, v := range u {
			out[k] = v
		}
	}
	return out
}

// Validate 做最基本的入库前校验。
func (r *Record) Validate() error {
	if r.Raw == "" && r.Msg == "" {
		return fmt.Errorf("记录既无原文也无消息")
	}
	if r.IngestTime.IsZero() {
		return fmt.Errorf("缺少 ingest_time")
	}
	return nil
}
