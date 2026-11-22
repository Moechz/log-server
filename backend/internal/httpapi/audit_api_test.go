package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Moechz/log-server/backend/internal/audit"
	"github.com/Moechz/log-server/backend/internal/auth"
)

func TestAudit_HookAndAdminAPI(t *testing.T) {
	l, err := audit.Open(audit.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := &Server{Auth: auth.New(auth.Config{Mode: auth.ModeTOS}), Audit: l}

	req := httptest.NewRequest("PUT", "/api/v1/sources", nil)
	req = req.WithContext(auth.WithUser(req.Context(), auth.User{Name: "alice", Role: auth.RoleAdmin, IsAdmin: true}))
	req.Header.Set("X-Forwarded-For", "10.0.0.9, 127.0.0.1")
	s.audit(req, "sources.put", "", "ok", "count=2")

	items := l.Recent(10)
	if len(items) != 1 || items[0].Action != "sources.put" || items[0].Actor != "alice" || items[0].IP != "10.0.0.9" {
		t.Fatalf("审计记录异常: %+v", items)
	}

	// 管理员可读
	rec := httptest.NewRecorder()
	s.adminOnly(s.handleAuditActions)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("管理员读审计应 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// operator 不可读，且会记录 admin.denied
	op := httptest.NewRequest("GET", "/api/v1/audit/actions", nil)
	op = op.WithContext(auth.WithUser(op.Context(), auth.User{Name: "bob", Role: auth.RoleOperator}))
	rec2 := httptest.NewRecorder()
	s.adminOnly(s.handleAuditActions)(rec2, op)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("operator 读审计应 403，得到 %d", rec2.Code)
	}
	if got := l.Recent(1)[0]; got.Action != "admin.denied" {
		t.Fatalf("拒绝访问应留痕，得到 %+v", got)
	}
}
