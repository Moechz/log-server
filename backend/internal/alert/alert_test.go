package alert

import (
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

func rec(raw, host, app string) *model.Record {
	return &model.Record{Raw: raw, Host: host, App: app, IngestTime: time.Now().UTC()}
}

func TestBuiltinRules_Compile(t *testing.T) {
	e := New(Config{})
	if err := e.Add(BuiltinRules()...); err != nil {
		t.Fatalf("内置规则应全部可编译: %v", err)
	}
	if got := len(e.Rules()); got < 18 {
		t.Errorf("内置规则数偏少: %d", got)
	}
}

func TestEvaluate_MatchesBuiltinPatterns(t *testing.T) {
	cases := []struct{ raw, wantRule string }{
		{"kernel: Out of memory: Killed process 999 (ffmpeg)", "oom"},
		{"nginx: write() failed (28: No space left on device)", "disk-full"},
		{"kernel: EXT4-fs error: Remounting filesystem read-only", "fs-ro"},
		{"smartd: Device: /dev/sda, I/O error", "io-error"},
		{"kernel: Machine Check Exception", "mce"},
		{"mdadm: Disk failure on sdb, disabling device", "raid-degraded"},
		{"kernel: Kernel panic - not syncing", "kernel-panic"},
		{"systemd: nginx.service: Failed with result 'exit-code'", "svc-failed"},
		{"sshd[1]: Failed password for root from 10.0.0.5", "ssh-brute"},
		{"dockerd: container OOMKilled", "container-oom"},
		{"nginx: 10.0.0.1 - - [x] \"POST /a HTTP/1.1\" 503 12", "http-5xx"},
	}
	e := New(Config{OnAlert: func(Alert) {}})
	if err := e.Add(BuiltinRules()...); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		before := e.Stats()
		e.Evaluate(rec(c.raw, "h1", "app"))
		after := e.Stats()
		if after.Total == before.Total && after.RuleHits[c.wantRule] == before.RuleHits[c.wantRule] {
			t.Errorf("规则 %s 未命中: %s", c.wantRule, c.raw)
		}
		if after.RuleHits[c.wantRule] == 0 {
			t.Errorf("规则 %s 命中计数为 0: %s", c.wantRule, c.raw)
		}
	}
}

func TestEvaluate_FrequencyRuleThreshold(t *testing.T) {
	e := New(Config{})
	if err := e.Add(Rule{
		ID: "flood", Name: "频率规则", Category: "测试", Pattern: `(?i)flood`,
		Level: LevelWarn, Enabled: true, Window: time.Minute, Threshold: 3,
	}); err != nil {
		t.Fatal(err)
	}
	r := rec("flood event", "h", "a")
	e.Evaluate(r)
	e.Evaluate(r)
	if got := len(e.Active()); got != 0 {
		t.Fatalf("未达阈值不应告警，得到 %d 条", got)
	}
	e.Evaluate(r) // 第 3 次达标
	if got := len(e.Active()); got != 1 {
		t.Fatalf("达到阈值应告警，得到 %d 条", got)
	}
}

func TestEvaluate_SuppressionAccumulatesCount(t *testing.T) {
	e := New(Config{SuppressBy: time.Hour})
	if err := e.Add(Rule{
		ID: "dup", Name: "重复告警", Category: "测试", Pattern: `(?i)boom`,
		Level: LevelCrit, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	r := rec("boom", "h", "a")
	for i := 0; i < 5; i++ {
		e.Evaluate(r)
	}
	act := e.Active()
	if len(act) != 1 {
		t.Fatalf("抑制期内应只有 1 条活跃告警，得到 %d", len(act))
	}
	if act[0].Count != 5 {
		t.Errorf("抑制期内应累加计数到 5，得到 %d", act[0].Count)
	}
}

func TestEvaluate_DifferentHostNotSuppressed(t *testing.T) {
	e := New(Config{SuppressBy: time.Hour})
	if err := e.Add(Rule{ID: "r", Name: "n", Category: "c", Pattern: `boom`, Level: LevelWarn, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.Evaluate(rec("boom", "h1", "a"))
	e.Evaluate(rec("boom", "h2", "a"))
	if got := len(e.Active()); got != 2 {
		t.Errorf("不同主机应各自告警，得到 %d", got)
	}
}

func TestEvaluate_OnAlertCallback(t *testing.T) {
	var got []Alert
	e := New(Config{OnAlert: func(a Alert) { got = append(got, a) }})
	if err := e.Add(Rule{ID: "cb", Name: "回调", Category: "c", Pattern: `x`, Level: LevelCrit, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.Evaluate(rec("x marks", "h", "a"))
	if len(got) != 1 || got[0].RuleID != "cb" || got[0].LevelText != "严重" {
		t.Fatalf("回调内容错误: %+v", got)
	}
}

func TestEvaluate_DisabledRuleIgnored(t *testing.T) {
	e := New(Config{})
	if err := e.Add(Rule{ID: "off", Name: "n", Category: "c", Pattern: `z`, Level: LevelWarn, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	e.Evaluate(rec("z", "h", "a"))
	if got := len(e.Active()); got != 0 {
		t.Errorf("停用规则不应触发，得到 %d", got)
	}
}

func TestAdd_InvalidRegexRejected(t *testing.T) {
	e := New(Config{})
	if err := e.Add(Rule{ID: "bad", Name: "n", Category: "c", Pattern: `([`, Level: LevelWarn, Enabled: true}); err == nil {
		t.Error("非法正则应被拒绝")
	}
}

func TestResolve(t *testing.T) {
	e := New(Config{})
	if err := e.Add(Rule{ID: "r", Name: "n", Category: "c", Pattern: `q`, Level: LevelWarn, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.Evaluate(rec("q", "h1", "a"))
	if !e.Resolve("r", "h1") {
		t.Fatal("解除应成功")
	}
	if got := len(e.Active()); got != 0 {
		t.Errorf("解除后应为空，得到 %d", got)
	}
}

func TestStats_ByLevel(t *testing.T) {
	e := New(Config{})
	if err := e.Add(
		Rule{ID: "c", Name: "n", Category: "c", Pattern: `crit-x`, Level: LevelCrit, Enabled: true},
		Rule{ID: "w", Name: "n", Category: "c", Pattern: `warn-y`, Level: LevelWarn, Enabled: true},
	); err != nil {
		t.Fatal(err)
	}
	e.Evaluate(rec("crit-x", "h", "a"))
	e.Evaluate(rec("warn-y", "h", "a"))
	st := e.Stats()
	if st.ByLevel["严重"] != 1 || st.ByLevel["警告"] != 1 {
		t.Errorf("按级别计数错误: %+v", st.ByLevel)
	}
}
