package pipeline

import (
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/receive"
)

func TestProcess_RFC3164_SSHDFailedLogin(t *testing.T) {
	p := New(DefaultConfig())
	ingest := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	raw := []byte(`<38>Oct 12 10:00:00 web01 sshd[1234]: Failed password for root from 10.0.0.5`)

	r := p.Process(raw, receive.SourceInfo{IP: "10.0.0.5", Port: 51234, Protocol: "udp"}, ingest)

	if r.Raw != string(raw) {
		t.Fatal("原文必须逐字节保留")
	}
	if r.SeverityNumber == nil || *r.SeverityNumber != 6 {
		t.Errorf("severity 归一化错误: %v", r.SeverityNumber)
	}
	if r.Level != "info" {
		t.Errorf("level 文本 = %q，期望 info（D-006 双字段）", r.Level)
	}
	if r.Host != "web01" || r.App != "sshd" || r.PID == nil || *r.PID != 1234 {
		t.Errorf("派生字段错误: host=%q app=%q pid=%v", r.Host, r.App, r.PID)
	}
	// sshd → 审计类，走审计保留档案
	if !r.IsAudit || r.ClassPrimary != "auth" {
		t.Errorf("sshd 应归为 auth/审计，得到 class=%q isAudit=%v", r.ClassPrimary, r.IsAudit)
	}
	if r.RetentionProfile != "security-180d" {
		t.Errorf("审计类保留档案应为 security-180d，得到 %q", r.RetentionProfile)
	}
	if len(r.Classes) != 2 || r.Classes[1] != "audit" {
		t.Errorf("classes 应为 [auth audit]，得到 %v", r.Classes)
	}
	// RFC3164 无年份 → 必须标记为推断
	if !r.TimeInferred {
		t.Error("RFC3164 无年份应标记 TimeInferred")
	}
	if r.EventTime.Year() != 2026 {
		t.Errorf("年份推断错误: %v", r.EventTime)
	}
}

func TestProcess_KernelIsSystemNotAudit(t *testing.T) {
	p := New(DefaultConfig())
	r := p.Process([]byte(`<12>Oct 12 10:00:00 nas kernel: Out of memory: Killed process 1`),
		receive.SourceInfo{IP: "10.0.0.1", Protocol: "udp"}, time.Now().UTC())

	if r.ClassPrimary != "system" {
		t.Errorf("kernel 应归 system，得到 %q", r.ClassPrimary)
	}
	if r.IsAudit {
		t.Error("内核日志不应默认归为审计类")
	}
	if r.RetentionProfile != "ops-14d" {
		t.Errorf("非审计类应为 ops-14d，得到 %q", r.RetentionProfile)
	}
}

func TestProcess_ParseFailureKeepsRaw(t *testing.T) {
	p := New(DefaultConfig())
	raw := []byte("garbage without PRI")
	r := p.Process(raw, receive.SourceInfo{Protocol: "udp"}, time.Now().UTC())

	if r.Raw != string(raw) {
		t.Fatal("解析失败也必须保留原文（FR-206）")
	}
	if r.ParseError == "" {
		t.Error("解析失败必须写 ParseError 而不是丢弃")
	}
	if r.Msg == "" {
		t.Error("解析失败时消息体应回退为原文，避免空记录")
	}
}

func TestProcess_FutureClockGatedByTimeGate(t *testing.T) {
	p := New(DefaultConfig())
	ingest := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	// 设备时钟超前 30 天 → 必须被闸门回退（否则数据永不淘汰，存储泄漏）
	raw := []byte(`<34>1 2026-11-11T10:00:00Z host app - - - future`)
	r := p.Process(raw, receive.SourceInfo{Protocol: "tcp"}, ingest)

	if r.TimeTrusted {
		t.Error("超前 30 天必须判为不可信")
	}
	if !r.EventTime.Equal(ingest) {
		t.Errorf("不可信时应回退为 ingest_time，得到 %v", r.EventTime)
	}
	if r.EventTimeRaw == "" {
		t.Error("必须保留原始解析时间（ls_event_time_raw）")
	}
}

func TestProcess_RFC5424_SDToUserFields(t *testing.T) {
	p := New(DefaultConfig())
	raw := []byte(`<165>1 2026-10-12T10:00:00Z host evntslog 1024 ID47 [ex@1 trace="abc"] hi`)
	// ★ 与消息同刻的固定 ingest：时间闸门（D-019）会在 |event−ingest| > 24h 时回退，
	//   若这里用 time.Now()，测试会随"今天几号"而红/绿（典型的时钟依赖型脆测试）。
	ingest := time.Date(2026, 10, 12, 10, 0, 1, 0, time.UTC)
	r := p.Process(raw, receive.SourceInfo{Protocol: "tcp"}, ingest)

	if r.User["trace"] != "abc" {
		t.Errorf("SD 字段应进用户字段（原样保留），得到 %+v", r.User)
	}
	if r.Msg != "hi" {
		t.Errorf("消息体应为 hi，得到 %q", r.Msg)
	}
	if r.TimeTrusted != true {
		t.Error("RFC5424 时间应可信")
	}
}

func TestLevelName_Mapping(t *testing.T) {
	want := map[int]string{
		0: "emerg", 1: "alert", 2: "crit", 3: "err",
		4: "warning", 5: "notice", 6: "info", 7: "debug",
	}
	for sev, name := range want {
		if got := LevelName(sev); got != name {
			t.Errorf("LevelName(%d) = %q，期望 %q", sev, got, name)
		}
	}
}
