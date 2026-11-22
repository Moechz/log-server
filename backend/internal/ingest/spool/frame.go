package spool

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

// frameVersion 是队列帧格式版本。升级格式时递增，旧帧由 decodeFrame 明确拒绝
// （而不是静默解出半条记录）。队列里的帧是**我们自己**的格式，与 API 无关。
const frameVersion = 1

// frame 是落盘到 queue 的一条记录（带版本号，便于以后演进）。
type frame struct {
	V int       `json:"v"`
	R recordDTO `json:"r"`
}

// sourceDTO 与 model.Source 一一对应（Record 本身没有 JSON tag，不能直接序列化，
// 否则字段名会随重构漂移，升级后旧队列将无法解码）。
type sourceDTO struct {
	Kind      string `json:"kind,omitempty"`
	IP        string `json:"ip,omitempty"`
	Port      int    `json:"port,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	TLSCN     string `json:"tls_cn,omitempty"`
	Device    string `json:"device,omitempty"`
	Path      string `json:"path,omitempty"`
	Collector string `json:"collector,omitempty"`
}

// recordDTO 是 model.Record 的显式线格式（字段全覆盖）。
type recordDTO struct {
	Raw              string         `json:"raw"`
	IngestTime       time.Time      `json:"ingest_time"`
	Source           sourceDTO      `json:"source"`
	PRI              *int           `json:"pri,omitempty"`
	Facility         *int           `json:"facility,omitempty"`
	Severity         *int           `json:"severity,omitempty"`
	Encoding         string         `json:"encoding,omitempty"`
	RetentionProfile string         `json:"retention_profile,omitempty"`
	Dup              bool           `json:"dup,omitempty"`
	Seq              uint64         `json:"seq,omitempty"`
	EventTime        time.Time      `json:"event_time"`
	TimeTrusted      bool           `json:"time_trusted,omitempty"`
	TimeInferred     bool           `json:"time_inferred,omitempty"`
	EventTimeRaw     string         `json:"event_time_raw,omitempty"`
	Host             string         `json:"host,omitempty"`
	App              string         `json:"app,omitempty"`
	PID              *int           `json:"pid,omitempty"`
	Level            string         `json:"level,omitempty"`
	SeverityNumber   *int           `json:"severity_number,omitempty"`
	Format           string         `json:"format,omitempty"`
	ClassPrimary     string         `json:"class_primary,omitempty"`
	Classes          []string       `json:"classes,omitempty"`
	IsAudit          bool           `json:"is_audit,omitempty"`
	Tags             []string       `json:"tags,omitempty"`
	Msg              string         `json:"msg,omitempty"`
	ParseError       string         `json:"parse_error,omitempty"`
	User             map[string]any `json:"user,omitempty"`
}

func toDTO(r *model.Record) recordDTO {
	return recordDTO{
		Raw:        r.Raw,
		IngestTime: r.IngestTime,
		Source: sourceDTO{
			Kind: string(r.Source.Kind), IP: r.Source.IP, Port: r.Source.Port,
			Protocol: r.Source.Protocol, TLSCN: r.Source.TLSCN,
			Device: r.Source.Device, Path: r.Source.Path, Collector: r.Source.Collector,
		},
		PRI: r.PRI, Facility: r.Facility, Severity: r.Severity,
		Encoding: r.Encoding, RetentionProfile: r.RetentionProfile,
		Dup: r.Dup, Seq: r.Seq,
		EventTime: r.EventTime, TimeTrusted: r.TimeTrusted, TimeInferred: r.TimeInferred,
		EventTimeRaw: r.EventTimeRaw, Host: r.Host, App: r.App, PID: r.PID,
		Level: r.Level, SeverityNumber: r.SeverityNumber, Format: r.Format,
		ClassPrimary: r.ClassPrimary, Classes: r.Classes, IsAudit: r.IsAudit,
		Tags: r.Tags, Msg: r.Msg, ParseError: r.ParseError, User: r.User,
	}
}

func fromDTO(d recordDTO) *model.Record {
	return &model.Record{
		Raw:        d.Raw,
		IngestTime: d.IngestTime,
		Source: model.Source{
			Kind: model.SourceKind(d.Source.Kind), IP: d.Source.IP, Port: d.Source.Port,
			Protocol: d.Source.Protocol, TLSCN: d.Source.TLSCN,
			Device: d.Source.Device, Path: d.Source.Path, Collector: d.Source.Collector,
		},
		PRI: d.PRI, Facility: d.Facility, Severity: d.Severity,
		Encoding: d.Encoding, RetentionProfile: d.RetentionProfile,
		Dup: d.Dup, Seq: d.Seq,
		EventTime: d.EventTime, TimeTrusted: d.TimeTrusted, TimeInferred: d.TimeInferred,
		EventTimeRaw: d.EventTimeRaw, Host: d.Host, App: d.App, PID: d.PID,
		Level: d.Level, SeverityNumber: d.SeverityNumber, Format: d.Format,
		ClassPrimary: d.ClassPrimary, Classes: d.Classes, IsAudit: d.IsAudit,
		Tags: d.Tags, Msg: d.Msg, ParseError: d.ParseError, User: d.User,
	}
}

func encodeFrame(r *model.Record) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("spool: 记录为空")
	}
	return json.Marshal(frame{V: frameVersion, R: toDTO(r)})
}

func decodeFrame(b []byte) (*model.Record, error) {
	var f frame
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("spool: 帧解码失败: %w", err)
	}
	if f.V != frameVersion {
		return nil, fmt.Errorf("spool: 不支持的帧版本 %d（期望 %d）", f.V, frameVersion)
	}
	return fromDTO(f.R), nil
}
