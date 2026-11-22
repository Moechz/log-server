package syslog

import (
	"strings"
	"testing"
	"time"
)

func TestParseRFC3164_Basic(t *testing.T) {
	raw := `<34>Oct 11 22:14:15 mymachine su[230]: 'su root' failed for lonvick on /dev/pts/8`
	m, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if m.Raw != raw {
		t.Fatal("原文必须逐字节保留")
	}
	if m.Version != 0 {
		t.Errorf("应识别为 RFC3164，得到 version=%d", m.Version)
	}
	if m.PRI != 34 || m.Facility != 4 || m.Severity != 2 {
		t.Errorf("PRI 解析错误: pri=%d fac=%d sev=%d（期望 34/4/2）", m.PRI, m.Facility, m.Severity)
	}
	if m.Hostname != "mymachine" {
		t.Errorf("hostname = %q", m.Hostname)
	}
	if m.AppName != "su" || m.ProcID != "230" {
		t.Errorf("tag 解析错误: app=%q pid=%q", m.AppName, m.ProcID)
	}
	if m.Msg != "'su root' failed for lonvick on /dev/pts/8" {
		t.Errorf("msg = %q", m.Msg)
	}
	if m.HasYear {
		t.Error("RFC3164 无年份 → HasYear 必须为 false（上层要推断，D-019）")
	}
	if m.Timestamp.Month() != time.October || m.Timestamp.Day() != 11 {
		t.Errorf("时间解析错误: %v", m.Timestamp)
	}
}

func TestParseRFC3164_MessageWithColon(t *testing.T) {
	// 消息体里含 ": " 不能被误当成 tag 分隔符
	raw := `<13>Feb  5 17:32:18 10.0.0.99 app[1]: key: value: more`
	m, _ := Parse([]byte(raw))
	if m.AppName != "app" {
		t.Errorf("app = %q，应取第一个 tag（app）而不是消息体里的 key", m.AppName)
	}
	if m.Msg != "key: value: more" {
		t.Errorf("msg = %q", m.Msg)
	}
	if m.Hostname != "10.0.0.99" {
		t.Errorf("hostname = %q", m.Hostname)
	}
}

func TestParseRFC3164_NoPID(t *testing.T) {
	m, _ := Parse([]byte(`<30>Oct 11 22:14:15 host kernel: boom`))
	if m.AppName != "kernel" || m.ProcID != "" {
		t.Errorf("app=%q pid=%q，期望 kernel/空", m.AppName, m.ProcID)
	}
	if m.Msg != "boom" {
		t.Errorf("msg = %q", m.Msg)
	}
}

func TestParseRFC5424_WithStructuredData(t *testing.T) {
	// RFC 5424 正确写法：多个 SD-ELEMENT 直接相接
	raw := `<165>1 2003-10-11T22:14:15.003Z mymachine.example.com evntslog 1024 ID47 ` +
		`[exampleSDID@32473 iut="3" eventSource="Application" eventID="1011"]` +
		`[examplePriority@32473 class="high"] An application event log entry...`
	m, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("应识别为 RFC5424，得到 version=%d", m.Version)
	}
	if m.PRI != 165 || m.Facility != 20 || m.Severity != 5 {
		t.Errorf("PRI: pri=%d fac=%d sev=%d（期望 165/20/5）", m.PRI, m.Facility, m.Severity)
	}
	if m.Hostname != "mymachine.example.com" || m.AppName != "evntslog" ||
		m.ProcID != "1024" || m.MsgID != "ID47" {
		t.Errorf("头部字段错误: %+v", m)
	}
	if !m.HasYear {
		t.Error("RFC5424 带年份 → HasYear 必须为 true")
	}
	if m.Timestamp.Year() != 2003 || m.Timestamp.Nanosecond() != 3000000 {
		t.Errorf("时间解析错误: %v", m.Timestamp)
	}
	sd := m.StructuredData["exampleSDID@32473"]
	if sd["iut"] != "3" || sd["eventSource"] != "Application" || sd["eventID"] != "1011" {
		t.Errorf("SD 解析错误: %+v", sd)
	}
	if m.StructuredData["examplePriority@32473"]["class"] != "high" {
		t.Errorf("第二个 SD 元素解析错误")
	}
	if m.Msg != "An application event log entry..." {
		t.Errorf("msg = %q", m.Msg)
	}
}

func TestParseRFC5424_NilFields(t *testing.T) {
	m, err := Parse([]byte(`<34>1 - - - - - -`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if m.Version != 1 || m.Hostname != "" || m.AppName != "" || m.ProcID != "" || m.MsgID != "" {
		t.Errorf("`-` 字段应解析为空: %+v", m)
	}
	if m.Msg != "" {
		t.Errorf("msg 应为空，得到 %q", m.Msg)
	}
}

func TestParseRFC5424_EscapedSDValue(t *testing.T) {
	m, err := Parse([]byte(`<34>1 - - - - - [ex@1 msg="a\"b\]c\\d"] body`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := `a"b]c\d`
	if got := m.StructuredData["ex@1"]["msg"]; got != want {
		t.Errorf("转义解析错误: got %q want %q", got, want)
	}
	if m.Msg != "body" {
		t.Errorf("msg = %q", m.Msg)
	}
}

func TestParseRFC5424_BOMStripped(t *testing.T) {
	m, _ := Parse([]byte("<34>1 - - - - - - \ufeffhello"))
	if m.Msg != "hello" {
		t.Errorf("UTF-8 BOM 应被剥离，得到 %q", m.Msg)
	}
}

func TestParse_NoPRI_KeepsRaw(t *testing.T) {
	raw := "plain line without pri"
	m, err := Parse([]byte(raw))
	if err == nil {
		t.Error("无 PRI 应返回错误（但仍保留数据）")
	}
	if m.Raw != raw {
		t.Fatal("解析失败也必须保留原文（FR-206）")
	}
	if m.Msg != raw {
		t.Errorf("无 PRI 时整条当消息体，得到 %q", m.Msg)
	}
}

func TestParse_PRIOutOfRange(t *testing.T) {
	for _, bad := range []string{`<192>Oct 11 22:14:15 h a: b`, `<999>x`, `<abc>x`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("非法 PRI 应报错: %s", bad)
		}
	}
}

func TestParse_NonUTF8_RawPreserved(t *testing.T) {
	// "你好" 的 GBK 编码：C4 E3 BA C3
	gbk := []byte{0xC4, 0xE3, 0xBA, 0xC3}
	raw := append([]byte("<134>Oct 11 22:14:15 host app: "), gbk...)
	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(m.Msg, string(gbk)) {
		t.Error("非 UTF-8 字节必须原样保留（FR-204：编码由上层探测，不由解析器丢弃）")
	}
	if len(m.Raw) != len(raw) {
		t.Errorf("Raw 长度 %d != 输入 %d", len(m.Raw), len(raw))
	}
}

// ── 帧切分（RFC 6587）─────────────────────────────────────────────────

func TestReadFrames_OctetCounting_MultiAndPartial(t *testing.T) {
	// 两帧完整 + 一帧不完整（应留在 rest）
	// 帧格式：`<len> SP <msg>`；末帧故意不完整
	in := []byte("7 <13>1 a7 <13>1 b7 <13>")
	frames, rest, err := ReadFrames(in, FramingOctetCounting)
	if err != nil {
		t.Fatalf("切分失败: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("应切出 2 个完整帧，得到 %d", len(frames))
	}
	if string(frames[0]) != "<13>1 a" || string(frames[1]) != "<13>1 b" {
		t.Errorf("帧内容错误: %q %q", frames[0], frames[1])
	}
	if string(rest) != "7 <13>" {
		t.Errorf("剩余应为不完整帧，得到 %q", rest)
	}
}

func TestReadFrames_NonTransparent_Lines(t *testing.T) {
	in := []byte("<13>1 a\n<13>1 b\r\n<13>1 partial")
	frames, rest, err := ReadFrames(in, FramingNonTransparent)
	if err != nil {
		t.Fatalf("切分失败: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("应切出 2 帧，得到 %d", len(frames))
	}
	if string(frames[1]) != "<13>1 b" {
		t.Errorf("CRLF 应被正确处理，得到 %q", frames[1])
	}
	if string(rest) != "<13>1 partial" {
		t.Errorf("剩余错误: %q", rest)
	}
}

func TestReadFrames_AutoDetect(t *testing.T) {
	// 以数字开头 → octet-counting
	oc, _, _ := ReadFrames([]byte("4 aaaa"), FramingAuto)
	if len(oc) != 1 || string(oc[0]) != "aaaa" {
		t.Errorf("自动识别 octet-counting 失败: %q", oc)
	}
	// 以 `<` 开头 → 按行
	ln, _, _ := ReadFrames([]byte("<13>1 x\n"), FramingAuto)
	if len(ln) != 1 || string(ln[0]) != "<13>1 x" {
		t.Errorf("自动识别非透明帧失败: %q", ln)
	}
}

func TestReadFrames_OctetCounting_BadLength(t *testing.T) {
	if _, _, err := ReadFrames([]byte("999999999 x"), FramingOctetCounting); err == nil {
		t.Error("超限长度应报错（防内存放大）")
	}
	if _, _, err := ReadFrames([]byte("abc x"), FramingOctetCounting); err == nil {
		t.Error("非数字长度应报错")
	}
}

func TestParseRFC5424_SDSeparatedBySpace(t *testing.T) {
	// 部分设备在多个 SD 元素之间插入空格（违反 RFC 但真实存在）→ 应宽容接受
	m, err := Parse([]byte(`<34>1 - - - - - [a@1 x="1"] [b@1 y="2"] body`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if m.StructuredData["a@1"]["x"] != "1" || m.StructuredData["b@1"]["y"] != "2" {
		t.Errorf("宽容解析失败: %+v", m.StructuredData)
	}
	if m.Msg != "body" {
		t.Errorf("msg = %q", m.Msg)
	}
}
