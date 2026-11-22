// silence.go —— FR-804：告警确认 / 静默 / 维护窗口。
//
// 设计：静默以「作用域 + 到期时间」表达（RuleID/Host/App 为空即不限）；
// 三者全空 = 全局维护窗口。运行时在 Evaluate 里先查静默，命中则不产生新告警、不通知。
package alert

import (
	"fmt"
	"time"
)

// silencedLocked 判断 now 时刻是否有静默命中（调用方需持锁）。
func (e *Engine) silencedLocked(ruleID, host, app string, now time.Time) bool {
	for _, s := range e.silences {
		if s.Until.After(now) && s.Match(ruleID, host, app) {
			return true
		}
	}
	return false
}

// Silences 返回未过期的静默（新的在前）。
func (e *Engine) Silences() []Silence {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.activeSilencesLocked()
}

func (e *Engine) activeSilencesLocked() []Silence {
	now := time.Now()
	out := make([]Silence, 0, len(e.silences))
	for _, s := range e.silences {
		if s.Until.After(now) {
			out = append(out, s)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt.After(out[i].CreatedAt) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// RestoreSilences 用持久化数据恢复（启动时调用，自动丢弃已过期项）。
func (e *Engine) RestoreSilences(list []Silence) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	e.silences = e.silences[:0]
	for _, s := range list {
		if !s.Until.After(now) {
			continue
		}
		if n, err := parseSilenceSeq(s.ID); err == nil && n > e.silSeq {
			e.silSeq = n
		}
		e.silences = append(e.silences, s)
	}
}

// AddSilence 新增一条静默（minutes<=0 视为 60 分钟）。
func (e *Engine) AddSilence(s Silence, minutes int) Silence {
	if minutes <= 0 {
		minutes = 60
	}
	e.mu.Lock()
	e.silSeq++
	now := time.Now()
	s.ID = fmt.Sprintf("S%06d", e.silSeq)
	s.CreatedAt = now
	s.Until = now.Add(time.Duration(minutes) * time.Minute)
	e.silences = append(e.silences, s)
	list := e.snapshotSilencesLocked()
	e.mu.Unlock()
	e.notifySilenceChange(list)
	return s
}

// SetMaintenance 设置全局维护窗口（覆盖旧的维护窗口，不影响普通静默）。
func (e *Engine) SetMaintenance(until time.Time, reason, by string) Silence {
	e.mu.Lock()
	keep := e.silences[:0]
	for _, s := range e.silences {
		if !s.Maintenance {
			keep = append(keep, s)
		}
	}
	e.silences = keep
	e.silSeq++
	s := Silence{
		ID: fmt.Sprintf("S%06d", e.silSeq), Until: until, Reason: reason,
		By: by, Maintenance: true, CreatedAt: time.Now(),
	}
	e.silences = append(e.silences, s)
	list := e.snapshotSilencesLocked()
	e.mu.Unlock()
	e.notifySilenceChange(list)
	return s
}

// ClearMaintenance 结束维护窗口，返回是否确有维护窗口。
func (e *Engine) ClearMaintenance() bool {
	e.mu.Lock()
	keep := e.silences[:0]
	removed := false
	for _, s := range e.silences {
		if s.Maintenance {
			removed = true
			continue
		}
		keep = append(keep, s)
	}
	e.silences = keep
	list := e.snapshotSilencesLocked()
	e.mu.Unlock()
	if removed {
		e.notifySilenceChange(list)
	}
	return removed
}

// RemoveSilence 删除一条静默。
func (e *Engine) RemoveSilence(id string) bool {
	e.mu.Lock()
	keep := e.silences[:0]
	removed := false
	for _, s := range e.silences {
		if s.ID == id {
			removed = true
			continue
		}
		keep = append(keep, s)
	}
	e.silences = keep
	list := e.snapshotSilencesLocked()
	e.mu.Unlock()
	if removed {
		e.notifySilenceChange(list)
	}
	return removed
}

func (e *Engine) snapshotSilencesLocked() []Silence {
	out := make([]Silence, len(e.silences))
	copy(out, e.silences)
	return out
}

func (e *Engine) notifySilenceChange(list []Silence) {
	if e.cfg.OnSilenceChange != nil {
		e.cfg.OnSilenceChange(list)
	}
}

// Ack 确认一条活跃告警（不改变 firing 状态）。
func (e *Engine) Ack(id, by string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for _, a := range e.active {
		if a.ID == id {
			a.Acked, a.AckedAt, a.AckedBy = true, &now, by
			return true
		}
	}
	return false
}

// AckAll 确认全部活跃告警，返回本次新确认的条数。
func (e *Engine) AckAll(by string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	n := 0
	for _, a := range e.active {
		if !a.Acked {
			a.Acked, a.AckedAt, a.AckedBy = true, &now, by
			n++
		}
	}
	return n
}

func parseSilenceSeq(id string) (uint64, error) {
	if len(id) < 2 {
		return 0, fmt.Errorf("bad id")
	}
	var n uint64
	_, err := fmt.Sscanf(id[1:], "%d", &n)
	return n, err
}
