package receive

import (
	"fmt"
	"net/netip"
	"strings"
)

// Policy 是接收面的准入与限速策略（FR-603/604）。
//
// 语义：
//   - Deny 优先于一切（包括 loopback）；
//   - Allow 为空 = 接受所有来源；非空 = 只接受列表内网段；
//   - loopback 默认允许（本机 logger / 验证脚本）；显式 Deny 127.0.0.0/8 可关闭；
//   - MaxEPS / MaxBytesPerSec 是**每来源 IP** 的每秒上限，0 = 不限。
type Policy struct {
	Allow  []netip.Prefix
	Deny   []netip.Prefix
	MaxEPS int
	MaxBPS int64
}

// ParsePolicy 把配置里的字符串 CIDR（也接受单个 IP）解析为 Policy。
func ParsePolicy(allow, deny []string, maxEPS int, maxBPS int64) (Policy, error) {
	if maxEPS < 0 {
		return Policy{}, fmt.Errorf("每来源 EPS 上限不能为负数")
	}
	if maxBPS < 0 {
		return Policy{}, fmt.Errorf("每来源字节/秒上限不能为负数")
	}
	p := Policy{MaxEPS: maxEPS, MaxBPS: maxBPS}
	for _, s := range allow {
		pf, err := parsePrefix(s)
		if err != nil {
			return Policy{}, fmt.Errorf("allow 网段 %q 无效: %w", s, err)
		}
		p.Allow = append(p.Allow, pf)
	}
	for _, s := range deny {
		pf, err := parsePrefix(s)
		if err != nil {
			return Policy{}, fmt.Errorf("deny 网段 %q 无效: %w", s, err)
		}
		p.Deny = append(p.Deny, pf)
	}
	return p, nil
}

func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Prefix{}, fmt.Errorf("空网段")
	}
	if strings.Contains(s, "/") {
		pf, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		return pf.Masked(), nil
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	ip = ip.Unmap()
	return netip.PrefixFrom(ip, ip.BitLen()), nil
}

// Allows 判断来源 IP 是否允许接入（Deny > loopback > Allow 空/命中）。
func (p Policy) Allows(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true // 解析不出 IP 时不因策略丢数据（上层仍会记录来源字符串）
	}
	ip = ip.Unmap()
	for _, d := range p.Deny {
		if d.Contains(ip) {
			return false
		}
	}
	if ip.IsLoopback() {
		return true
	}
	if len(p.Allow) == 0 {
		return true
	}
	for _, a := range p.Allow {
		if a.Contains(ip) {
			return true
		}
	}
	return false
}

// Summary 返回给 UI/API 的字符串化策略。
func (p Policy) Summary() map[string]any {
	allow := make([]string, 0, len(p.Allow))
	for _, x := range p.Allow {
		allow = append(allow, x.String())
	}
	deny := make([]string, 0, len(p.Deny))
	for _, x := range p.Deny {
		deny = append(deny, x.String())
	}
	return map[string]any{
		"allow": allow, "deny": deny,
		"max_eps": p.MaxEPS, "max_bytes_per_sec": p.MaxBPS,
		"allow_all": len(p.Allow) == 0,
	}
}
