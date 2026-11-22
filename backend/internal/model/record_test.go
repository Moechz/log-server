package model

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
	"unicode/utf8"
)

func TestApplyTimeGate_Trusted(t *testing.T) {
	ing := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	r := &Record{IngestTime: ing, EventTime: ing.Add(-2 * time.Hour)}
	r.ApplyTimeGate(TimeGateThreshold)
	if !r.TimeTrusted {
		t.Fatal("2 小时偏差应判定为可信")
	}
	if !r.EventTime.Equal(ing.Add(-2 * time.Hour)) {
		t.Fatalf("可信时 _time 应保持 event_time，得到 %v", r.EventTime)
	}
}

func TestApplyTimeGate_DriftToPast(t *testing.T) {
	ing := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	evt := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) // 设备时钟漂到 2020
	r := &Record{IngestTime: ing, EventTime: evt}
	r.ApplyTimeGate(TimeGateThreshold)

	if r.TimeTrusted {
		t.Fatal("6 年偏差必须判定为不可信")
	}
	if !r.EventTime.Equal(ing) {
		t.Fatalf("不可信时 _time 必须回退为 ingest_time，得到 %v", r.EventTime)
	}
	if r.EventTimeRaw == "" {
		t.Fatal("必须保留原始解析时间 ls_event_time_raw")
	}
}

func TestApplyTimeGate_FutureClock(t *testing.T) {
	ing := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	r := &Record{IngestTime: ing, EventTime: ing.Add(30 * 24 * time.Hour)}
	r.ApplyTimeGate(TimeGateThreshold)
	if r.TimeTrusted || !r.EventTime.Equal(ing) {
		t.Fatal("未来时间必须回退，否则数据永不淘汰（存储泄漏）")
	}
}

func TestInferYear_CrossYear(t *testing.T) {
	ing := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	// 日志写的是 12 月 31 日（去年）
	parsed := time.Date(0, 12, 31, 23, 59, 59, 0, time.UTC)
	got := InferYear(parsed, ing)
	if got.Year() != 2025 {
		t.Fatalf("跨年推断应为 2025，得到 %d", got.Year())
	}
}

func TestSanitizeUser(t *testing.T) {
	in := map[string]any{
		"host":     "user-host",            // 用户自己的 host —— 原样保留
		"trace_id": "abc",                  // 普通字段
		"_stream":  "evil",                 // 必须剥离
		"_time":    "1970-01-01T00:00:00Z", // 必须剥离
		"ls_host":  "spoofed",              // 必须改写为 user_ls_host
	}
	out, stripped, renamed := SanitizeUser(in)
	if stripped != 2 {
		t.Fatalf("应剥离 2 个保留字段，实得 %d", stripped)
	}
	if renamed != 1 {
		t.Fatalf("应改写 1 个 ls_ 前缀字段，实得 %d", renamed)
	}
	if out["host"] != "user-host" {
		t.Fatal("用户字段必须原样保留")
	}
	if _, ok := out["_stream"]; ok {
		t.Fatal("_stream 必须被剥离（防基数爆炸）")
	}
	if out["user_ls_host"] != "spoofed" {
		t.Fatal("用户带 ls_ 前缀的字段必须改写为 user_ 前缀")
	}
}

func TestVLLog_PrefixesAndRaw(t *testing.T) {
	ing := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	pri, sev, pid, sevnum := 134, 6, 1234, 6
	r := &Record{
		Raw:        "<134>Oct 10 12:34:56 web01 sshd[1234]: Failed password for root",
		IngestTime: ing, EventTime: ing,
		Source: Source{Kind: SrcRemoteSyslog, IP: "10.0.0.5", Port: 51234, Protocol: "udp"},
		PRI:    &pri, Severity: &sev, Encoding: "utf-8", RetentionProfile: "security-180d",
		Host: "web01", App: "sshd", PID: &pid, SeverityNumber: &sevnum,
		ClassPrimary: "auth", Classes: []string{"auth", "security", "audit"}, IsAudit: true,
		Msg:  "Failed password for root",
		User: map[string]any{"host": "user-said", "_stream": "evil"},
	}
	r.ApplyTimeGate(TimeGateThreshold)
	log := r.VLLog()

	if log["_msg"] != r.Raw {
		t.Fatal("_msg 必须是逐字节原文")
	}
	if log["ls_host"] != "web01" {
		t.Fatal("派生字段必须带 ls_ 前缀")
	}
	if log["host"] != "user-said" {
		t.Fatal("用户字段必须原样并列，且不与我们的字段冲突")
	}
	if _, ok := log["_stream"]; ok {
		t.Fatal("用户注入的 _stream 绝不能落库")
	}
	if log["ls_is_audit"] != true {
		t.Fatal("is_audit 应为 true")
	}
	if log["ls_time_trusted"] != true {
		t.Fatal("时间可信标记缺失")
	}
}

// TestVLLog_NonUTF8RawPreserved：非 UTF-8 原文必须能还原（D-031 / L8）。
// JSON 字符串承载不了非法 UTF-8，所以 _msg 放 UTF-8 视图，原始字节进 ls_raw_b64。
func TestVLLog_NonUTF8RawPreserved(t *testing.T) {
	raw := string([]byte{0xC4, 0xE3, 0xBA, 0xC3}) // GBK "你好"
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	r := &Record{
		Raw: raw, Msg: "你好", IngestTime: now, EventTime: now,
		Encoding: "gbk", Source: Source{Kind: SrcLocalFile},
	}
	log := r.VLLog()

	msg, _ := log["_msg"].(string)
	if !utf8.ValidString(msg) || msg != "你好" {
		t.Fatalf("非 UTF-8 的 _msg 应为可读的 UTF-8 视图，得到 %q", msg)
	}
	b64, _ := log["ls_raw_b64"].(string)
	back, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("ls_raw_b64 不是合法 base64: %v", err)
	}
	if string(back) != raw {
		t.Fatalf("原始字节不可还原：got % x want % x", []byte(back), []byte(raw))
	}
	if got := log["ls_raw_bytes"]; got != len(raw) {
		t.Fatalf("ls_raw_bytes 应为 %d，得到 %v", len(raw), got)
	}
	if log["ls_raw_encoding"] != "gbk" {
		t.Fatalf("ls_raw_encoding 应为 gbk，得到 %v", log["ls_raw_encoding"])
	}

	// 再走一次 JSON（实际写 VL 的路径）：_msg 合法、b64 仍可还原。
	body, err := json.Marshal(log)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(m["_msg"].(string)) {
		t.Fatal("JSON 序列化后 _msg 仍非法")
	}
	back2, _ := base64.StdEncoding.DecodeString(m["ls_raw_b64"].(string))
	if string(back2) != raw {
		t.Fatal("JSON 序列化后原始字节不可还原")
	}

	// 合法 UTF-8 不额外写 b64（避免存储膨胀）。
	r2 := &Record{Raw: "hello", IngestTime: now, EventTime: now}
	if _, ok := r2.VLLog()["ls_raw_b64"]; ok {
		t.Fatal("合法 UTF-8 不应写 ls_raw_b64")
	}
}
