package encoding

import (
	"bytes"
	"testing"
)

func TestNormalize_UTF8Passthrough(t *testing.T) {
	in := []byte("hello 世界")
	r := Normalize(in, "")
	if r.Name != UTF8 || r.Changed {
		t.Fatalf("UTF-8 不应被改写: name=%q changed=%v", r.Name, r.Changed)
	}
	if !bytes.Equal(r.UTF8, in) {
		t.Error("内容应逐字节一致")
	}
}

func TestNormalize_UTF8BOMStripped(t *testing.T) {
	r := Normalize([]byte("\xEF\xBB\xBFhello"), "")
	if r.Name != UTF8 || !r.Changed {
		t.Fatalf("应识别 UTF-8 BOM: name=%q changed=%v", r.Name, r.Changed)
	}
	if string(r.UTF8) != "hello" {
		t.Errorf("BOM 应被剥离，得到 %q", r.UTF8)
	}
}

func TestNormalize_UTF16BOM(t *testing.T) {
	// "hi" 的 UTF-16LE / BE
	le := []byte{0xFF, 0xFE, 'h', 0x00, 'i', 0x00}
	be := []byte{0xFE, 0xFF, 0x00, 'h', 0x00, 'i'}

	if r := Normalize(le, ""); r.Name != UTF16LE || string(r.UTF8) != "hi" {
		t.Errorf("UTF-16LE 解码失败: name=%q out=%q", r.Name, r.UTF8)
	}
	if r := Normalize(be, ""); r.Name != UTF16BE || string(r.UTF8) != "hi" {
		t.Errorf("UTF-16BE 解码失败: name=%q out=%q", r.Name, r.UTF8)
	}
}

func TestNormalize_Latin1Fallback(t *testing.T) {
	// 0xE9 在 GB18030/Big5/EUC-KR/SJIS 里都是非法序列 → 应落到 Latin-1
	in := []byte("caf\xe9 au lait")
	r := Normalize(in, "")
	if r.Name != Latin1 {
		t.Fatalf("期望 Latin-1 兜底，得到 %q", r.Name)
	}
	if string(r.UTF8) != "café au lait" {
		t.Errorf("Latin-1 解码结果错误: %q", r.UTF8)
	}
}

func TestNormalize_Windows1252HighRange(t *testing.T) {
	// 0x93/0x94 是 Windows-1252 的弯引号（ISO-8859-1 中为未定义控制符）
	r := Normalize([]byte{0x93, 'a', 0x94}, "")
	if string(r.UTF8) != "“a”" {
		t.Errorf("Windows-1252 高位映射错误: %q（name=%s）", r.UTF8, r.Name)
	}
}

func TestNormalize_HintUTF8SkipsConversion(t *testing.T) {
	in := []byte("\xC4\xE3\xBA\xC3") // GBK "你好"
	r := Normalize(in, "utf-8")
	if r.Name != UTF8 || !bytes.Equal(r.UTF8, in) {
		t.Errorf("用户显式指定 utf-8 时不应转换: name=%q", r.Name)
	}
}

func TestNormalize_GBK(t *testing.T) {
	if !ConverterAvailable() {
		t.Skip("宿主无 iconv：GB18030 走原文保留路径（另有 TestNormalize_NoConverterKeepsRaw）")
	}
	g := []byte{0xC4, 0xE3, 0xBA, 0xC3} // "你好"
	r := Normalize(g, "")
	if r.Name != "gb18030" {
		t.Fatalf("期望 gb18030，得到 %q", r.Name)
	}
	if string(r.UTF8) != "你好" {
		t.Errorf("GBK→UTF-8 结果错误: %q", r.UTF8)
	}
}

func TestNormalize_HintForcesEncoding(t *testing.T) {
	if !ConverterAvailable() {
		t.Skip("宿主无 iconv")
	}
	g := []byte{0xC4, 0xE3, 0xBA, 0xC3}
	r := Normalize(g, "GBK")
	if r.Name != "gbk" || string(r.UTF8) != "你好" {
		t.Errorf("显式 hint 应生效: name=%q out=%q", r.Name, r.UTF8)
	}
}

func TestNormalize_BinaryNeverPanics(t *testing.T) {
	// 二进制（含非法字节）：永不 panic，且必须给出可落库的 UTF-8 视图与编码名
	in := []byte{0x00, 0x01, 0xFF, 0xFE, 0x80}
	r := Normalize(in, "")
	if len(r.UTF8) == 0 {
		t.Error("二进制输入应给出非空视图（原文永远另有 Raw 字段保底）")
	}
	if r.Name == "" {
		t.Error("必须给出编码名（unknown/iso-8859-1 均可）")
	}
}
