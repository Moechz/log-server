package httpapi

import (
	"net/http"

	"github.com/Moechz/log-server/backend/internal/auth"
)

// handleOnboarding 返回首次使用向导状态 + 当前配置进度（FR-1005）。
func (s *Server) handleOnboarding(w http.ResponseWriter, _ *http.Request) {
	if s.OnboardingGet == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "首次向导未启用")
		return
	}
	hints := map[string]any{}
	if s.Sources != nil {
		n := 0
		for _, x := range s.Sources() {
			if k, _ := x["kind"].(string); k != "journald" {
				n++
			}
		}
		hints["file_sources"] = n
	}
	if s.ForwardStats != nil {
		hints["forward_targets"] = len(s.ForwardStats())
	}
	if s.NotifyConfig != nil {
		hints["notify_channels"] = len(s.NotifyConfig())
	}
	writeData(w, map[string]any{
		"state":          s.OnboardingGet(),
		"hints":          hints,
		"retention_days": s.Cfg.RetentionDays,
		"data_dir":       s.Cfg.DataDir,
	})
}

// handleOnboardingComplete 标记向导完成（仅管理员）。
func (s *Server) handleOnboardingComplete(w http.ResponseWriter, r *http.Request) {
	if s.OnboardingComplete == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "首次向导未启用")
		return
	}
	actor := "unknown"
	if u, ok := auth.FromContext(r.Context()); ok {
		actor = u.Name
	}
	if err := s.OnboardingComplete(actor); err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	s.audit(r, "onboarding.complete", "", "ok", "actor="+actor)
	writeData(w, map[string]any{"completed": true})
}

// handleOnboardingReset 重新运行向导（仅管理员；删除完成标记）。
func (s *Server) handleOnboardingReset(w http.ResponseWriter, r *http.Request) {
	if s.OnboardingReset == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "首次向导未启用")
		return
	}
	if err := s.OnboardingReset(); err != nil {
		writeErr(w, http.StatusInternalServerError, "reset_failed", err.Error())
		return
	}
	s.audit(r, "onboarding.reset", "", "ok", "重新运行首次向导")
	writeData(w, map[string]any{"completed": false})
}
