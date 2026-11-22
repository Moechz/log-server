package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Moechz/log-server/backend/internal/receive"
)

// handleReceivePolicyGet 返回当前接收准入/限速策略（FR-603/604）。
func (s *Server) handleReceivePolicyGet(w http.ResponseWriter, _ *http.Request) {
	if s.ReceivePolicy == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "接收策略未启用")
		return
	}
	writeData(w, map[string]any{"policy": s.ReceivePolicy()})
}

// handleReceivePolicyPut 保存并**热生效**（无需重启；只影响之后的消息）。
func (s *Server) handleReceivePolicyPut(w http.ResponseWriter, r *http.Request) {
	if s.ReceiveApply == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "接收策略未启用")
		return
	}
	var body struct {
		Allow          []string `json:"allow"`
		Deny           []string `json:"deny"`
		MaxEPS         int      `json:"max_eps"`
		MaxBytesPerSec int64    `json:"max_bytes_per_sec"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "请求体解析失败: "+err.Error())
		return
	}
	p, err := receive.ParsePolicy(body.Allow, body.Deny, body.MaxEPS, body.MaxBytesPerSec)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_policy", err.Error())
		return
	}
	if err := s.ReceiveApply(p); err != nil {
		writeErr(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	s.audit(r, "receive.policy.put", "", "ok",
		fmt.Sprintf("allow=%d deny=%d max_eps=%d max_bps=%d", len(p.Allow), len(p.Deny), p.MaxEPS, p.MaxBPS))
	writeData(w, map[string]any{"policy": p.Summary()})
}
