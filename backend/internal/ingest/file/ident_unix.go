//go:build unix

package file

import (
	"os"
	"syscall"
)

// statIdent 返回 (dev, inode)。轮转判定必须用 inode，不能只看路径。
func statIdent(fi os.FileInfo) (dev, inode uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), st.Ino
	}
	return 0, 0
}
