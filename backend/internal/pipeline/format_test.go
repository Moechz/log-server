package pipeline

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/encoding"
	"github.com/Moechz/log-server/backend/internal/model"
)

func TestDetectFormat_AllFormats(t *testing.T) {
	cases := []struct {
		line string
		want Format
	}{
		{`<34>1 2026-10-12T10:00:00Z nas app 1 - - hello`, FormatRFC5424},
		{`<34>Oct 12 10:00:00 nas app: hello`, FormatRFC3164},
		{`{"time":"2026-10-12T10:00:00Z","level":"error","msg":"boom"}`, FormatJSON},
		{`time=2026-10-12T10:00:00Z level=info msg="hello world" svc=api`, FormatLogfmt},
		{`127.0.0.1 - frank [10/Oct/2000:13:55:36 -0700] "GET /a.gif HTTP/1.0" 200 2326 "-" "Mozilla/4.08"`, FormatCLF},
		{`plain text line without any structure`, FormatPlain},
		{``, FormatPlain},
	}
	for _, c := range cases {
		if got := DetectFormat(c.line); got != c.want {
			t.Errorf("DetectFormat(%q) = %q，期望 %q", c.line, got, c.want)
		}
	}
}

// TC-08：采集 nginx access log（CLF）→ 字段结构化解出，原文可查。
func TestProcessFile_CLF_FieldsAndRaw(t *testing.T) {
	p := New(DefaultConfig())
	line := `192.168.1.9 - - [12/Oct/2026:10:00:00 +0800] "POST /api/v1/logs HTTP/1.1" 500 1234 "http://nas/" "curl/8.0"`
	ingest := time.Date(2026, 10, 12, 10, 0, 1, 0, time.UTC)
	r := p.ProcessFile([]byte(line), "/var/log/nginx/access.log", ingest, "")

	if r.Format != string(FormatCLF) {
		t.Fatalf("格式应为 clf，得到 %q", r.Format)
	}
	if r.Raw != line {
		t.Error("原文必须逐字节保留（FR-206）")
	}
	if r.Source.Kind != model.SrcLocalFile || r.Source.Path != "/var/log/nginx/access.log" {
		t.Errorf("来源信封错误: %+v", r.Source)
	}
	want := map[string]any{
		"remote_host": "192.168.1.9", "request_method": "POST",
		"request_uri": "/api/v1/logs", "protocol": "HTTP/1.1",
		"status": "500", "bytes": 1234,
	}
	for k, v := range want {
		if r.User[k] != v {
			t.Errorf("用户字段 %s = %v，期望 %v", k, r.User[k], v)
		}
	}
	if r.Level != "err" {
		t.Errorf("500 应映射为 err，得到 %q", r.Level)
	}
	if r.SeverityNumber == nil || *r.SeverityNumber != 3 {
		t.Error("500 的 severity_number 应为 3")
	}
	if !r.EventTime.Equal(time.Date(2026, 10, 12, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("CLF 时间未按 UTC 归一: %v", r.EventTime)
	}
	if r.EventTimeRaw == "" {
		t.Error("必须保留原始时间文本")
	}
}

func TestProcessFile_JSON(t *testing.T) {
	p := New(DefaultConfig())
	line := `{"time":"2026-10-12T10:00:00Z","level":"WARN","msg":"disk almost full","host":"nas","service":"storage","used_pct":93}`
	r := p.ProcessFile([]byte(line), "/volume1/app/app.log", time.Now().UTC(), "")

	if r.Format != string(FormatJSON) {
		t.Fatalf("格式应为 json，得到 %q", r.Format)
	}
	if r.Msg != "disk almost full" || r.Host != "nas" || r.App != "storage" {
		t.Errorf("派生字段提取失败: msg=%q host=%q app=%q", r.Msg, r.Host, r.App)
	}
	if r.Level != "WARN" || r.SeverityNumber == nil || *r.SeverityNumber != 4 {
		t.Errorf("级别双字段错误: level=%q sev=%v", r.Level, r.SeverityNumber)
	}
	// 用户字段原样保留（不做前缀改写/丢弃）
	if v, ok := r.User["used_pct"].(float64); !ok || v != 93 {
		t.Errorf("用户字段 used_pct 应原样保留，得到 %v", r.User["used_pct"])
	}
	// 落库序列化：_msg 是原文，ls_format 标出格式
	log := r.VLLog()
	if log["_msg"] != line {
		t.Error("_msg 必须是原文")
	}
	if log["ls_format"] != "json" || log["ls_msg"] != "disk almost full" {
		t.Errorf("落库字段错误: %v / %v", log["ls_format"], log["ls_msg"])
	}
}

func TestProcessFile_Logfmt(t *testing.T) {
	p := New(DefaultConfig())
	line := `time=2026-10-12T10:00:00Z level=error msg="connection refused" service=api retries=3`
	r := p.ProcessFile([]byte(line), "/tmp/app.log", time.Now().UTC(), "")

	if r.Format != string(FormatLogfmt) {
		t.Fatalf("格式应为 logfmt，得到 %q", r.Format)
	}
	if r.Msg != "connection refused" || r.App != "api" || r.Level != "error" {
		t.Errorf("logfmt 提取失败: msg=%q app=%q level=%q", r.Msg, r.App, r.Level)
	}
	if r.User["retries"] != "3" {
		t.Errorf("用户字段应保留原始键名，得到 %v", r.User)
	}
}

func TestProcessFile_PlainAndSyslog(t *testing.T) {
	p := New(DefaultConfig())
	plain := `just a line with no structure at all`
	r := p.ProcessFile([]byte(plain), "/tmp/x.log", time.Now().UTC(), "")
	if r.Format != string(FormatPlain) || r.Msg != plain || r.Raw != plain {
		t.Errorf("纯文本应原样保留: format=%q msg=%q", r.Format, r.Msg)
	}
	// 文件里出现 syslog 行（/var/log/messages 场景）也应被解析
	sys := `<34>Oct 12 10:00:00 nas sshd[42]: Failed password for root`
	r2 := p.ProcessFile([]byte(sys), "/var/log/messages", time.Now().UTC(), "")
	if r2.Format != string(FormatRFC3164) || r2.App != "sshd" || !r2.IsAudit {
		t.Errorf("文件内 syslog 行解析失败: format=%q app=%q audit=%v", r2.Format, r2.App, r2.IsAudit)
	}
}

// TC-03：GBK 中文日志 → 转 UTF-8 后可正常检索；ls_encoding 记录原编码。
func TestProcessFile_GBKEncoding(t *testing.T) {
	if !encoding.ConverterAvailable() {
		t.Skip("宿主无 iconv：GB18030 走原文保留路径（原文仍在 _msg 中，不丢数据）")
	}
	p := New(DefaultConfig())
	// "磁盘已满" 的 GBK 编码
	line := append([]byte("2026-10-12 10:00:00 [error] "), []byte{
		0xB4, 0xC5, 0xC5, 0xCC, 0xD2, 0xD1, 0xC2, 0xFA,
	}...)
	r := p.ProcessFile(line, "/tmp/gbk.log", time.Now().UTC(), "")

	if r.Encoding != "gb18030" {
		t.Errorf("应识别为 gb18030，得到 %q", r.Encoding)
	}
	if r.Msg != "2026-10-12 10:00:00 [error] 磁盘已满" {
		t.Errorf("GBK 应转为可检索的 UTF-8 中文，得到 %q", r.Msg)
	}
	if r.Raw != string(line) {
		t.Error("原文必须保留原始 GBK 字节（不可改写）")
	}
}

func TestProcessFile_EncodingHint(t *testing.T) {
	if !encoding.ConverterAvailable() {
		t.Skip("宿主无 iconv")
	}
	p := New(DefaultConfig())
	gbk := []byte{0xC4, 0xE3, 0xBA, 0xC3} // "你好"
	r := p.ProcessFile(gbk, "/tmp/gbk.log", time.Now().UTC(), "GBK")
	if r.Encoding != "gbk" || r.Msg != "你好" {
		t.Errorf("显式编码声明未生效: enc=%q msg=%q", r.Encoding, r.Msg)
	}
}

func TestParseCLF_RejectsNonCLF(t *testing.T) {
	for _, s := range []string{
		`{"json":"not clf"}`,
		`Oct 12 10:00:00 nas app: hi`,
		`127.0.0.1 - - [oops] "GET / HTTP/1.1" 20x 1`,
		``,
	} {
		if _, _, ok := parseCLF(s); ok {
			t.Errorf("不应把 %q 判为 CLF", s)
		}
	}
}

func TestLowerKV_KeepsNumbersAsText(t *testing.T) {
	raw := []byte(`{"n":1.5,"b":true,"s":"x","nil":null,"obj":{"a":1}}`)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	kv := lowerKV(m)
	if kv["n"] != "1.5" || kv["b"] != "true" || kv["s"] != "x" {
		t.Errorf("展平结果错误: %v", kv)
	}
	if _, ok := kv["nil"]; ok {
		t.Error("null 不应产生键值")
	}
	if _, ok := kv["obj"]; !ok {
		t.Error("嵌套对象应被展平为文本，而不是丢弃")
	}
}
