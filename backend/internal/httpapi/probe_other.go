//go:build !unix

package httpapi

// probeTCP 非 unix 平台占位（本项目只发布 linux）。
func probeTCP(string) (bool, string) { return false, "当前平台不支持探测" }
