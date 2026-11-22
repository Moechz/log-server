package receive

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

func udpSend(t *testing.T, port int, msg string) {
	t.Helper()
	c, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte(msg))
}

func TestServer_UDP_ACLDenied(t *testing.T) {
	port := freeUDPPort(t)
	raws := make(chan string, 4)
	p, err := ParsePolicy(nil, []string{"127.0.0.0/8"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{
		UDPAddr: fmt.Sprintf("127.0.0.1:%d", port), Policy: p,
		Handler: func(raw []byte, _ SourceInfo) { raws <- string(raw) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	udpSend(t, port, "<14>should-be-dropped")
	select {
	case m := <-raws:
		t.Fatalf("被 ACL 拒绝的消息不应交付: %q", m)
	case <-time.After(200 * time.Millisecond):
	}
	if st := s.Stats(); st.ACLDenied == 0 {
		t.Fatalf("应统计 ACL 拒绝: %+v", st)
	}
}

func TestServer_UDP_RateLimited(t *testing.T) {
	port := freeUDPPort(t)
	raws := make(chan string, 16)
	p, err := ParsePolicy(nil, nil, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{
		UDPAddr: fmt.Sprintf("127.0.0.1:%d", port), Policy: p,
		Handler: func(raw []byte, _ SourceInfo) { raws <- string(raw) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 10; i++ {
		udpSend(t, port, fmt.Sprintf("<14>rate-%d", i))
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(raws); n > 2 {
		t.Fatalf("每来源 EPS=1，本秒最多 1-2 条，实际交付 %d", n)
	}
	if st := s.Stats(); st.RateLimited == 0 {
		t.Fatalf("应统计限速: %+v", st)
	}
}
