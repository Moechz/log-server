package receive

import (
	"net/netip"
	"sync"
	"time"
)

// maxTrackedSources 防止 UDP 源地址伪造把限速表打爆（超限的新来源直接按"限速"处理）。
const maxTrackedSources = 10000

type usage struct {
	count int
	bytes int64
}

// sourceLimiter 是**每来源 IP、每秒**的固定窗口限速器（FR-604）。
type sourceLimiter struct {
	mu   sync.Mutex
	eps  int
	bps  int64
	sec  int64
	seen map[netip.Addr]*usage
}

func newSourceLimiter(eps int, bps int64) *sourceLimiter {
	return &sourceLimiter{eps: eps, bps: bps, seen: map[netip.Addr]*usage{}}
}

func (l *sourceLimiter) set(eps int, bps int64) {
	l.mu.Lock()
	l.eps, l.bps = eps, bps
	l.seen = map[netip.Addr]*usage{}
	l.sec = 0
	l.mu.Unlock()
}

// allow 记录一条消息；返回 false 表示本秒内该来源超限。
func (l *sourceLimiter) allow(ip netip.Addr, n int64) bool {
	ip = ip.Unmap()
	now := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.eps <= 0 && l.bps <= 0 {
		return true
	}
	if now != l.sec {
		l.sec = now
		clear(l.seen)
	}
	u := l.seen[ip]
	if u == nil {
		if len(l.seen) >= maxTrackedSources {
			return false
		}
		u = &usage{}
		l.seen[ip] = u
	}
	if l.eps > 0 && u.count >= l.eps {
		return false
	}
	if l.bps > 0 && u.bytes+n > l.bps {
		return false
	}
	u.count++
	u.bytes += n
	return true
}
