package model

import "strings"

// SeverityFromText 按《DEV-REQUIREMENTS.md 附录 B》把应用侧级别文本归一化为
// syslog severity（0–7，数值越小越严重）。第二个返回值表示是否识别成功。
//
// 约定（D-006）：**原始级别文本永远保留在 Record.Level**，这里只产出归一化数值；
// 识别失败时返回 ok=false，调用方应留空 severity_number 而不是猜一个值。
func SeverityFromText(text string) (int, bool) {
	s := strings.ToLower(strings.TrimSpace(text))
	s = strings.Trim(s, "[]()\"'.:")
	if s == "" {
		return 0, false
	}
	switch s {
	case "emerg", "emergency":
		return 0, true
	case "alert":
		return 1, true
	case "crit", "critical", "fatal", "panic", "severe", "dpanic":
		return 2, true
	case "err", "error", "eror", "e":
		return 3, true
	case "warn", "warning", "wrn", "w":
		return 4, true
	case "notice":
		return 5, true
	case "info", "information", "informational", "i", "log":
		return 6, true
	case "debug", "trace", "verbose", "fine", "finer", "finest", "d", "t", "v":
		return 7, true
	}
	// 纯数字：只接受 syslog 域（0–7）。
	// 其它数值域（Python 10/20/30…、log4j、pino）方向相反且无法从文本可靠推断，
	// 宁可留空 severity_number 也不猜（D-006：不做有损覆盖）。
	if n, ok := atoiStrict(s); ok {
		if n >= 0 && n <= 7 {
			return n, true
		}
		return 0, false
	}
	return 0, false
}

func atoiStrict(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
		if n > 1_000_000 {
			return 0, false
		}
	}
	return n, true
}
