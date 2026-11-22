package pipeline

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Moechz/log-server/backend/internal/encoding"
	"github.com/Moechz/log-server/backend/internal/model"
)

// FormatJournald 是 systemd journald 来源的格式标识（ls_format=journald）。
const FormatJournald = "journald"

// JournalFields 是 journald JSON 里我们要映射到派生字段的键（其余进"用户字段"，原样保留）。
var journalMappedKeys = map[string]bool{
	"MESSAGE": true, "PRIORITY": true, "SYSLOG_IDENTIFIER": true, "_COMM": true,
	"_HOSTNAME": true, "_PID": true, "__REALTIME_TIMESTAMP": true, "_SOURCE_REALTIME_TIMESTAMP": true,
}

// ProcessJournal 把一条 journald JSON 输出（`journalctl -o json`）转成 Record（FR-101/FR-901）。
//
// 设计要点：
//   - **原文不可变**（D-005）：原始 JSON 行逐字节进 `Record.Raw` → 落库为 `_msg`；
//     MESSAGE 只是"解析出的消息体"（`ls_msg`），不替代原文；
//   - 其余 journald 字段（`_SYSTEMD_UNIT`、`_BOOT_ID`、`_TRANSPORT`、`_UID`…）全部进
//     **用户字段**（不加前缀，原样保留），检索时可 `extra.` 前缀或直接按字段名查询；
//   - 级别按 D-006 双字段：`PRIORITY`(0–7) → `severity_number`，同时保留原始文本级别名；
//   - 时间取 `__REALTIME_TIMESTAMP`（微秒），仍走时间闸门（D-019）；
//   - 解析失败**不丢数据**：原文照常入库 + `ls_parse_error`（FR-206）。
func (p *Processor) ProcessJournal(raw []byte, ingest time.Time) *model.Record {
	rec := &model.Record{
		Raw:        string(raw),
		IngestTime: ingest.UTC(),
		Source:     model.Source{Kind: model.SrcJournald, Collector: "journald"},
		Format:     FormatJournald,
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		rec.ParseError = "journald JSON 解析失败: " + errString(err)
		rec.Msg = string(raw)
		rec.Encoding = encoding.Normalize(raw, "").Name
		return p.finalize(rec)
	}

	// ① 消息体：MESSAGE 可能是字符串，也可能是**字节数组**（非 UTF-8 内容 journald 用数组表示）
	msgRaw, enc := messageBytes(m["MESSAGE"])
	rec.Encoding = enc
	if msgRaw != nil {
		nz := encoding.Normalize(msgRaw, "")
		rec.Msg = string(nz.UTF8)
		if rec.Encoding == "" || rec.Encoding == encoding.UTF8 {
			rec.Encoding = nz.Name
		}
	} else {
		rec.Msg = string(raw)
		rec.Encoding = encoding.UTF8
	}
	if rec.Encoding == "" {
		rec.Encoding = encoding.UTF8
	}

	// ② 派生字段
	if v, ok := m["SYSLOG_IDENTIFIER"].(string); ok && v != "" {
		rec.App = v
	} else if v, ok := m["_COMM"].(string); ok {
		rec.App = v
	}
	if v, ok := m["_HOSTNAME"].(string); ok {
		rec.Host = v
	}
	if v, ok := m["_PID"].(string); ok {
		if n, err := strconv.Atoi(v); err == nil {
			rec.PID = &n
		}
	}
	if v, ok := m["PRIORITY"].(string); ok {
		if sev, err := strconv.Atoi(v); err == nil && sev >= 0 && sev <= 7 {
			rec.Severity = &sev
			rec.SeverityNumber = &sev
			rec.Level = LevelName(sev)
		}
	}
	if t, ok := journalTime(m); ok {
		rec.EventTime = t.UTC()
		rec.EventTimeRaw = t.UTC().Format(time.RFC3339Nano)
	}

	// ③ 其余字段进用户字段（原样保留；MESSAGE 已映射为消息体，不再重复塞大字段）
	user := make(map[string]any, len(m))
	for k, v := range m {
		if k == "MESSAGE" {
			continue
		}
		user[k] = v
	}
	rec.User = user

	return p.finalize(rec)
}

// messageBytes 取 MESSAGE：字符串 → 原字节；数组（字节序列）→ 拼接；其它类型 → 文本形式。
func messageBytes(v any) ([]byte, string) {
	switch t := v.(type) {
	case nil:
		return nil, ""
	case string:
		return []byte(t), encoding.UTF8
	case []any:
		out := make([]byte, 0, len(t))
		for _, e := range t {
			switch n := e.(type) {
			case float64:
				out = append(out, byte(int(n)&0xFF))
			case string:
				out = append(out, []byte(n)...)
			}
		}
		// journald 用字节数组表示"非文本"内容：能按 UTF-8 读出来就用文本，
		// 否则按 D-005 的要求以 base64 完整保留（绝不丢字节）。
		if utf8.Valid(out) {
			return out, encoding.UTF8
		}
		return []byte(base64.StdEncoding.EncodeToString(out)), "base64"
	default:
		return []byte(fmt.Sprint(t)), encoding.UTF8
	}
}

// journalTime 解析 journald 的微秒时间戳（__REALTIME_TIMESTAMP，字符串或数字）。
func journalTime(m map[string]any) (time.Time, bool) {
	for _, k := range []string{"__REALTIME_TIMESTAMP", "_SOURCE_REALTIME_TIMESTAMP"} {
		switch v := m[k].(type) {
		case string:
			if us, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && us > 0 {
				return time.Unix(us/1_000_000, (us%1_000_000)*1000), true
			}
		case float64:
			us := int64(v)
			if us > 0 {
				return time.Unix(us/1_000_000, (us%1_000_000)*1000), true
			}
		}
	}
	return time.Time{}, false
}

func errString(err error) string {
	if err == nil {
		return "结构不是 JSON 对象"
	}
	return err.Error()
}
