// notify.go —— 通知通道配置与测试（FR-803）。
//
// 凭据红线：SMTP 口令只存于应用私有配置，GET 返回时一律脱敏为 "***"；
// PUT 收到 "***" 表示“保持原值”（避免前端回显）。
package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Moechz/log-server/backend/internal/notify"
)

func maskNotify(cfgs []notify.Config) []notify.Config {
	out := make([]notify.Config, len(cfgs))
	copy(out, cfgs)
	for i := range out {
		if out[i].SMTPPass != "" {
			out[i].SMTPPass = "***"
		}
		if out[i].Headers != nil {
			h := make(map[string]string, len(out[i].Headers))
			for k, v := range out[i].Headers {
				h[k] = maskHeader(k, v)
			}
			out[i].Headers = h
		}
	}
	return out
}

// maskHeader 对明显是凭据的自定义头做脱敏（如 Authorization / X-Api-Key）。
func maskHeader(k, v string) string {
	lk := strings.ToLower(k)
	if strings.Contains(lk, "auth") || strings.Contains(lk, "token") ||
		strings.Contains(lk, "key") || strings.Contains(lk, "secret") {
		if v != "" {
			return "***"
		}
	}
	return v
}

func (s *Server) handleNotifyGet(w http.ResponseWriter, _ *http.Request) {
	cfgs := []notify.Config{}
	if s.NotifyConfig != nil {
		cfgs = s.NotifyConfig()
	}
	deliveries := []notify.Delivery{}
	if s.NotifyStatus != nil {
		deliveries = s.NotifyStatus()
	}
	writeData(w, map[string]any{
		"channels": maskNotify(cfgs),
		"recent":   deliveries,
		"kinds":    notify.ValidKinds(),
	})
}

func (s *Server) handleNotifyPut(w http.ResponseWriter, r *http.Request) {
	if s.NotifyApply == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "通知功能未启用")
		return
	}
	var body struct {
		Channels []notify.Config `json:"channels"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 512<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "请求体解析失败: "+err.Error())
		return
	}
	// 保留被脱敏的字段（口令 / 凭据类头）
	oldPass := map[string]string{}
	oldHdr := map[string]map[string]string{}
	if s.NotifyConfig != nil {
		for _, c := range s.NotifyConfig() {
			k := strings.ToLower(c.Kind)
			oldPass[k] = c.SMTPPass
			oldHdr[k] = c.Headers
		}
	}
	for i := range body.Channels {
		k := strings.ToLower(body.Channels[i].Kind)
		if body.Channels[i].SMTPPass == "***" {
			body.Channels[i].SMTPPass = oldPass[k]
		}
		hdrs := body.Channels[i].Headers
		for hk, hv := range hdrs {
			if hv == "***" {
				if prev, ok := oldHdr[k]; ok {
					if v, ok2 := prev[hk]; ok2 {
						hdrs[hk] = v
					}
				}
			}
		}
	}
	if err := s.NotifyApply(body.Channels); err != nil {
		writeErr(w, http.StatusBadRequest, "apply_failed", err.Error())
		return
	}
	if s.Self != nil {
		s.Self.Add("info", "notify", "通知通道配置已更新", "count", strconv.Itoa(len(body.Channels)))
	}
	s.audit(r, "notify.put", "", "ok", "count="+strconv.Itoa(len(body.Channels)))
	writeData(w, map[string]any{"channels": maskNotify(body.Channels)})
}

func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if s.NotifyTest == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "通知功能未启用")
		return
	}
	var body struct {
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if err := s.NotifyTest(r.Context(), body.Kind); err != nil {
		s.audit(r, "notify.test", body.Kind, "error", err.Error())
		writeErr(w, http.StatusBadRequest, "test_failed", err.Error())
		return
	}
	s.audit(r, "notify.test", body.Kind, "ok", "测试消息已发送")
	writeData(w, map[string]any{"ok": true, "message": "测试消息已发送"})
}
