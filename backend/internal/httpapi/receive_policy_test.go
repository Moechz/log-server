package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Moechz/log-server/backend/internal/auth"
	"github.com/Moechz/log-server/backend/internal/receive"
)

func TestReceivePolicy_PutValidatesAndApplies(t *testing.T) {
	var got receive.Policy
	s := &Server{
		Auth:          auth.New(auth.Config{Mode: auth.ModeTOS}),
		ReceivePolicy: func() map[string]any { return got.Summary() },
		ReceiveApply:  func(p receive.Policy) error { got = p; return nil },
	}
	req := httptest.NewRequest("PUT", "/api/v1/receive/policy",
		strings.NewReader(`{"allow":["10.0.0.0/8"],"deny":["10.1.0.0/16"],"max_eps":100,"max_bytes_per_sec":1048576}`))
	req = req.WithContext(auth.WithUser(req.Context(), auth.User{Name: "admin", Role: auth.RoleAdmin, IsAdmin: true}))
	rec := httptest.NewRecorder()
	s.adminOnly(s.handleReceivePolicyPut)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if got.MaxEPS != 100 || got.MaxBPS != 1<<20 || len(got.Allow) != 1 || len(got.Deny) != 1 {
		t.Fatalf("策略未应用: %+v", got)
	}

	bad := httptest.NewRequest("PUT", "/api/v1/receive/policy", strings.NewReader(`{"allow":["not-a-cidr"]}`))
	bad = bad.WithContext(auth.WithUser(bad.Context(), auth.User{Name: "admin", Role: auth.RoleAdmin, IsAdmin: true}))
	rec2 := httptest.NewRecorder()
	s.adminOnly(s.handleReceivePolicyPut)(rec2, bad)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("非法 CIDR 应 400，得到 %d", rec2.Code)
	}
}

func TestReceivePolicy_Get(t *testing.T) {
	s := &Server{
		Auth:          auth.New(auth.Config{Mode: auth.ModeTOS}),
		ReceivePolicy: func() map[string]any { return receive.Policy{MaxEPS: 5}.Summary() },
	}
	req := httptest.NewRequest("GET", "/api/v1/receive/policy", nil)
	req = req.WithContext(auth.WithUser(req.Context(), auth.User{Role: auth.RoleOperator}))
	rec := httptest.NewRecorder()
	s.handleReceivePolicyGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET 应 200，得到 %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["data"].(map[string]any)["policy"].(map[string]any)["max_eps"].(float64) != 5 {
		t.Fatalf("GET 内容异常: %s", rec.Body.String())
	}
}
