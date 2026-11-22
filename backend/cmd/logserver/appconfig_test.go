package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
)

// TestAppConfig_ReceivePolicyRoundTrip 守住 FR-603/604 的设置能落盘并在重启后加载。
func TestAppConfig_ReceivePolicyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	in := appConfig{Receive: &receivePolicy{
		Allow: []string{"192.168.1.0/24"}, Deny: []string{"192.168.1.9/32"},
		MaxEPS: 42, MaxBytesPerSec: 1 << 20,
	}}
	if err := saveAppConfig(path, in, lg); err != nil {
		t.Fatal(err)
	}
	out := loadAppConfig(path, lg)
	if out.Receive == nil || !reflect.DeepEqual(out.Receive, in.Receive) {
		t.Fatalf("receive_policy 未往返: got %+v want %+v", out.Receive, in.Receive)
	}
}
