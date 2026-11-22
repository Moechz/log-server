// Package syslog 实现 RFC 3164 / RFC 5424 解析与 RFC 6587 帧切分。
//
// 设计要点（对齐 D-005 三段式）：
//   - **原文逐字节保留**在 Message.Raw，解析失败也绝不丢（FR-206）
//   - 解析出的字段进"派生"段；未解析成功时 Raw 仍是唯一可信来源
//   - 不假设编码是 UTF-8：非 UTF-8 字节原样保留，由上层做编码探测（FR-204）
package syslog

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Framing 是 RFC 6587 的两种 TCP 帧格式。
type Framing int

const (
	// FramingAuto 自动识别（优先 octet-counting，否则按行）
	FramingAuto Framing = iota
	// FramingOctetCounting 「MSG-LEN SP SYSLOG-MSG」
	FramingOctetCounting
	// FramingNonTransparent 以 LF 分隔（老设备常用）
	FramingNonTransparent
)

// Message 是一条解析后的 syslog 消息。
type Message struct {
	Raw string // ① 原文（逐字节，含 <PRI> 头）

	PRI      int // -1 表示缺失
	Facility int
	Severity int

	Version int // 0 = RFC3164，1 = RFC5424

	Timestamp    time.Time // 缺失时为 zero
	TimestampRaw string
	HasYear      bool // RFC3164 无年份 → false（上层需推断，见 D-019）

	Hostname string
	AppName  string
	ProcID   string
	MsgID    string

	// StructuredData 仅 RFC 5424：[SD-ID]map[PARAM]value
	StructuredData map[string]map[string]string

	Msg string // 消息体（去掉头部）
}

const (
	// MaxMessageSize 单条消息上限（超过按畸形处理，但仍保留原文）
	MaxMessageSize = 1 << 20
	// MaxFramesPerRead 一次 ReadFrames 最多切出的帧数（防内存放大）
	MaxFramesPerRead = 4096
)

// Parse 解析单条 syslog 消息（RFC 5424 优先，回退 RFC 3164）。
// 永不因格式问题丢弃：解析失败时也返回带 Raw 的 Message 与 error。
func Parse(raw []byte) (*Message, error) {
	m := &Message{Raw: string(raw), PRI: -1, Facility: -1, Severity: -1}
	if len(raw) == 0 {
		return m, fmt.Errorf("空消息")
	}
	if len(raw) > MaxMessageSize {
		return m, fmt.Errorf("消息超过上限 %d 字节", MaxMessageSize)
	}

	rest, err := m.parsePRI(raw)
	if err != nil {
		m.Msg = string(raw) // 没有 PRI：整条当消息体（老设备/自定义源）
		return m, err
	}
	m.Msg = string(rest)

	// RFC 5424：<PRI>VERSION SP ...
	if len(rest) > 0 && rest[0] >= '1' && rest[0] <= '9' {
		if ok := m.parseRFC5424(rest); ok {
			return m, nil
		}
	}
	// RFC 3164
	m.parseRFC3164(rest)
	return m, nil
}

// parsePRI 解析 `<0-191>`，返回剩余字节。
func (m *Message) parsePRI(b []byte) ([]byte, error) {
	if len(b) < 3 || b[0] != '<' {
		return b, fmt.Errorf("缺少 <PRI>")
	}
	end := bytes.IndexByte(b, '>')
	if end < 2 || end > 4 {
		return b, fmt.Errorf("<PRI> 格式非法")
	}
	n, err := strconv.Atoi(string(b[1:end]))
	if err != nil || n < 0 || n > 191 {
		return b, fmt.Errorf("<PRI> 越界或非法: %q", b[1:end])
	}
	m.PRI = n
	m.Facility = n / 8
	m.Severity = n % 8
	return b[end+1:], nil
}

// parseRFC5424 解析头部：VERSION SP TIMESTAMP SP HOSTNAME SP APP-NAME SP PROCID SP MSGID SP SD [SP MSG]
func (m *Message) parseRFC5424(b []byte) bool {
	// 需要 6 个头部字段（VERSION/TIMESTAMP/HOSTNAME/APP-NAME/PROCID/MSGID）+ 剩余部分
	// 注意 splitN 的语义：splitN(b, sep, n) 返回 n-1 个字段 + 剩余 → 故传 7
	fields, rest, ok := splitN(b, ' ', 7)
	if !ok || len(fields) != 6 {
		return false
	}
	ver, err := strconv.Atoi(string(fields[0]))
	if err != nil || ver < 1 || ver > 999 {
		return false
	}
	m.Version = ver

	// TIMESTAMP（`-` 表示缺失）
	if ts := string(fields[1]); ts != "-" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			m.Timestamp, m.TimestampRaw, m.HasYear = t, ts, true
		} else {
			m.TimestampRaw = ts
		}
	}
	if v := string(fields[2]); v != "-" {
		m.Hostname = v
	}
	if v := string(fields[3]); v != "-" {
		m.AppName = v
	}
	if v := string(fields[4]); v != "-" {
		m.ProcID = v
	}
	if v := string(fields[5]); v != "-" {
		m.MsgID = v
	}

	// STRUCTURED-DATA：`-` 或一个/多个 [SD-ID PARAM="v" ...]
	sd, msg, ok := parseStructuredData(rest)
	if !ok {
		return false
	}
	m.StructuredData = sd
	// MSG 可能带 UTF-8 BOM
	m.Msg = strings.TrimPrefix(string(msg), "\ufeff")
	return true
}

// parseStructuredData 解析 SD 段并返回剩余消息体。
func parseStructuredData(b []byte) (map[string]map[string]string, []byte, bool) {
	if len(b) == 0 {
		return nil, nil, true
	}
	if b[0] == '-' {
		rest := b[1:]
		if len(rest) > 0 && rest[0] == ' ' {
			rest = rest[1:]
		}
		return nil, rest, true
	}
	if b[0] != '[' {
		// 既不是 `-` 也不是 `[` → 头部不合法
		return nil, nil, false
	}
	out := map[string]map[string]string{}
	i := 0
	for i < len(b) && b[i] == '[' {
		// 找未转义的 ]
		j, depth := i+1, 1
		for j < len(b) {
			switch b[j] {
			case '\\':
				j += 2
				continue
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					goto found
				}
			}
			j++
		}
		return nil, nil, false
	found:
		body := string(b[i+1 : j])
		id, params := parseSDElement(body)
		if id != "" {
			out[id] = params
		}
		i = j + 1
		// 宽容处理：RFC 5424 规定多个 SD-ELEMENT 直接相接（`[a][b]`），
		// 但实测部分设备/库会在其间插入一个空格（`[a] [b]`）。
		// 仅当空格后紧跟 `[` 时才跳过，尽量不与「MSG 以 [ 开头」混淆。
		if i < len(b) && b[i] == ' ' && i+1 < len(b) && b[i+1] == '[' {
			i++
		}
	}
	rest := b[i:]
	if len(rest) > 0 && rest[0] == ' ' {
		rest = rest[1:]
	}
	return out, rest, true
}

// parseSDElement 解析 `SD-ID PARAM="value"...`。
func parseSDElement(s string) (string, map[string]string) {
	sp := strings.IndexByte(s, ' ')
	id := s
	if sp >= 0 {
		id, s = s[:sp], s[sp+1:]
	} else {
		return id, nil
	}
	params := map[string]string{}
	for {
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			break
		}
		k := s[:eq]
		s = s[eq+1:]
		if len(s) == 0 || s[0] != '"' {
			break
		}
		s = s[1:]
		var sb strings.Builder
		i := 0
		for i < len(s) {
			if s[i] == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case '"':
					sb.WriteByte('"')
				case '\\':
					sb.WriteByte('\\')
				case ']':
					sb.WriteByte(']')
				default:
					sb.WriteByte(s[i+1])
				}
				i += 2
				continue
			}
			if s[i] == '"' {
				i++
				break
			}
			sb.WriteByte(s[i])
			i++
		}
		params[k] = sb.String()
		s = s[i:]
		if len(s) > 0 && s[0] == ' ' {
			s = s[1:]
			continue
		}
		break
	}
	return id, params
}

// parseRFC3164 解析 `<PRI>MMM dd hh:mm:ss HOSTNAME TAG[PID]: MSG`。
// 注意：**无年份、无时区** → HasYear=false，上层必须推断（D-019）。
func (m *Message) parseRFC3164(b []byte) {
	m.Version = 0
	s := string(b)

	// 时间戳长度固定 15：`Jan _2 15:04:05`（日字段可能用空格填充）
	if len(s) >= 15 {
		ts := s[:15]
		if t, err := time.Parse("Jan _2 15:04:05", ts); err == nil {
			m.Timestamp, m.TimestampRaw, m.HasYear = t, ts, false
			s = strings.TrimPrefix(s[15:], " ")
		}
	}

	// HOSTNAME（到第一个空格）
	host := s
	if i := strings.IndexByte(s, ' '); i >= 0 {
		host, s = s[:i], s[i+1:]
	} else {
		s = ""
	}
	m.Hostname = host

	// TAG[PID]: MSG —— 冒号后必须跟空格，否则不当成 tag（避免误切消息体里的冒号）
	if i := strings.Index(s, ": "); i >= 0 && i <= 64 {
		tag := s[:i]
		rest := s[i+2:]
		if len(tag) > 0 && !strings.ContainsAny(tag, " \t") {
			m.AppName, m.ProcID = splitTagPID(tag)
			m.Msg = rest
			return
		}
	}
	m.Msg = s
}

// splitTagPID 把 `sshd[1234]` 拆成 app=sshd、pid=1234。
func splitTagPID(tag string) (string, string) {
	open := strings.IndexByte(tag, '[')
	if open < 0 || !strings.HasSuffix(tag, "]") {
		return tag, ""
	}
	return tag[:open], tag[open+1 : len(tag)-1]
}

// splitN 按 sep 切出 n 段，返回前 n-1 段与剩余部分。
func splitN(b []byte, sep byte, n int) ([][]byte, []byte, bool) {
	fields := make([][]byte, 0, n)
	start := 0
	for i := 0; i < len(b) && len(fields) < n-1; i++ {
		if b[i] == sep {
			fields = append(fields, b[start:i])
			start = i + 1
		}
	}
	fields = append(fields, b[start:])
	if len(fields) < n {
		return fields, nil, false
	}
	return fields[:n-1], fields[n-1], true
}

// ReadFrames 从流中切出完整帧（RFC 6587）。
// 返回切出的帧、未消费的剩余字节（下轮拼接）。
func ReadFrames(buf []byte, framing Framing) (frames [][]byte, rest []byte, err error) {
	switch framing {
	case FramingOctetCounting:
		return readOctetCounting(buf)
	case FramingNonTransparent:
		return readLines(buf)
	default:
		// 自动：首字符是数字则按 octet-counting，否则按行
		if len(buf) > 0 && buf[0] >= '0' && buf[0] <= '9' {
			return readOctetCounting(buf)
		}
		return readLines(buf)
	}
}

// readOctetCounting 处理 `MSG-LEN SP SYSLOG-MSG`。
func readOctetCounting(buf []byte) ([][]byte, []byte, error) {
	var frames [][]byte
	i := 0
	for len(frames) < MaxFramesPerRead {
		sp := bytes.IndexByte(buf[i:], ' ')
		if sp < 0 {
			break
		}
		sp += i
		n, err := strconv.Atoi(string(buf[i:sp]))
		if err != nil || n < 0 || n > MaxMessageSize {
			return frames, nil, fmt.Errorf("octet-counting 长度非法: %q", buf[i:sp])
		}
		if sp+1+n > len(buf) {
			break // 尚未收全
		}
		frames = append(frames, buf[sp+1:sp+1+n])
		i = sp + 1 + n
	}
	return frames, buf[i:], nil
}

// readLines 处理以 LF 分隔的非透明帧。
func readLines(buf []byte) ([][]byte, []byte, error) {
	var frames [][]byte
	i := 0
	for len(frames) < MaxFramesPerRead {
		nl := bytes.IndexByte(buf[i:], '\n')
		if nl < 0 {
			break
		}
		nl += i
		line := bytes.TrimRight(buf[i:nl], "\r")
		if len(line) > 0 {
			frames = append(frames, line)
		}
		i = nl + 1
	}
	return frames, buf[i:], nil
}
