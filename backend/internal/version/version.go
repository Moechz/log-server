// Package version 保存构建期注入的版本信息。
//
// 版本号的**单一事实源**是 packaging/templates/config.ini；构建时通过
// -ldflags "-X .../internal/version.Version=<ver>" 注入，保证三处一致（D-016）。
package version

import "runtime"

var (
	// AppID 是平台应用 id（D-022）；同时用于 config.ini 与备份件元信息。
	AppID = "log-server-for-tos"
	// Version 由构建期注入（形如 0.1.0-1）。
	Version = "0.1.0-dev"
	// BuildDate 固定为 tag 提交时间戳（可复现构建要求）。
	BuildDate = "unknown"
	// Commit 上游 git commit。
	Commit = "unknown"
)

// Info 返回「关于」页所需的版本信息。
func Info() map[string]string {
	return map[string]string{
		"version":   Version,
		"build":     BuildDate,
		"commit":    Commit,
		"arch":      runtime.GOARCH,
		"os":        runtime.GOOS,
		"goversion": runtime.Version(),
	}
}
