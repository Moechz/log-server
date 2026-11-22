//go:build !unix

package file

import "os"

// statIdent 非 unix 平台的退化实现（本项目只发布 linux，此文件仅保证可编译）。
func statIdent(fi os.FileInfo) (dev, inode uint64) {
	return 0, uint64(fi.ModTime().UnixNano())
}
