package httpapi

import (
	"net/http"
	"testing"
)

func TestSourcesRequireHooks(t *testing.T) {
	s := newTestServer(t)
	if code, _ := do(t, s, "PUT", "/api/v1/sources", `{"sources":[]}`); code != http.StatusNotImplemented {
		t.Errorf("未注入钩子时 PUT /sources 应 501，得到 %d", code)
	}
	if code, _ := do(t, s, "POST", "/api/v1/ingest/pause", ""); code != http.StatusNotImplemented {
		t.Errorf("未注入钩子时 pause 应 501，得到 %d", code)
	}
}

func TestSourcesPutValidatesPath(t *testing.T) {
	s := newTestServer(t)
	var got []SourceSpec
	s.SourcesApply = func(specs []SourceSpec) error { got = specs; return nil }

	code, _ := do(t, s, "PUT", "/api/v1/sources", `{"sources":[{"path":"relative/log","enabled":true}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("相对路径应 400，得到 %d", code)
	}

	code, body := do(t, s, "PUT", "/api/v1/sources",
		`{"sources":[{"path":"/var/log/a.log","enabled":true,"multiline":true},{"path":"/var/log/a.log"},{"path":" "}]}`)
	if code != http.StatusOK {
		t.Fatalf("应 200，得到 %d %v", code, body)
	}
	if len(got) != 1 || got[0].Path != "/var/log/a.log" || !got[0].Multiline {
		t.Fatalf("去重/字段错误: %+v", got)
	}
}

func TestIngestPauseToggle(t *testing.T) {
	s := newTestServer(t)
	paused := false
	s.IngestPause = func(p bool) { paused = p }
	s.IngestPaused = func() bool { return paused }

	if code, _ := do(t, s, "POST", "/api/v1/ingest/pause", ""); code != http.StatusOK {
		t.Fatalf("pause 应 200，得到 %d", code)
	}
	if !paused {
		t.Fatal("应处于暂停")
	}
	if code, _ := do(t, s, "POST", "/api/v1/ingest/resume", ""); code != http.StatusOK {
		t.Fatalf("resume 应 200，得到 %d", code)
	}
	if paused {
		t.Fatal("应已恢复")
	}
}
