// logs.go —— 检索系列端点：条件解析（共用）、时间轴直方图（FR-403）、上下文（FR-406）。
package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Moechz/log-server/backend/internal/store"
)

// logQueryFromRequest 把 /logs、/logs/histogram、/logs/context 共用的查询参数
// 解析为 store.Query（q / host / app / class / min_severity / from / to / limit）。
func logQueryFromRequest(r *http.Request, defLimit int) store.Query {
	q := r.URL.Query()
	limit := defLimit
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	sq := store.Query{Limit: limit, Text: q.Get("q")}
	if h := q.Get("host"); h != "" {
		sq.Hosts = []string{h}
	}
	if a := q.Get("app"); a != "" {
		sq.Apps = []string{a}
	}
	if c := q.Get("class"); c != "" {
		sq.Classes = []string{c}
	}
	if ms := q.Get("min_severity"); ms != "" {
		if n, err := strconv.Atoi(ms); err == nil && n >= 0 && n <= 7 {
			sq.MinSeverity = &n
		}
	}
	if from := q.Get("from"); from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			sq.Range.From = t
		}
	}
	if to := q.Get("to"); to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			sq.Range.To = t
		}
	}
	return sq
}

// handleLogsHistogram 时间轴直方图（FR-403）：按区间返回计数，供前端画图与框选下钻。
func (s *Server) handleLogsHistogram(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage_unavailable", "存储引擎不可用")
		return
	}
	sq := logQueryFromRequest(r, 0)
	now := time.Now().UTC()
	if sq.Range.To.IsZero() {
		sq.Range.To = now
	}
	if sq.Range.From.IsZero() {
		sq.Range.From = sq.Range.To.Add(-24 * time.Hour)
	}
	interval := parseInterval(r.URL.Query().Get("interval"))
	if interval <= 0 {
		interval = autoInterval(sq.Range.To.Sub(sq.Range.From))
	}
	buckets, err := s.Store.Histogram(r.Context(), sq, interval)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "store_error", err.Error())
		return
	}
	var total int64
	for _, b := range buckets {
		total += b.Count
	}
	writeData(w, map[string]any{
		"interval_sec": int(interval.Seconds()),
		"from":         sq.Range.From.UTC(),
		"to":           sq.Range.To.UTC(),
		"buckets":      buckets,
		"total":        total,
	})
}

// handleLogsContext 上下文（FR-406）：返回 pivot 之前/之后各 N 条（均按时间正序）。
// 告警页的「查看上下文」（FR-807）与检索结果行的「上下文」按钮都走这里。
func (s *Server) handleLogsContext(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "storage_unavailable", "存储引擎不可用")
		return
	}
	cr, ok := s.Store.(store.ContextReader)
	if !ok {
		writeErr(w, http.StatusNotImplemented, "not_supported", "当前存储引擎不支持上下文查询")
		return
	}
	q := r.URL.Query()
	pivot, err := time.Parse(time.RFC3339, q.Get("t"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_time", "参数 t 必须是 RFC3339 时间（如 2026-10-14T12:00:00Z）")
		return
	}
	before := clampInt(atoiOr(q.Get("before"), 10), 0, 100)
	after := clampInt(atoiOr(q.Get("after"), 10), 0, 100)

	sq := logQueryFromRequest(r, 0)
	sq.Range = store.TimeRange{} // 时间范围由 pivot 决定，忽略 from/to
	res, err := cr.Context(r.Context(), sq, pivot.UTC(), before, after)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "store_error", err.Error())
		return
	}
	writeData(w, map[string]any{
		"pivot":  pivot.UTC(),
		"before": res.Before,
		"after":  res.After,
	})
}

// ── 小工具 ────────────────────────────────────────────────────────

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// parseInterval 解析 "60s" / "5m" / "1h" / "7d"。
func parseInterval(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return 0
}

// autoInterval 让时间轴大致落在 ≤60 根柱子（在候选档位里选最接近的）。
func autoInterval(span time.Duration) time.Duration {
	if span <= 0 {
		span = 24 * time.Hour
	}
	for _, c := range []time.Duration{
		time.Minute, 5 * time.Minute, 15 * time.Minute,
		time.Hour, 6 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour,
	} {
		if span/c <= 60 {
			return c
		}
	}
	return 7 * 24 * time.Hour
}
