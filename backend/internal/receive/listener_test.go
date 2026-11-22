package receive

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("取空闲 UDP 端口失败: %v", err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// writeSelfSigned 生成自签证书（测试用；不落系统目录，只落 t.TempDir）。
func writeSelfSigned(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "logserver-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("签发证书失败: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	cf, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	kf, err := os.Create(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	kf.Close()
	return certPath, keyPath
}

func TestServer_TLS_Delivers(t *testing.T) {
	cert, key := writeSelfSigned(t)
	port := freePort(t)
	raws := make(chan string, 4)
	srcs := make(chan SourceInfo, 4)

	s := New(Options{
		TLSAddr:     fmt.Sprintf("127.0.0.1:%d", port),
		TLSCertFile: cert,
		TLSKeyFile:  key,
		Handler: func(raw []byte, src SourceInfo) {
			raws <- string(raw)
			srcs <- src
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer s.Close()
	if !s.TLSEnabled() {
		t.Fatal("TLS 监听应当已启用")
	}

	conn, err := tls.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port),
		&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("TLS 连接失败: %v", err)
	}
	defer conn.Close()
	msg := "<134>1 2026-10-13T00:00:00Z nas app 1 - - tls hello\n"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	select {
	case got := <-raws:
		if got != "<134>1 2026-10-13T00:00:00Z nas app 1 - - tls hello" {
			t.Errorf("原文不一致: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("3s 内未收到 TLS 消息")
	}
	select {
	case src := <-srcs:
		if src.Protocol != "tls" {
			t.Errorf("来源协议应为 tls，实际 %q", src.Protocol)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到来源信息")
	}
	if st := s.Stats(); st.TLSConns == 0 {
		t.Error("TLSConns 计数应为非 0")
	}
}

func TestServer_TLS_WithoutCertDoesNotBlockStart(t *testing.T) {
	s := New(Options{TLSAddr: fmt.Sprintf("127.0.0.1:%d", freePort(t))})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("证书缺失时不应导致启动失败（D-014 降级口径）: %v", err)
	}
	defer s.Close()
	if s.TLSEnabled() {
		t.Error("未配置证书时 TLS 不应启用")
	}
}

func TestServer_TLS_BadCertDoesNotBlockStart(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.crt")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := New(Options{
		TLSAddr:     fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		TLSCertFile: bad,
		TLSKeyFile:  bad,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("坏证书不应导致启动失败: %v", err)
	}
	defer s.Close()
	if s.TLSEnabled() {
		t.Error("坏证书时 TLS 不应启用")
	}
}

func TestServer_UDPAndTCP_Deliver(t *testing.T) {
	uport := freeUDPPort(t)
	tport := freePort(t)
	got := make(chan SourceInfo, 8)

	s := New(Options{
		UDPAddr: fmt.Sprintf("127.0.0.1:%d", uport),
		TCPAddr: fmt.Sprintf("127.0.0.1:%d", tport),
		Handler: func(_ []byte, src SourceInfo) { got <- src },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	defer s.Close()

	// UDP
	uc, err := net.Dial("udp", fmt.Sprintf("127.0.0.1:%d", uport))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = uc.Write([]byte("<134>Oct 13 00:00:00 nas app: udp hello"))
	uc.Close()

	// TCP
	tc, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", tport))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = tc.Write([]byte("<134>Oct 13 00:00:00 nas app: tcp hello\n"))
	tc.Close()

	seen := map[string]bool{}
	deadline := time.After(4 * time.Second)
	for len(seen) < 2 {
		select {
		case src := <-got:
			seen[src.Protocol] = true
		case <-deadline:
			t.Fatalf("未同时收到 udp 与 tcp（已收到 %v）", seen)
		}
	}
}
