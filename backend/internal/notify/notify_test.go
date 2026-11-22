package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPassLevel(t *testing.T) {
	if !passLevel(0, 7) {
		t.Error("min=0 应放行全部")
	}
	if !passLevel(4, 2) {
		t.Error("严重告警应放行")
	}
	if passLevel(4, 6) {
		t.Error("info 不应通过 min=4")
	}
}

func TestWebhookSend(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type 错误: %s", r.Header.Get("Content-Type"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := New(nil)
	d.Apply([]Config{{Kind: "webhook", Enabled: true, URL: srv.URL}})
	d.Send(Message{Title: "t", Level: 2, Host: "h1"})

	if got["title"] != "t" || got["host"] != "h1" {
		t.Fatalf("webhook 载荷错误: %v", got)
	}
	rec := d.Recent(1)
	if len(rec) != 1 || !rec[0].OK {
		t.Fatalf("投递记录错误: %+v", rec)
	}
}

func TestWebhookFailureRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	d := New(nil)
	d.Apply([]Config{{Kind: "webhook", Enabled: true, URL: srv.URL}})
	d.Send(Message{Title: "t", Level: 2})
	rec := d.Recent(1)
	if len(rec) != 1 || rec[0].OK || rec[0].Error == "" {
		t.Fatalf("应记录失败: %+v", rec)
	}
}

func TestApplyWarnings(t *testing.T) {
	d := New(nil)
	warns := d.Apply([]Config{
		{Kind: "smtp", Enabled: true},                      // 缺 host/to
		{Kind: "webhook", Enabled: true},                   // 缺 url
		{Kind: "bogus", Enabled: true},                     // 未知类型
		{Kind: "webhook", Enabled: false, URL: "http://x"}, // 停用：不告警
	})
	if len(warns) != 3 {
		t.Fatalf("应产生 3 条告警，得到 %d: %v", len(warns), warns)
	}
}

func TestTestUnknownChannel(t *testing.T) {
	d := New(nil)
	if err := d.Test(context.Background(), "webhook"); err == nil {
		t.Fatal("未配置的通道应报错")
	}
}

func TestDispatcherLevelFilter(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(200)
	}))
	defer srv.Close()
	d := New(nil)
	d.Apply([]Config{{Kind: "webhook", Enabled: true, URL: srv.URL, MinSeverity: 3}})
	d.Send(Message{Title: "crit", Level: 2})
	d.Send(Message{Title: "info", Level: 6})
	time.Sleep(50 * time.Millisecond)
	if hits != 1 {
		t.Fatalf("级别过滤错误：hits=%d want 1", hits)
	}
}
