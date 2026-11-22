package receive

import (
	"net/netip"
	"testing"
	"time"
)

func TestPolicy_AllowDenyPrecedence(t *testing.T) {
	p, err := ParsePolicy([]string{"10.0.0.0/8"}, []string{"10.1.0.0/16"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.2.3.4", true},
		{"10.1.2.3", false},    // deny 优先
		{"192.168.1.1", false}, // 有 allow 列表
		{"127.0.0.1", true},    // loopback 默认允许
		{"::1", true},
	}
	for _, c := range cases {
		if got := p.Allows(netip.MustParseAddr(c.ip)); got != c.want {
			t.Errorf("%s: got %v want %v", c.ip, got, c.want)
		}
	}
}

func TestPolicy_ExplicitLoopbackDeny(t *testing.T) {
	p, err := ParsePolicy(nil, []string{"127.0.0.0/8"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Allows(netip.MustParseAddr("127.0.0.1")) {
		t.Fatal("显式 deny loopback 应生效")
	}
}

func TestPolicy_SingleIPAndInvalid(t *testing.T) {
	p, err := ParsePolicy([]string{"192.168.1.10"}, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Allows(netip.MustParseAddr("192.168.1.10")) || p.Allows(netip.MustParseAddr("192.168.1.11")) {
		t.Fatal("单个 IP 应按 /32 处理")
	}
	if _, err := ParsePolicy([]string{"not-a-cidr"}, nil, 0, 0); err == nil {
		t.Fatal("非法 CIDR 应报错")
	}
	if _, err := ParsePolicy(nil, nil, -1, 0); err == nil {
		t.Fatal("负数 EPS 应报错")
	}
}

func TestPolicy_Summary(t *testing.T) {
	p, _ := ParsePolicy([]string{"10.0.0.0/8"}, nil, 100, 1<<20)
	sm := p.Summary()
	if sm["allow_all"] != false || sm["max_eps"] != 100 || sm["max_bytes_per_sec"] != int64(1<<20) {
		t.Fatalf("summary 异常: %+v", sm)
	}
}

func TestLimiter_EPSAndBytes(t *testing.T) {
	ip := netip.MustParseAddr("10.0.0.1")
	l := newSourceLimiter(2, 0)
	if !l.allow(ip, 1) || !l.allow(ip, 1) {
		t.Fatal("前两条应通过")
	}
	if l.allow(ip, 1) {
		t.Fatal("第三条应被 EPS 限速")
	}
	if !l.allow(netip.MustParseAddr("10.0.0.2"), 1) {
		t.Fatal("不同来源应独立计数")
	}

	lb := newSourceLimiter(0, 10)
	if !lb.allow(ip, 6) || lb.allow(ip, 5) {
		t.Fatal("字节上限应生效")
	}

	l0 := newSourceLimiter(0, 0)
	for i := 0; i < 100; i++ {
		if !l0.allow(ip, 1<<20) {
			t.Fatal("0 应为不限")
		}
	}
}

func TestLimiter_WindowReset(t *testing.T) {
	ip := netip.MustParseAddr("10.0.0.1")
	l := newSourceLimiter(1, 0)
	if !l.allow(ip, 1) {
		t.Fatal("第一条通过")
	}
	if l.allow(ip, 1) {
		t.Fatal("同秒第二条拒绝")
	}
	// 手动把窗口推到过去，模拟下一秒
	l.mu.Lock()
	l.sec = time.Now().Unix() - 1
	l.mu.Unlock()
	if !l.allow(ip, 1) {
		t.Fatal("新一秒应重置")
	}
}
