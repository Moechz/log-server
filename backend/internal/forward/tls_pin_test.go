package forward

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 生成自签证书，返回 cert/key 路径与该证书的 sha256 指纹（TC-06 用）。
func selfSigned(t *testing.T) (certPath, keyPath, fingerprint string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "forward-test"},
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
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	dir := t.TempDir()
	certPath = filepath.Join(dir, "server.crt")
	keyPath = filepath.Join(dir, "server.key")
	cf, _ := os.Create(certPath)
	_ = pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	cf.Close()
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	kf, _ := os.Create(keyPath)
	_ = pem.Encode(kf, &pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	kf.Close()
	return certPath, keyPath, hex.EncodeToString(sum[:])
}

// TC-06：自签证书 + 指纹固定 —— 指纹相符则连接成功，不符则拒绝并报错。
func TestBuildTLS_PinnedFingerprint(t *testing.T) {
	cert, key, fp := selfSigned(t)

	// 指纹相符：应可完成 TLS 握手
	cfg, err := buildTLS(TLSOptions{PinnedSHA256: fp})
	if err != nil {
		t.Fatalf("buildTLS 失败: %v", err)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("指纹固定时应设置 VerifyPeerCertificate")
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", mustServerTLS(t, cert, key))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// 接受循环 + 重试：全量测试并行时偶发握手竞争，单次失败不应误报（最多试 3 次）。
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).Handshake()
				_ = c.Close()
			}()
		}
	}()
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	var c *tls.Conn
	var derr error
	for i := 0; i < 3; i++ {
		c, derr = tls.DialWithDialer(dialer, "tcp", ln.Addr().String(), cfg)
		if derr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if derr != nil {
		t.Fatalf("指纹相符时握手应成功，得到: %v", derr)
	}
	_ = c.Close()

	// 指纹不符：必须被拒绝
	badCfg, err := buildTLS(TLSOptions{PinnedSHA256: "00" + fp[2:]})
	if err != nil {
		t.Fatal(err)
	}
	ln2, err := tls.Listen("tcp", "127.0.0.1:0", mustServerTLS(t, cert, key))
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	go func() {
		conn, err := ln2.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 1)
		_, _ = conn.Read(buf) // 触发对端校验与告警
		_ = conn.Close()
	}()
	if c2, err := tls.Dial("tcp", ln2.Addr().String(), badCfg); err == nil {
		_ = c2.Close()
		t.Fatal("指纹不符时必须拒绝连接")
	} else if !strings.Contains(err.Error(), "指纹") && !strings.Contains(err.Error(), "certificate") {
		t.Logf("拒绝原因（实现相关，可接受）: %v", err)
	}
}

func mustServerTLS(t *testing.T, cert, key string) *tls.Config {
	t.Helper()
	c, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{c}, MinVersion: tls.VersionTLS12}
}
