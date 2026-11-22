package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Moechz/log-server/backend/internal/auth"
)

func callAdminOnly(t *testing.T, s *Server, u *auth.User) (int, bool) {
	t.Helper()
	called := false
	h := s.adminOnly(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest("PUT", "/api/v1/sources", nil)
	if u != nil {
		req = req.WithContext(auth.WithUser(req.Context(), *u))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code, called
}

func TestAdminOnly_OperatorForbidden(t *testing.T) {
	s := &Server{Auth: auth.New(auth.Config{Mode: auth.ModeTOS})}
	code, called := callAdminOnly(t, s, &auth.User{Role: auth.RoleOperator})
	if code != http.StatusForbidden || called {
		t.Fatalf("operator 应被 403 拒绝且不执行处理函数，得到 code=%d called=%v", code, called)
	}
}

func TestAdminOnly_AdminPasses(t *testing.T) {
	s := &Server{Auth: auth.New(auth.Config{Mode: auth.ModeTOS})}
	code, called := callAdminOnly(t, s, &auth.User{Role: auth.RoleAdmin, IsAdmin: true})
	if code != http.StatusNoContent || !called {
		t.Fatalf("admin 应通过，得到 code=%d called=%v", code, called)
	}
}

func TestAdminOnly_NoAuthKeepsLegacy(t *testing.T) {
	// 单测/嵌入式用法没有 auth 中间件时保持旧行为（否则所有 handler 测试都要改造）。
	s := &Server{}
	code, called := callAdminOnly(t, s, nil)
	if code != http.StatusNoContent || !called {
		t.Fatalf("无 auth 时应放行，得到 code=%d called=%v", code, called)
	}
}
