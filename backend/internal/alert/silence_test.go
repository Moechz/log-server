package alert

import (
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

func mkRec(msg string) *model.Record {
	return &model.Record{Raw: msg, Msg: msg, Host: "h1", App: "sshd"}
}

func TestSilenceSuppressesFiring(t *testing.T) {
	var fired int
	e := New(Config{OnAlert: func(Alert) { fired++ }, SuppressBy: time.Millisecond})
	if err := e.Add(Rule{ID: "r1", Name: "R1", Pattern: "boom", Level: LevelWarn, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// 静默规则 r1
	e.AddSilence(Silence{RuleID: "r1"}, 60)
	e.Evaluate(mkRec("boom happened"))
	if fired != 0 {
		t.Fatalf("静默期内不应告警，fired=%d", fired)
	}
	if len(e.Silences()) != 1 {
		t.Fatal("静默应存在")
	}
	// 移除静默后应告警
	if !e.RemoveSilence(e.Silences()[0].ID) {
		t.Fatal("移除静默失败")
	}
	e.Evaluate(mkRec("boom happened"))
	if fired != 1 {
		t.Fatalf("移除静默后应告警，fired=%d", fired)
	}
}

func TestMaintenanceSilencesEverything(t *testing.T) {
	var fired int
	e := New(Config{OnAlert: func(Alert) { fired++ }, SuppressBy: time.Millisecond})
	_ = e.Add(Rule{ID: "r1", Name: "R1", Pattern: "x", Level: LevelWarn, Enabled: true})
	_ = e.Add(Rule{ID: "r2", Name: "R2", Pattern: "y", Level: LevelWarn, Enabled: true})
	e.SetMaintenance(time.Now().Add(time.Hour), "升级", "admin")
	e.Evaluate(mkRec("x here"))
	e.Evaluate(mkRec("y here"))
	if fired != 0 {
		t.Fatalf("维护窗口内不应告警，fired=%d", fired)
	}
	if !e.Stats().Maintenance {
		t.Fatal("Stats 应反映维护窗口")
	}
	if !e.ClearMaintenance() {
		t.Fatal("应能结束维护窗口")
	}
	e.Evaluate(mkRec("x here"))
	if fired != 1 {
		t.Fatalf("维护结束后应告警，fired=%d", fired)
	}
}

func TestSilenceScopeHost(t *testing.T) {
	e := New(Config{OnAlert: func(Alert) {}})
	_ = e.Add(Rule{ID: "r1", Name: "R1", Pattern: "x", Level: LevelWarn, Enabled: true})
	e.AddSilence(Silence{RuleID: "r1", Host: "other"}, 60)
	var fired int
	e2 := e
	e2.cfg.OnAlert = func(Alert) { fired++ }
	e2.Evaluate(mkRec("x")) // host=h1，不应被 host=other 的静默命中
	if fired != 1 {
		t.Fatalf("作用域不匹配时不应静默，fired=%d", fired)
	}
}

func TestSilenceExpiry(t *testing.T) {
	e := New(Config{OnAlert: func(Alert) {}})
	sl := e.AddSilence(Silence{RuleID: "r1"}, 1)
	if len(e.Silences()) != 1 {
		t.Fatal("静默应存在")
	}
	// 手动过期
	e.mu.Lock()
	for i := range e.silences {
		if e.silences[i].ID == sl.ID {
			e.silences[i].Until = time.Now().Add(-time.Second)
		}
	}
	e.mu.Unlock()
	if len(e.Silences()) != 0 {
		t.Fatal("过期静默不应返回")
	}
}

func TestAckAndRestore(t *testing.T) {
	e := New(Config{OnAlert: func(Alert) {}, SuppressBy: time.Millisecond})
	_ = e.Add(Rule{ID: "r1", Name: "R1", Pattern: "x", Level: LevelWarn, Enabled: true})
	e.Evaluate(mkRec("x"))
	act := e.Active()
	if len(act) != 1 {
		t.Fatalf("应有 1 条活跃告警，得到 %d", len(act))
	}
	if !e.Ack(act[0].ID, "admin") {
		t.Fatal("确认失败")
	}
	if !e.Active()[0].Acked {
		t.Fatal("应标记为已确认")
	}
	if e.AckAll("admin") != 0 {
		t.Fatal("全部确认后应无新增")
	}

	// 静默持久化恢复
	e.AddSilence(Silence{RuleID: "r1"}, 60)
	list := e.Silences()
	e2 := New(Config{})
	e2.RestoreSilences(list)
	if len(e2.Silences()) != 1 {
		t.Fatal("恢复静默失败")
	}
}
