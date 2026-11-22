// Package mask 提供**统一的脱敏原语**（红线 7：凭据绝不进入任何日志、导出件、诊断包）。
//
// 为什么要独立成包：诊断包导出与**自身日志落盘**都需要同一套判定，
// 两处各写一份必然漂移（一处修了另一处忘），所以把规则收敛到这里。
//
// 判定顺序：
//  1. 键名语义优先（Key）——命中即整体替换为 `***`；
//  2. 值样式兜底（Value）——键名看不出来时的后备（token/PAT/私钥块/Bearer/URL userinfo）。
package mask

import "regexp"

// Placeholder 是脱敏后的占位符。
const Placeholder = "***"

var (
	// keyRe 命中即整体替换（键名语义优先）。
	keyRe = regexp.MustCompile(`(?i)(pass|passwd|secret|token|credential|apikey|api_key|private_key|key_file|cert_file|pinned_sha256|dsn)`)
	// valueRe 命中字符串里的凭据样式（键名看不出来时的兜底）。
	valueRe = regexp.MustCompile(`(ghp_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|Bearer\s+[A-Za-z0-9._~+/-]{20,})`)
	// urlUserInfoRe 命中 URL 里的 user:pass@（保留主机，去掉凭据）。
	urlUserInfoRe = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/@\s]+@`)
)

// Key 判断键名是否属于凭据字段（命中则其值整体替换）。
func Key(k string) bool { return keyRe.MatchString(k) }

// Value 对字符串做样式级脱敏（幂等）。
func Value(s string) string {
	s = valueRe.ReplaceAllString(s, Placeholder)
	return urlUserInfoRe.ReplaceAllString(s, "$1"+Placeholder+"@")
}

// Fields 返回按键脱敏后的字段副本（键名命中 → 值替换；否则对值做样式脱敏）。
// nil/空输入返回 nil，便于 json 省略空对象。
func Fields(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if Key(k) {
			out[k] = Placeholder
			continue
		}
		out[k] = Value(v)
	}
	return out
}
