// Package encoding 实现日志字节流的编码探测与转 UTF-8（FR-204）。
//
// 设计约束：
//  1. **零外部 Go 依赖**（D-015）：UTF-8 / UTF-8-BOM / UTF-16(BOM) / Latin-1(Windows-1252)
//     全部用标准库自行解码；
//  2. 其余多字节编码（GB18030/GBK/Big5/EUC-KR/Shift_JIS）**依赖宿主的 `iconv`**
//     （glibc 的 libc-bin 自带，TOS 预装；不属于"需要安装的系统包"）。
//     探测不到 iconv 时**不报错**：原文照常入库，仅记录 `ls_encoding=unknown`（不丢数据）。
//  3. 原文（Record.Raw）**永不被改写** —— 这里只负责给出用于解析的 UTF-8 视图。
package encoding

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// 常见编码名（落库到 ls_encoding）。
const (
	UTF8    = "utf-8"
	UTF16LE = "utf-16le"
	UTF16BE = "utf-16be"
	Latin1  = "iso-8859-1"
	Unknown = "unknown"
)

// externalCandidates 是交给 iconv 尝试的多字节编码（顺序即优先级）。
//
// 说明：GB18030 是 GBK 的超集，中国的遗留应用最常见，故排第一；
// 单纯"转换成功"不能证明编码正确（GB18030 能吞下大部分字节序列），
// 因此**允许用户按采集源显式指定编码**（sourceSpec.Encoding）来绕过自动探测。
var externalCandidates = []string{"GB18030", "BIG5", "EUC-KR", "SHIFT_JIS"}

var (
	iconvOnce sync.Once
	iconvPath string
)

// ConverterAvailable 报告宿主是否提供 iconv（供能力探测与 UI 如实展示）。
func ConverterAvailable() bool {
	iconvOnce.Do(func() {
		p, err := exec.LookPath("iconv")
		if err == nil {
			iconvPath = p
		}
	})
	return iconvPath != ""
}

// Result 是一次编码探测的结果。
type Result struct {
	// UTF8 是可用于解析/落库的 UTF-8 视图（探测失败时为原始字节）。
	UTF8 []byte
	// Name 是探测到的编码名（落库 ls_encoding）。
	Name string
	// Changed 表示 UTF8 与输入字节不同（即发生了转换）。
	Changed bool
}

// Normalize 探测编码并返回 UTF-8 视图。
//
// hint 非空时按用户指定的编码名强制转换（如采集源显式声明 "GB18030"）；
// hint == "utf-8" 时不做任何转换。
//
// **自动探测的固有局限（已在文档中如实声明）**：日志行里高位字节恰好构成另一编码的
// 合法序列时无法可靠区分（如 Windows-1252 的“ 在某些编码下也是合法双字节）。
// 因此：① 转换结果中出现 C1 控制符（U+0080–U+009F）一律判为误判；
// ② CJK 候选还要求输出真有汉字/假名/谚文；
// ③ 遗留编码建议按采集源用 `encoding` 字段显式指定。
func Normalize(b []byte, hint string) Result {
	if len(b) == 0 {
		return Result{UTF8: b, Name: UTF8}
	}
	if h := strings.ToLower(strings.TrimSpace(hint)); h != "" && h != "auto" {
		switch h {
		case UTF8:
			return Result{UTF8: b, Name: UTF8}
		case "windows-1252", Latin1, "latin1", "cp1252":
			out := decodeLatin1(b)
			return Result{UTF8: out, Name: Latin1, Changed: !bytes.Equal(out, b)}
		}
		canon := normalizeName(h)
		if out, ok := convert(canon, b); ok && plausible(canon, out) {
			// 记录**用户声明的编码名**（比规范名更贴近实情）
			return Result{UTF8: out, Name: h, Changed: !bytes.Equal(out, b)}
		}
		// 用户指定的编码不可用/结果不可信：退回自动探测（不丢数据）
	}

	// ① UTF-8（含 ASCII）
	if utf8.Valid(b) {
		if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
			return Result{UTF8: b[3:], Name: UTF8, Changed: true}
		}
		return Result{UTF8: b, Name: UTF8}
	}

	// ② UTF-16（仅在有 BOM 时判定 —— 无 BOM 的 UTF-16 与多字节编码无法可靠区分）
	if out, name, ok := decodeUTF16BOM(b); ok {
		return Result{UTF8: out, Name: name, Changed: true}
	}

	// ③ 多字节编码：交给宿主 iconv，并对结果做可信性检查
	if ConverterAvailable() {
		for _, c := range externalCandidates {
			if out, ok := convert(c, b); ok && plausible(c, out) {
				return Result{UTF8: out, Name: strings.ToLower(c), Changed: true}
			}
		}
	}

	// ④ 兜底：Latin-1/Windows-1252 单字节映射（每个字节都能映射，永不失败、无损）
	return Result{UTF8: decodeLatin1(b), Name: Latin1, Changed: true}
}

// normalizeName 统一用户/调用方给的编码名写法。
func normalizeName(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	switch h {
	case "gbk", "cp936", "gb2312", "gb-2312", "gb18030-2000":
		return "gb18030" // GBK/GB2312 都是 GB18030 的子集
	case "big-5", "big5-hkscs":
		return "big5"
	case "sjis", "shift-jis", "cp932":
		return "shift_jis"
	}
	return h
}

// plausible 判断转换结果是否可信（防误判，见 Normalize 注释）。
func plausible(cand string, out []byte) bool {
	var cjk, hangul, kana int
	for _, r := range string(out) {
		switch {
		case r >= 0x80 && r <= 0x9F:
			return false // C1 控制符 —— 典型的误判特征（如 EUC-KR 把 C1 字节原样透过）
		case r >= 0x4E00 && r <= 0x9FFF, r >= 0x3400 && r <= 0x4DBF:
			cjk++
		case r >= 0xAC00 && r <= 0xD7AF:
			hangul++
		case r >= 0x3040 && r <= 0x30FF:
			kana++
		}
	}
	switch strings.ToUpper(cand) {
	case "GB18030", "GBK":
		return cjk > 0
	case "BIG5":
		return cjk > 0
	case "EUC-KR":
		return hangul > 0
	case "SHIFT_JIS":
		return kana > 0
	}
	return true
}

// convert 用宿主 iconv 转码；失败（不支持的编码/非法字节序列）返回 ok=false。
func convert(name string, b []byte) ([]byte, bool) {
	if !ConverterAvailable() {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, iconvPath, "-f", name, "-t", "UTF-8")
	cmd.Stdin = bytes.NewReader(b)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	// iconv 可能"部分成功"后以非 0 退出（已在上面拦截）；再校验输出必须是合法 UTF-8
	if !utf8.Valid(out.Bytes()) {
		return nil, false
	}
	return out.Bytes(), true
}

// decodeUTF16BOM 处理带 BOM 的 UTF-16。
func decodeUTF16BOM(b []byte) ([]byte, string, bool) {
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		return utf16Decode(b[2:], false), UTF16LE, true
	case len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF:
		return utf16Decode(b[2:], true), UTF16BE, true
	}
	return nil, "", false
}

func utf16Decode(b []byte, bigEndian bool) []byte {
	n := len(b) / 2
	u := make([]uint16, 0, n)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		}
	}
	return []byte(string(utf16.Decode(u)))
}

// win1252High 是 Windows-1252 在 0x80–0x9F 区间与 ISO-8859-1 的差异表（其余字节一一对应）。
var win1252High = [32]rune{
	0x20AC, 0xFFFD, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0xFFFD, 0x017D, 0xFFFD,
	0xFFFD, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0xFFFD, 0x017E, 0x0178,
}

// decodeLatin1 把每个字节映射为一个 Unicode 码点（Windows-1252 口径）。
// 该映射**无损可逆**（单字节 ↔ 单码点），所以可用作永不失败的兜底。
func decodeLatin1(b []byte) []byte {
	var sb strings.Builder
	sb.Grow(len(b) * 2)
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case c >= 0x80 && c <= 0x9F:
			r := win1252High[c-0x80]
			if r == 0xFFFD {
				r = rune(c) // 未定义位置按 ISO-8859-1 原样映射
			}
			sb.WriteRune(r)
		default:
			sb.WriteRune(rune(c))
		}
	}
	return []byte(sb.String())
}

// ErrNoConverter 在需要外部转换器而不可用时返回（调用方据此决定是否提示用户）。
var ErrNoConverter = errors.New("宿主未提供 iconv，多字节编码无法转换（原文仍完整保留）")
