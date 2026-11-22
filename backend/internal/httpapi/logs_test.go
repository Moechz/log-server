package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestParseInterval(t *testing.T) {
	cases := map[string]time.Duration{
		"":    0,
		"60s": time.Minute,
		"5m":  5 * time.Minute,
		"1h":  time.Hour,
		"7d":  7 * 24 * time.Hour,
		"xx":  0,
	}
	for in, want := range cases {
		if got := parseInterval(in); got != want {
			t.Errorf("parseInterval(%q)=%v want %v", in, got, want)
		}
	}
}

func TestAutoInterval(t *testing.T) {
	if got := autoInterval(time.Hour); got != time.Minute {
		t.Errorf("1h → %v want 1m", got)
	}
	if got := autoInterval(24 * time.Hour); got != time.Hour {
		t.Errorf("24h → %v want 1h", got)
	}
	if got := autoInterval(7 * 24 * time.Hour); got != 6*time.Hour {
		t.Errorf("7d → %v want 6h", got)
	}
}

func TestLogQueryFromRequest(t *testing.T) {
	req := httptest.NewRequest("GET",
		"/api/v1/logs?q=oom&host=h1&app=sshd&class=auth&min_severity=3&limit=50"+
			"&from=2026-10-14T00:00:00Z&to=2026-10-14T01:00:00Z", nil)
	sq := logQueryFromRequest(req, 0)
	if sq.Text != "oom" || len(sq.Hosts) != 1 || sq.Hosts[0] != "h1" ||
		len(sq.Apps) != 1 || len(sq.Classes) != 1 || sq.Limit != 50 {
		t.Fatalf("解析错误: %+v", sq)
	}
	if sq.MinSeverity == nil || *sq.MinSeverity != 3 {
		t.Fatalf("min_severity 解析错误: %+v", sq.MinSeverity)
	}
	if sq.Range.From.IsZero() || sq.Range.To.IsZero() {
		t.Fatalf("时间范围解析错误: %+v", sq.Range)
	}
}

func TestLogsEndpointsRequireStore(t *testing.T) {
	s := newTestServer(t) // Store 为 nil
	for _, path := range []string{"/api/v1/logs/histogram", "/api/v1/logs/context?t=2026-10-14T00:00:00Z"} {
		code, _ := do(t, s, "GET", path, "")
		if code != http.StatusServiceUnavailable {
			t.Errorf("%s 应返回 503，得到 %d", path, code)
		}
	}
}
