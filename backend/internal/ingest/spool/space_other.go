//go:build !linux

package spool

// CapMaxBytes 在非 Linux 平台不做剩余空间收紧（仅用于本地测试）。
func CapMaxBytes(_ string, want int64) int64 { return want }
