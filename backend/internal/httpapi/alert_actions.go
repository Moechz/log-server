// alert_actions.go —— FR-804：告警确认 / 静默 / 维护窗口。
package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/Moechz/log-server/backend/internal/alert"
	"github.com/Moechz/log-server/backend/internal/auth"
)

// actor 取当前操作者（TOS 会话用户名），取不到则记 "ui"。
func (s *Server) actor(r *http.Request) string {
	if u, ok := auth.FromContext(r.Context()); ok && u.Name != "" {
		return u.Name
	}
	return "ui"
}

const maxSilenceMinutes = 7 * 24 * 60 // 上限 7 天

func (s *Server) handleAlertAck(w http.ResponseWriter, r *http.Request) {
	if s.AlertEngine == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "告警操作未启用")
		return
	}
	id := r.PathValue("id")
	if !s.AlertEngine.Ack(id, s.actor(r)) {
		writeErr(w, http.StatusNotFound, "not_found", "告警不存在或已解除: "+id)
		return
	}
	s.audit(r, "alert.ack", id, "ok", "")
	writeData(w, map[string]any{"id": id, "acked": true})
}

func (s *Server) handleAlertAckAll(w http.ResponseWriter, r *http.Request) {
	if s.AlertEngine == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "告警操作未启用")
		return
	}
	n := s.AlertEngine.AckAll(s.actor(r))
	if s.Self != nil {
		s.Self.Add("info", "alert", "已确认全部活跃告警", "count", strconv.Itoa(n))
	}
	s.audit(r, "alert.ack_all", "", "ok", "count="+strconv.Itoa(n))
	writeData(w, map[string]any{"acked": n})
}

func (s *Server) handleSilenceList(w http.ResponseWriter, _ *http.Request) {
	if s.AlertEngine == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "告警操作未启用")
		return
	}
	writeData(w, map[string]any{"items": s.AlertEngine.Silences()})
}

func (s *Server) handleSilenceCreate(w http.ResponseWriter, r *http.Request) {
	if s.AlertEngine == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "告警操作未启用")
		return
	}
	var body struct {
		RuleID  string `json:"rule_id"`
		Host    string `json:"host"`
		App     string `json:"app"`
		Minutes int    `json:"minutes"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.Minutes <= 0 || body.Minutes > maxSilenceMinutes {
		body.Minutes = 60
	}
	sl := s.AlertEngine.AddSilence(alert.Silence{
		RuleID: body.RuleID, Host: body.Host, App: body.App,
		Reason: body.Reason, By: s.actor(r),
	}, body.Minutes)
	if s.Self != nil {
		s.Self.Add("info", "alert", "已添加告警静默",
			"rule", body.RuleID, "host", body.Host, "minutes", strconv.Itoa(body.Minutes))
	}
	s.audit(r, "alert.silence.create", body.RuleID, "ok",
		"host="+body.Host+" app="+body.App+" minutes="+strconv.Itoa(body.Minutes))
	writeData(w, map[string]any{"silence": sl})
}

func (s *Server) handleSilenceDelete(w http.ResponseWriter, r *http.Request) {
	if s.AlertEngine == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "告警操作未启用")
		return
	}
	id := r.PathValue("id")
	if !s.AlertEngine.RemoveSilence(id) {
		writeErr(w, http.StatusNotFound, "not_found", "静默不存在")
		return
	}
	s.audit(r, "alert.silence.delete", id, "ok", "")
	writeData(w, map[string]any{"removed": true})
}

func (s *Server) handleMaintenanceSet(w http.ResponseWriter, r *http.Request) {
	if s.AlertEngine == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "告警操作未启用")
		return
	}
	var body struct {
		Minutes int    `json:"minutes"`
		Until   string `json:"until"`
		Reason  string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	var until time.Time
	if body.Until != "" {
		if t, err := time.Parse(time.RFC3339, body.Until); err == nil {
			until = t
		}
	}
	if until.IsZero() {
		if body.Minutes <= 0 || body.Minutes > maxSilenceMinutes {
			body.Minutes = 60
		}
		until = time.Now().Add(time.Duration(body.Minutes) * time.Minute)
	}
	sl := s.AlertEngine.SetMaintenance(until, body.Reason, s.actor(r))
	if s.Self != nil {
		s.Self.Add("warn", "alert", "进入维护窗口（告警静默）", "until", until.Format(time.RFC3339))
	}
	s.audit(r, "alert.maintenance.set", "", "ok", "until="+until.Format(time.RFC3339)+" reason="+body.Reason)
	writeData(w, map[string]any{"maintenance": sl})
}

func (s *Server) handleMaintenanceClear(w http.ResponseWriter, r *http.Request) {
	if s.AlertEngine == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "告警操作未启用")
		return
	}
	cleared := s.AlertEngine.ClearMaintenance()
	if s.Self != nil && cleared {
		s.Self.Add("info", "alert", "维护窗口已结束（恢复告警）")
	}
	s.audit(r, "alert.maintenance.clear", "", "ok", "")
	writeData(w, map[string]any{"cleared": cleared})
}
