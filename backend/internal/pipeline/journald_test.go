package pipeline

import (
	"strings"
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

// 真实 journalctl -o json 输出样例（systemd 会输出单行 JSON）。
// 注意时间戳与测试用 ingest 取同刻，避免触发时间闸门回退（D-019）。
// 1791948660000000 µs = 2026-10-14T03:31:00Z
const journalSSH = `{"__REALTIME_TIMESTAMP":"1791948660000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"sshd","_HOSTNAME":"nas","_PID":"1234","MESSAGE":"Failed password for root from 10.0.0.1 port 55 ssh2","_SYSTEMD_UNIT":"ssh.service","_TRANSPORT":"syslog","_BOOT_ID":"abc123"}`

func TestProcessJournal_MapsDerivedFields(t *testing.T) {
	p := New(DefaultConfig())
	ingest := time.Date(2026, 10, 14, 3, 31, 0, 0, time.UTC)
	r := p.ProcessJournal([]byte(journalSSH), ingest)

	if r.Format != FormatJournald {
		t.Fatalf("格式应为 journald，得到 %q", r.Format)
	}
	if r.Source.Kind != model.SrcJournald {
		t.Errorf("来源应为 journald，得到 %q", r.Source.Kind)
	}
	if r.Raw != journalSSH {
		t.Error("原文（journald JSON 行）必须逐字节保留")
	}
	if r.Msg != "Failed password for root from 10.0.0.1 port 55 ssh2" {
		t.Errorf("消息体应为 MESSAGE，得到 %q", r.Msg)
	}
	if r.App != "sshd" || r.Host != "nas" {
		t.Errorf("app/host 映射错误: %q / %q", r.App, r.Host)
	}
	if r.PID == nil || *r.PID != 1234 {
		t.Errorf("pid 映射错误: %v", r.PID)
	}
	if r.SeverityNumber == nil || *r.SeverityNumber != 3 || r.Level != "err" {
		t.Errorf("级别双字段错误: sev=%v level=%q", r.SeverityNumber, r.Level)
	}
	// __REALTIME_TIMESTAMP 为微秒；与 ingest 同刻 → 时间闸门不回退（D-019）
	if !r.EventTime.Equal(ingest) {
		t.Errorf("时间应取自 __REALTIME_TIMESTAMP（微秒），得到 %v 期望 %v", r.EventTime, ingest)
	}
	if !r.TimeTrusted {
		t.Error("同刻时间应判为可信")
	}
	if r.EventTimeRaw == "" {
		t.Error("必须保留原始时间文本（ls_event_time_raw）")
	}
	// 用户字段：journald 其余字段原样保留（不加前缀，未被丢弃）
	for _, k := range []string{"_SYSTEMD_UNIT", "_TRANSPORT", "_BOOT_ID", "PRIORITY"} {
		if _, ok := r.User[k]; !ok {
			t.Errorf("用户字段缺少 %s（journald 字段必须原样保留）", k)
		}
	}
	// 落库形态：_msg 是原文，ls_format=journald
	log := r.VLLog()
	if log["_msg"] != journalSSH {
		t.Error("_msg 必须是原始 JSON 行")
	}
	if log["ls_format"] != "journald" || log["ls_source_kind"] != "journald" {
		t.Errorf("落库字段错误: %v / %v", log["ls_format"], log["ls_source_kind"])
	}
}

func TestProcessJournal_ByteArrayMessage(t *testing.T) {
	p := New(DefaultConfig())
	// journald 用字节数组表示非 UTF-8 内容（此处 GBK 的 "你好"）
	line := `{"MESSAGE":[196,227,186,195],"PRIORITY":"6","SYSLOG_IDENTIFIER":"legacy"}`
	r := p.ProcessJournal([]byte(line), time.Now().UTC())
	if r.Msg == "" {
		t.Fatal("消息体不应为空")
	}
	// 允许两种正确形态：转码成功（iconv 可用）或 base64 保留（D-005）
	if r.Encoding == "base64" {
		if strings.TrimSpace(r.Msg) == "" {
			t.Error("base64 保留形态不应为空")
		}
	} else if r.Msg != "你好" {
		t.Errorf("GBK 字节数组应转码为中文或 base64 保留，得到 %q（enc=%s）", r.Msg, r.Encoding)
	}
}

func TestProcessJournal_InvalidJSONKeepsRaw(t *testing.T) {
	p := New(DefaultConfig())
	raw := "not a json line"
	r := p.ProcessJournal([]byte(raw), time.Now().UTC())
	if r.Raw != raw {
		t.Error("解析失败也必须保留原文（FR-206）")
	}
	if r.ParseError == "" {
		t.Error("解析失败必须写 parse_error")
	}
	if r.Msg == "" {
		t.Error("解析失败时消息体应回退为原文")
	}
}

func TestProcessJournal_MissingPriority(t *testing.T) {
	p := New(DefaultConfig())
	r := p.ProcessJournal([]byte(`{"MESSAGE":"hi","SYSLOG_IDENTIFIER":"x"}`), time.Now().UTC())
	if r.SeverityNumber != nil {
		t.Error("没有 PRIORITY 时 severity_number 应为空（不猜）")
	}
	if r.Msg != "hi" {
		t.Errorf("消息体错误: %q", r.Msg)
	}
}
