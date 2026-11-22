//go:build unix

package httpapi

import (
	"fmt"
	"net"
	"time"
)

// probeTCP 做一次 TCP 连接探测（连通性测试用）。
func probeTCP(addr string) (bool, string) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false, "TCP 不可达: " + err.Error()
	}
	_ = c.Close()
	return true, fmt.Sprintf("TCP 连接成功（%s）", addr)
}
