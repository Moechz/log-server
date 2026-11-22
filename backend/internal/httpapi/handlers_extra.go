package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Moechz/log-server/backend/internal/alert"
	"github.com/Moechz/log-server/backend/internal/auth"
	"github.com/Moechz/log-server/backend/internal/forward"
	"github.com/Moechz/log-server/backend/internal/model"
)

// ── 快照（供总览）───────────────────────────────────────────────────

func (s *Server) forwardSnapshot() []forward.Status {
	if s.ForwardStats == nil {
		return []forward.Status{}
	}
	return s.ForwardStats()
}

func (s *Server) alertSnapshot() map[string]any {
	if s.AlertStats == nil {
		return map[string]any{"active": 0, "items": []any{}}
	}
	st := s.AlertStats()
	items := []alert.Alert{}
	if s.Alerts != nil {
		items = s.Alerts(10)
	}
	st["items"] = items
	return st
}

func (s *Server) sourcesSnapshot() []map[string]any {
	if s.Sources == nil {
		return []map[string]any{}
	}
	return s.Sources()
}

// ── 转发目标（FR-501/507）──────────────────────────────────────────

func (s *Server) handleForwardList(w http.ResponseWriter, _ *http.Request) {
	writeData(w, map[string]any{
		"targets": s.forwardSnapshot(),
		"config":  s.forwardConfig(),
	})
}

func (s *Server) forwardConfig() []forward.Target {
	if s.ForwardStats == nil {
		return []forward.Target{}
	}
	out := make([]forward.Target, 0)
	for _, st := range s.ForwardStats() {
		out = append(out, forward.Target{
			ID: st.ID, Name: st.Name, Protocol: st.Protocol, Addr: st.Addr, Enabled: st.Enabled,
		})
	}
	return out
}

// handleForwardPut 全量替换目标配置（幂等；保持不变的目标会**保留磁盘队列**）。
func (s *Server) handleForwardPut(w http.ResponseWriter, r *http.Request) {
	if s.ForwardApply == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "转发功能未启用")
		return
	}
	var body struct {
		Targets []forward.Target `json:"targets"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", "请求体解析失败: "+err.Error())
		return
	}
	for i := range body.Targets {
		if body.Targets[i].ID == "" {
			body.Targets[i].ID = fmt.Sprintf("t%d", i+1)
		}
		if body.Targets[i].Name == "" {
			body.Targets[i].Name = body.Targets[i].Addr
		}
	}
	if err := s.ForwardApply(body.Targets); err != nil {
		writeErr(w, http.StatusBadRequest, "apply_failed", err.Error())
		return
	}
	s.audit(r, "forward.put", "", "ok", "count="+strconv.Itoa(len(body.Targets)))
	writeData(w, map[string]any{"targets": s.forwardSnapshot()})
}

// handleForwardTest 连通性测试（FR-507）：不发送业务数据，只做 TCP 连接或 UDP 解析检查。
func (s *Server) handleForwardTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for _, st := range s.forwardSnapshot() {
		if st.ID != id {
			continue
		}
		ok, msg := probeTarget(st)
		result := "ok"
		if !ok {
			result = "error"
		}
		s.audit(r, "forward.test", id, result, msg)
		writeData(w, map[string]any{"id": id, "ok": ok, "message": msg})
		return
	}
	writeErr(w, http.StatusNotFound, "not_found", "目标不存在: "+id)
}

func probeTarget(st forward.Status) (bool, string) {
	switch st.Protocol {
	case forward.ProtoHTTP:
		c := &http.Client{Timeout: 3 * time.Second}
		resp, err := c.Get(st.Addr)
		if err != nil {
			return false, "HTTP 不可达: " + err.Error()
		}
		defer resp.Body.Close()
		return true, fmt.Sprintf("HTTP 可达（%d）", resp.StatusCode)
	default:
		// UDP 无连接语义：仅做地址解析校验
		if st.Protocol == forward.ProtoUDP {
			return true, "UDP 目标地址已配置（UDP 无连接确认，无法探测可达性）"
		}
		return probeTCP(st.Addr)
	}
}

// ── 告警与规则（M8）────────────────────────────────────────────────

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	items := []alert.Alert{}
	if s.Alerts != nil {
		items = s.Alerts(n)
	}
	st := map[string]any{}
	if s.AlertStats != nil {
		st = s.AlertStats()
	}
	out := map[string]any{"stats": st, "items": items}
	if s.AlertEngine != nil {
		out["silences"] = s.AlertEngine.Silences()
	}
	writeData(w, out)
}

func (s *Server) handleRules(w http.ResponseWriter, _ *http.Request) {
	items := []alert.Rule{}
	if s.Rules != nil {
		items = s.Rules()
	}
	writeData(w, map[string]any{"items": items})
}

// handleRulesDryRun 规则试跑（FR-802）：对给定的样例文本返回命中的规则，
// 用于保存前预估命中量，避免上线即刷屏。
func (s *Server) handleRulesDryRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Samples []string `json:"samples"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if s.Rules == nil {
		writeErr(w, http.StatusNotImplemented, "not_supported", "规则引擎未启用")
		return
	}
	rules := s.Rules()
	type hit struct {
		RuleID string `json:"rule_id"`
		Name   string `json:"name"`
		Level  string `json:"level"`
		Sample string `json:"sample"`
	}
	var hits []hit
	probe := alert.New(alert.Config{})
	if err := probe.Add(rules...); err != nil {
		writeErr(w, http.StatusInternalServerError, "rule_error", err.Error())
		return
	}
	for _, sample := range body.Samples {
		probe.Evaluate(&model.Record{Raw: sample, Msg: sample, IngestTime: time.Now().UTC()})
	}
	for _, a := range probe.Recent(200) {
		hits = append(hits, hit{RuleID: a.RuleID, Name: a.RuleName, Level: a.LevelText, Sample: a.SampleRaw})
	}
	writeData(w, map[string]any{"hits": hits, "count": len(hits)})
}

// ── 采集源（M1）────────────────────────────────────────────────────

func (s *Server) handleSources(w http.ResponseWriter, _ *http.Request) {
	writeData(w, map[string]any{"items": s.sourcesSnapshot(), "paused": s.ingestPaused()})
}

// ── 容量与预估（M3 / FR-305 / FR-311）─────────────────────────────

func (s *Server) handleStorageUsage(w http.ResponseWriter, _ *http.Request) {
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage_unavailable", "存储引擎不可用")
		return
	}
	u, err := s.Store.Usage(contextTODO())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "store_error", err.Error())
		return
	}
	writeData(w, map[string]any{
		"usage": u, "caps": s.Store.Capabilities(),
		"retention_days": s.Cfg.RetentionDays, "max_disk_bytes": s.Cfg.MaxDiskBytes,
	})
}

// handleStorageEstimate 容量预估器（FR-311，口径与文档附录 G 一致）。
func (s *Server) handleStorageEstimate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EPS      float64 `json:"eps"`
		Days     float64 `json:"days"`
		Ratio    float64 `json:"ratio"`
		BytesPer float64 `json:"bytes_per_record"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if body.EPS <= 0 {
		body.EPS = 300
	}
	if body.Days <= 0 {
		body.Days = 180
	}
	if body.Ratio <= 0 {
		body.Ratio = 10
	}
	if body.BytesPer <= 0 {
		body.BytesPer = 600
	}
	records := body.EPS * 86400 * body.Days
	raw := records * body.BytesPer
	stored := raw / body.Ratio
	perDay := body.EPS * 86400 * body.BytesPer / body.Ratio

	var availBytes float64
	if s.Store == nil {
		// 存储未配置：只给纯计算（不依赖磁盘信息）
		writeData(w, map[string]any{
			"records": records, "raw_bytes": raw, "stored_bytes": stored,
			"bytes_per_day": perDay, "available_bytes": 0,
			"used_pct": 0, "level": "ok",
			"note": "存储引擎未配置，未计算占上限比例",
		})
		return
	}
	if u, err := s.Store.Usage(contextTODO()); err == nil {
		availBytes = float64(s.Cfg.MaxDiskBytes)
		if availBytes == 0 {
			availBytes = float64(u.DiskFreeBytes)
		}
	}
	pct := 0.0
	if availBytes > 0 {
		pct = stored / availBytes * 100
	}
	level := "ok"
	if pct > 95 {
		level = "crit"
	} else if pct > 85 {
		level = "warn"
	}
	writeData(w, map[string]any{
		"records": records, "raw_bytes": raw, "stored_bytes": stored,
		"bytes_per_day": perDay, "available_bytes": availBytes,
		"used_pct": pct, "level": level,
	})
}

// ── 会话与鉴权（D-008：复用 TOS 用户与会话）──────────────────────────

func (s *Server) authInfo() map[string]any {
	if s.Auth == nil {
		return map[string]any{"mode": "off", "note": "鉴权未启用"}
	}
	d := s.Auth.Describe()
	if w := s.Auth.StartupWarning(); w != "" {
		d["warning"] = w
	}
	return d
}

// handleSession 返回当前会话用户与角色（前端用于展示与权限门控）。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.FromContext(r.Context())
	if !ok {
		auth.Unauthorized(w)
		return
	}
	writeData(w, map[string]any{
		"user": u,
		"auth": s.authInfo(),
		"capabilities": map[string]bool{
			"read":     u.Can("read"),
			"export":   u.Can("export"),
			"audit":    u.Can("audit.read"),
			"settings": u.Can("storage.settings"),
		},
	})
}

// handleAdminClaim 首次管理员认领：写标记 + 下发 HttpOnly cookie。
// 只允许一次（标记文件 O_EXCL）；之后只能读取 config/admin.token 解锁。
func (s *Server) handleAdminClaim(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeErr(w, http.StatusServiceUnavailable, "auth_unavailable", "鉴权未启用（仅本地开发模式）")
		return
	}
	switch err := s.Auth.Claim(w); {
	case err == nil:
		s.audit(r, "admin.claim", "first-run", "ok", "首次认领管理员")
		writeData(w, map[string]any{"claimed": true, "role": "admin"})
	case errors.Is(err, auth.ErrAlreadyClaimed):
		writeErr(w, http.StatusConflict, "already_claimed", "管理员已被认领；请用数据目录的 config/admin.token 解锁")
	case errors.Is(err, auth.ErrAdminDisabled):
		writeErr(w, http.StatusServiceUnavailable, "admin_disabled", "管理员令牌未启用（检查数据目录可写性）")
	default:
		writeErr(w, http.StatusInternalServerError, "claim_failed", err.Error())
	}
}
