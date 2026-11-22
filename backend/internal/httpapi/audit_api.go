package httpapi

import (
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/Moechz/log-server/backend/internal/audit"
	"github.com/Moechz/log-server/backend/internal/auth"
)

// audit 记录一次管理操作（FR-1104 / NFR-S5）。
// 失败不影响业务；审计日志自己会降级并在诊断包/页面显示 write_errors。
func (s *Server) audit(r *http.Request, action, target, result, detail string) {
	if s.Audit == nil {
		return
	}
	e := audit.Entry{
		Action: action, Target: target, Result: result, Detail: detail,
		IP: clientIP(r), Method: r.Method, Path: r.URL.Path,
	}
	if u, ok := auth.FromContext(r.Context()); ok {
		e.Actor, e.Role, e.Source = u.Name, string(u.Role), u.Source
	} else {
		e.Actor = "unknown"
	}
	s.Audit.Append(e)
}

// clientIP 取真实来源：优先 X-Forwarded-For（平台 nginx 会设置），否则 RemoteAddr。
func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			xff = xff[:i]
		}
		if ip := strings.TrimSpace(xff); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleAuditActions 返回最近的管理操作（仅管理员）。
func (s *Server) handleAuditActions(w http.ResponseWriter, r *http.Request) {
	if s.Audit == nil {
		writeErr(w, http.StatusServiceUnavailable, "audit_unavailable",
			"管理操作留痕未启用（检查数据目录可写性）")
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 500 {
		n = 100
	}
	writeData(w, map[string]any{
		"items": s.Audit.Recent(n),
		"stats": s.Audit.Stats(),
	})
}
