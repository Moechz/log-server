// netinfo.go —— 本机网络信息（只读）：供「接收」页告诉用户“发送方该填什么地址”。
//
// 为什么需要：接收数据的地址 ≠ 用户浏览管理界面的地址（后者可能带端口/域名）。
// 这里枚举真正的局域网 IPv4，排除 loopback 与虚拟网桥/Docker 接口。
package httpapi

import (
	"net"
	"net/http"
	"os"
	"strings"
)

// virtualIface 判断是否为不应对局域网设备暴露的虚拟接口。
func virtualIface(name string) bool {
	for _, p := range []string{
		"docker", "veth", "br-", "virbr", "tun", "tap", "tailscale", "wg", "zt", "flannel", "cni", "kube",
	} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (s *Server) handleNetwork(w http.ResponseWriter, _ *http.Request) {
	host, _ := os.Hostname()

	type ifaceAddr struct {
		Iface string `json:"iface"`
		IP    string `json:"ip"`
	}
	var out []ifaceAddr

	ifs, err := net.Interfaces()
	if err != nil {
		writeData(w, map[string]any{"hostname": host, "addresses": []ifaceAddr{}})
		return
	}
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			out = append(out, ifaceAddr{Iface: ifc.Name, IP: ip4.String()})
		}
	}
	writeData(w, map[string]any{"hostname": host, "addresses": out})
}
