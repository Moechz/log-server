package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/aggregate"
	"github.com/Moechz/log-server/backend/internal/capability"
	"github.com/Moechz/log-server/backend/internal/config"
	"github.com/Moechz/log-server/backend/internal/forward"
	"github.com/Moechz/log-server/backend/internal/selflog"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = "/Volume1/@apps/log-server-for-tos/data"
	cfg.VLogsStorage = "/Volume1/@apps/log-server-for-tos/data/engine"
	caps := capability.New()
	caps.Check(nil)
	//nolint:staticcheck // nil ctx 在探测实现里是允许的（仅读本地文件）
	return New(cfg, caps, nil, aggregate.New(time.Hour), selflog.New(64), "", nil)
}

func do(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t)
	code, body := do(t, s, "GET", "/healthz", "")
	if code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("healthz 失败: %d %v", code, body)
	}
}

func TestEnvelopeShape(t *testing.T) {
	s := newTestServer(t)
	for _, p := range []string{"/api/v1/about", "/api/v1/status", "/api/v1/capabilities", "/api/v1/overview"} {
		code, body := do(t, s, "GET", p, "")
		if code != http.StatusOK {
			t.Errorf("%s 返回 %d", p, code)
			continue
		}
		if ok, _ := body["ok"].(bool); !ok {
			t.Errorf("%s 未返回 ok=true: %v", p, body)
		}
		if _, has := body["data"]; !has {
			t.Errorf("%s 缺少 data 字段", p)
		}
	}
}

func TestCapabilities_ReportsAllTen(t *testing.T) {
	s := newTestServer(t)
	code, body := do(t, s, "GET", "/api/v1/capabilities", "")
	if code != http.StatusOK {
		t.Fatal(code)
	}
	data, _ := body["data"].(map[string]any)
	items, _ := data["items"].([]any)
	if len(items) != 10 {
		t.Fatalf("应有 10 项能力，得到 %d", len(items))
	}
	for _, it := range items {
		m := it.(map[string]any)
		if m["id"] == nil || m["state"] == nil {
			t.Errorf("能力条目缺字段: %v", m)
		}
	}
}

func TestOverview_NoHooksDoesNotPanic(t *testing.T) {
	s := newTestServer(t)
	code, body := do(t, s, "GET", "/api/v1/overview", "")
	if code != http.StatusOK {
		t.Fatalf("overview 返回 %d", code)
	}
	data := body["data"].(map[string]any)
	for _, k := range []string{"counters", "storage", "forward", "receivers", "alerts", "sources", "capabilities"} {
		if _, has := data[k]; !has {
			t.Errorf("overview 缺少 %s 段", k)
		}
	}
}

func TestStorageEstimate_Math(t *testing.T) {
	s := newTestServer(t)
	// 100 EPS × 30 天 × 600B ÷ 10 = 155.52e9/... 手工核：100*86400*30*600/10
	code, body := do(t, s, "POST", "/api/v1/storage/estimate",
		`{"eps":100,"days":30,"ratio":10,"bytes_per_record":600}`)
	if code != http.StatusOK {
		t.Fatalf("estimate 返回 %d", code)
	}
	d := body["data"].(map[string]any)
	wantStored := 100.0 * 86400 * 30 * 600 / 10
	if got := d["stored_bytes"].(float64); got != wantStored {
		t.Errorf("stored_bytes = %v，期望 %v（口径必须与文档附录 G 一致）", got, wantStored)
	}
	if got := d["bytes_per_day"].(float64); got != 100.0*86400*600/10 {
		t.Errorf("bytes_per_day = %v", got)
	}
}

func TestStorageEstimate_Defaults(t *testing.T) {
	s := newTestServer(t)
	code, body := do(t, s, "POST", "/api/v1/storage/estimate", `{}`)
	if code != http.StatusOK {
		t.Fatal(code)
	}
	d := body["data"].(map[string]any)
	if d["stored_bytes"].(float64) <= 0 {
		t.Error("缺省参数也应给出正数预估（默认 300 EPS / 180 天）")
	}
}

func TestStorageEstimate_BadJSON(t *testing.T) {
	s := newTestServer(t)
	code, body := do(t, s, "POST", "/api/v1/storage/estimate", `{not-json`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应返回 400，得到 %d", code)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Error("失败响应不应 ok=true")
	}
	if _, has := body["error"]; !has {
		t.Error("失败响应应包含 error 字段")
	}
}

func TestLogs_StoreUnavailable(t *testing.T) {
	s := newTestServer(t) // Store 为 nil
	code, body := do(t, s, "GET", "/api/v1/logs?limit=5", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("存储不可用应返回 503，得到 %d", code)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Error("不应 ok=true")
	}
}

func TestForwardPut_WithoutHook(t *testing.T) {
	s := newTestServer(t) // ForwardApply 为 nil
	code, _ := do(t, s, "PUT", "/api/v1/forward/targets", `{"targets":[]}`)
	if code != http.StatusNotImplemented {
		t.Fatalf("未启用转发时应返回 501，得到 %d", code)
	}
}

func TestForwardPut_AppliesAndAssignsIDs(t *testing.T) {
	s := newTestServer(t)
	var applied []forward.Target
	s.ForwardApply = func(ts []forward.Target) error { applied = ts; return nil }
	s.ForwardStats = func() []forward.Status { return nil }

	code, body := do(t, s, "PUT", "/api/v1/forward/targets",
		`{"targets":[{"protocol":"udp","addr":"10.0.0.9:514"}]}`)
	if code != http.StatusOK {
		t.Fatalf("应成功，得到 %d %v", code, body)
	}
	if len(applied) != 1 {
		t.Fatalf("应下发 1 个目标，得到 %d", len(applied))
	}
	if applied[0].ID == "" {
		t.Error("缺 id 时应自动分配（避免前端必须生成 id）")
	}
	if applied[0].Name != "10.0.0.9:514" {
		t.Errorf("缺 name 时应回填为地址，得到 %q", applied[0].Name)
	}
}

func TestForwardPut_RejectsInvalidTarget(t *testing.T) {
	s := newTestServer(t)
	s.ForwardApply = func([]forward.Target) error { return errors.New("协议非法") }
	code, body := do(t, s, "PUT", "/api/v1/forward/targets",
		`{"targets":[{"id":"a","protocol":"smtp","addr":"x"}]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法目标应 400，得到 %d", code)
	}
	if ok, _ := body["ok"].(bool); ok {
		t.Error("不应 ok=true")
	}
}

func TestForwardTest_UnknownTarget(t *testing.T) {
	s := newTestServer(t)
	s.ForwardStats = func() []forward.Status { return nil }
	code, _ := do(t, s, "POST", "/api/v1/forward/targets/nope/test", "")
	if code != http.StatusNotFound {
		t.Fatalf("未知目标应 404，得到 %d", code)
	}
}

func TestAlerts_NoHookReturnsEmpty(t *testing.T) {
	s := newTestServer(t)
	code, body := do(t, s, "GET", "/api/v1/alerts", "")
	if code != http.StatusOK {
		t.Fatal(code)
	}
	d := body["data"].(map[string]any)
	if _, has := d["items"]; !has {
		t.Error("应始终返回 items（空数组而非 null）")
	}
}

func TestRulesDryRun_BadJSON(t *testing.T) {
	s := newTestServer(t)
	code, _ := do(t, s, "POST", "/api/v1/rules/dryrun", `nope`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400，得到 %d", code)
	}
}
