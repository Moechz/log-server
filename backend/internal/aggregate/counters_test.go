package aggregate

import (
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

func sev(n int) *int { return &n }

func rec(class, kind, host, app string, sevNum int, when time.Time, raw string) *model.Record {
	return &model.Record{
		Raw: raw, IngestTime: when, EventTime: when,
		Source:       model.Source{Kind: model.SourceKind(kind)},
		ClassPrimary: class, Host: host, App: app, SeverityNumber: sev(sevNum),
	}
}

func TestCounters_AddAndTotals(t *testing.T) {
	c := New(24 * time.Hour)
	now := time.Now().UTC()

	c.Add(rec("auth", "remote_syslog", "web01", "sshd", 3, now, "xxxxx"))
	c.Add(rec("auth", "remote_syslog", "web01", "sshd", 3, now, "yyy"))
	c.Add(rec("system", "local_file", "nas", "kernel", 6, now, "z"))

	tt := c.Totals(now.Add(-time.Hour))
	if tt.Total != 3 {
		t.Fatalf("总数 = %d, 期望 3", tt.Total)
	}
	if tt.ByClass["auth"] != 2 {
		t.Errorf("auth = %d, 期望 2", tt.ByClass["auth"])
	}
	if tt.BySeverity["err"] != 2 {
		t.Errorf("err = %d, 期望 2（severity 3 应归一为 err）", tt.BySeverity["err"])
	}
	if tt.Bytes != 5+3+1 {
		t.Errorf("bytes = %d, 期望 9", tt.Bytes)
	}
}

func TestCounters_TopN(t *testing.T) {
	c := New(24 * time.Hour)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		c.Add(rec("auth", "remote_syslog", "h1", "sshd", 3, now, "x"))
	}
	for i := 0; i < 2; i++ {
		c.Add(rec("auth", "remote_syslog", "h2", "sshd", 3, now, "x"))
	}
	top := c.TopN(now.Add(-time.Hour), "host", 5)
	if len(top) != 2 || top[0].Value != "h1" || top[0].Count != 5 {
		t.Fatalf("TOP host 期望 h1=5，得到 %+v", top)
	}
}

func TestCounters_WindowEviction(t *testing.T) {
	c := New(2 * time.Hour)
	old := time.Now().UTC().Add(-10 * time.Hour)
	c.Add(rec("system", "local_file", "nas", "kernel", 6, old, "a"))
	// 触发淘汰
	c.Add(rec("system", "local_file", "nas", "kernel", 6, time.Now().UTC(), "b"))

	tt := c.Totals(time.Now().UTC().Add(-24 * time.Hour))
	if tt.Missed != 1 {
		t.Errorf("窗口外计数应记为 missed=1，得到 %d", tt.Missed)
	}
	if tt.Total != 1 {
		t.Errorf("窗口内应只剩 1 条，得到 %d", tt.Total)
	}
}

func TestCounters_Series(t *testing.T) {
	c := New(24 * time.Hour)
	now := time.Now().UTC()
	c.Add(rec("system", "local_file", "nas", "kernel", 6, now, "a"))
	c.Add(rec("system", "local_file", "nas", "kernel", 6, now, "b"))
	s := c.Series(now.Add(-time.Hour))
	if len(s) != 1 {
		t.Fatalf("应有 1 个时间桶，得到 %d", len(s))
	}
	for _, v := range s {
		if v != 2 {
			t.Errorf("桶内计数 = %d, 期望 2", v)
		}
	}
}
