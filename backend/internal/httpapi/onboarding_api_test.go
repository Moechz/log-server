package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Moechz/log-server/backend/internal/auth"
	"github.com/Moechz/log-server/backend/internal/onboarding"
)

func TestOnboardingFlow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onboarding.json")
	s := &Server{
		Auth:               auth.New(auth.Config{Mode: auth.ModeTOS}),
		OnboardingGet:      func() onboarding.State { return onboarding.Load(path) },
		OnboardingComplete: func(actor string) error { return onboarding.Complete(path, actor) },
		OnboardingReset:    func() error { return onboarding.Reset(path) },
	}
	admin := func() *http.Request {
		r := httptest.NewRequest("POST", "/api/v1/onboarding/complete", nil)
		return r.WithContext(auth.WithUser(r.Context(), auth.User{Name: "alice", Role: auth.RoleAdmin, IsAdmin: true}))
	}

	// 初始未完成
	rec := httptest.NewRecorder()
	s.handleOnboarding(rec, httptest.NewRequest("GET", "/api/v1/onboarding", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	state := got["data"].(map[string]any)["state"].(map[string]any)
	if state["completed"] != false {
		t.Fatalf("初始应未完成: %v", state)
	}

	// operator 不能标记完成
	op := httptest.NewRequest("POST", "/api/v1/onboarding/complete", nil)
	op = op.WithContext(auth.WithUser(op.Context(), auth.User{Name: "bob", Role: auth.RoleOperator}))
	rec2 := httptest.NewRecorder()
	s.adminOnly(s.handleOnboardingComplete)(rec2, op)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("operator 应 403，得到 %d", rec2.Code)
	}

	// admin 完成
	rec3 := httptest.NewRecorder()
	s.adminOnly(s.handleOnboardingComplete)(rec3, admin())
	if rec3.Code != http.StatusOK {
		t.Fatalf("完成应 200，得到 %d: %s", rec3.Code, rec3.Body.String())
	}
	if st := onboarding.Load(path); !st.Completed || st.CompletedBy != "alice" {
		t.Fatalf("状态未保存: %+v", st)
	}

	// reset
	rec4 := httptest.NewRecorder()
	s.adminOnly(s.handleOnboardingReset)(rec4, admin())
	if rec4.Code != http.StatusOK || onboarding.Load(path).Completed {
		t.Fatalf("重置失败: code=%d state=%+v", rec4.Code, onboarding.Load(path))
	}
}
