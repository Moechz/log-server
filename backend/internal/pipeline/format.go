package pipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
	"github.com/Moechz/log-server/backend/internal/receive/syslog"
)

// Format 是 FR-201 的日志格式标识（落库 `ls_format`）。
//
// 识别失败一律归入 plain（**不丢数据**：原文永远在 `_msg` / `Record.Raw`）。
type Format string

const (
	FormatRFC5424 Format = "rfc5424"
	FormatRFC3164 Format = "rfc3164"
	FormatJSON    Format = "json"
	FormatLogfmt  Format = "logfmt"
	FormatCLF     Format = "clf" // nginx/apache common 与 combined
	FormatPlain   Format = "plain"
)

// DetectFormat 识别一行日志的格式（FR-201）。
//
// 判定顺序有讲究：结构化格式优先（它们不会与 syslog 头冲突），
// syslog 判定放最后，因为 syslog 解析器对"前缀像主机名的普通文本"有一定容忍度。
func DetectFormat(line string) Format {
	s := strings.TrimSpace(line)
	if s == "" {
		return FormatPlain
	}
	if s[0] == '{' {
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) == nil && m != nil {
			return FormatJSON
		}
	}
	if _, _, ok := parseCLF(s); ok {
		return FormatCLF
	}
	if looksLogfmt(s) {
		return FormatLogfmt
	}
	if m, err := syslog.Parse([]byte(line)); err == nil {
		switch {
		case m.Version > 0:
			return FormatRFC5424
		case m.PRI >= 0 || !m.Timestamp.IsZero() || m.Hostname != "":
			return FormatRFC3164
		}
	}
	return FormatPlain
}

// clfRe 匹配 nginx/apache 的 common 与 combined 格式（含 IPv6 主机与 "-" 占位）。
var clfRe = regexp.MustCompile(
	`^(?P<host>\S+)\s+(?P<ident>\S+)\s+(?P<user>\S+)\s+\[(?P<time>[^\]]+)\]\s+` +
		`"(?P<req>[^"]*)"\s+(?P<status>\d{3})\s+(?P<bytes>\d+|-)(?:\s+"(?P<ref>[^"]*)"\s+"(?P<ua>[^"]*)")?`)

// clfTimeLayout 是 CLF 的时间格式（含时区偏移）。
const clfTimeLayout = "02/Jan/2006:15:04:05 -0700"

// parseCLF 解析 CLF（nginx/apache）日志行，返回用户字段、事件时间与是否识别成功。
func parseCLF(line string) (map[string]any, time.Time, bool) {
	m := clfRe.FindStringSubmatch(line)
	if m == nil {
		return nil, time.Time{}, false
	}
	f := func(name string) string {
		i := clfRe.SubexpIndex(name)
		if i <= 0 || i >= len(m) {
			return ""
		}
		return m[i]
	}
	out := map[string]any{
		"remote_host": f("host"),
		"remote_user": f("user"),
		"ident":       f("ident"),
		"request":     f("req"),
		"status":      f("status"),
	}
	if b := f("bytes"); b != "" && b != "-" {
		if n, err := strconv.Atoi(b); err == nil {
			out["bytes"] = n
		}
	}
	if ref := f("ref"); ref != "" && ref != "-" {
		out["referer"] = ref
	}
	if ua := f("ua"); ua != "" && ua != "-" {
		out["user_agent"] = ua
	}
	// 拆请求行：METHOD URI PROTO
	if parts := strings.SplitN(f("req"), " ", 3); len(parts) == 3 {
		out["request_method"], out["request_uri"], out["protocol"] = parts[0], parts[1], parts[2]
	}
	var ts time.Time
	if t, err := time.Parse(clfTimeLayout, f("time")); err == nil {
		ts = t
	}
	return out, ts, true
}

// logfmtPairRe 匹配 logfmt 的 `key=value`（value 可带引号）。
var logfmtPairRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]*=`)

// looksLogfmt 判断一行是否 logfmt（多数 token 形如 key=value）。
func looksLogfmt(s string) bool {
	tokens := splitLogfmtTokens(s)
	if len(tokens) < 2 {
		return false
	}
	hit := 0
	for _, t := range tokens {
		if logfmtPairRe.MatchString(t) {
			hit++
		}
	}
	return hit*2 >= len(tokens) && hit >= 2
}

// splitLogfmtTokens 按空白切分，但保留引号内的内容。
func splitLogfmtTokens(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote != 0:
			cur.WriteByte(c)
			if c == inQuote {
				inQuote = 0
			}
		case c == '"' || c == '\'':
			inQuote = c
			cur.WriteByte(c)
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// parseLogfmt 解析 logfmt 行为键值对（值去掉外层引号）。
func parseLogfmt(s string) map[string]string {
	out := map[string]string{}
	for _, t := range splitLogfmtTokens(s) {
		i := strings.IndexByte(t, '=')
		if i <= 0 {
			continue
		}
		k := t[:i]
		v := t[i+1:]
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ── 结构化字段 → 派生字段（FR-202 / FR-203）─────────────────────────

// 常见字段名（小写比较；用户原始键名不改动，只是我们用来填派生字段）。
var (
	msgKeys   = []string{"message", "msg", "log", "text", "body", "event"}
	levelKeys = []string{"level", "lvl", "loglevel", "levelname", "severity", "sev"}
	timeKeys  = []string{"time", "timestamp", "ts", "@timestamp", "eventtime", "event_time", "date", "time_local"}
	hostKeys  = []string{"host", "hostname", "host_name", "server", "nodename"}
	appKeys   = []string{"app", "application", "service", "logger", "program", "component", "module"}
)

// applyCommon 从结构化字段提取 msg/level/host/app/time 填进派生段（缺失则不动）。
//
// kv 是"小写键 → 值文本"的视图；rec.User 保留原始键与原始类型。
func applyCommon(rec *model.Record, kv map[string]string) {
	if v, ok := pick(kv, msgKeys); ok {
		rec.Msg = v
	}
	if v, ok := pick(kv, levelKeys); ok {
		rec.Level = v
		if sev, ok2 := model.SeverityFromText(v); ok2 {
			rec.SeverityNumber = &sev
		}
	}
	if v, ok := pick(kv, hostKeys); ok {
		rec.Host = v
	}
	if v, ok := pick(kv, appKeys); ok {
		rec.App = v
	}
	if v, ok := pick(kv, timeKeys); ok {
		if t, ok2 := parseTimeText(v); ok2 {
			rec.EventTime = t.UTC()
			rec.EventTimeRaw = v
		}
	}
}

func pick(kv map[string]string, keys []string) (string, bool) {
	for _, k := range keys {
		if v, ok := kv[k]; ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// parseTimeText 尽力解析常见时间文本（RFC3339 / "2006-01-02 15:04:05" / unix 秒-纳秒）。
func parseTimeText(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	layouts := []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05", "2006-01-02T15:04:05", clfTimeLayout,
		"2006/01/02 15:04:05", "01/02/2006 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, v); err == nil {
			return t, true
		}
	}
	// 纯数字：unix 秒 / 毫秒 / 微秒 / 纳秒
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
		switch len(v) {
		case 10:
			return time.Unix(n, 0), true
		case 13:
			return time.Unix(n/1000, (n%1000)*int64(time.Millisecond)), true
		case 16:
			return time.Unix(n/1_000_000, (n%1_000_000)*int64(time.Microsecond)), true
		case 19:
			return time.Unix(0, n), true
		}
	}
	return time.Time{}, false
}

// lowerKV 把 map[string]any 展平成"小写键 → 文本"（用于提取派生字段）。
func lowerKV(in map[string]any) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		switch t := v.(type) {
		case nil:
			continue
		case string:
			out[strings.ToLower(k)] = t
		case float64:
			out[strings.ToLower(k)] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			out[strings.ToLower(k)] = strconv.FormatBool(t)
		default:
			out[strings.ToLower(k)] = fmt.Sprint(t)
		}
	}
	return out
}

// stringKV 把 map[string]string 转为小写键视图（logfmt 用；原键名在 User 中保留）。
func stringKV(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

// anyFromStrings 把 logfmt 的 string map 转成 any map（保留原始键名）。
func anyFromStrings(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
