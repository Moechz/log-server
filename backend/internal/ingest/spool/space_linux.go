//go:build linux

package spool

import "syscall"

// CapMaxBytes 按队列所在卷的剩余空间收紧上限：
//   - 默认最多占用剩余空间的 10%；
//   - 剩余空间很小时（< 32 MB 的 10%），最多用一半，避免把卷写满；
//   - want <= 0 时也返回该兜底值（不允许真正"无上限"增长）。
func CapMaxBytes(dir string, want int64) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return want
	}
	free := int64(st.Bavail) * int64(st.Bsize)
	if free <= 0 {
		return want
	}
	limit := free / 10
	if limit < 32<<20 {
		limit = free / 2
	}
	if want > 0 && limit > want {
		limit = want
	}
	if limit <= 0 {
		return want
	}
	return limit
}
