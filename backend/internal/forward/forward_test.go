package forward

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Moechz/log-server/backend/internal/model"
)

func rec(host, app, class, raw string, sev int) *model.Record {
	return &model.Record{
		Raw: raw, Host: host, App: app, ClassPrimary: class, Msg: raw,
		SeverityNumber: &sev, IngestTime: time.Now().UTC(), EventTime: time.Now().UTC(),
	}
}

func TestFilter_Match(t *testing.T) {
	s := 3
	cases := []struct {
		name string
		f    Filter
		r    *model.Record
		want bool
	}{
		{"空过滤器全通过", Filter{}, rec("h", "a", "system", "x", 6), true},
		{"类别命中", Filter{Classes: []string{"auth"}}, rec("h", "a", "auth", "x", 6), true},
		{"类别不命中", Filter{Classes: []string{"auth"}}, rec("h", "a", "system", "x", 6), false},
		{"主机命中", Filter{Hosts: []string{"web01"}}, rec("web01", "a", "s", "x", 6), true},
		{"级别阈值", Filter{MinSeverity: &s}, rec("h", "a", "s", "x", 6), false},
		{"级别达标", Filter{MinSeverity: &s}, rec("h", "a", "s", "x", 3), true},
		{"文本排除", Filter{ExcludeText: "debug"}, rec("h", "a", "s", "some debug noise", 6), false},
		{"仅审计", Filter{OnlyAudit: true}, rec("h", "a", "s", "x", 6), false},
		{"来源类型", Filter{SourceKinds: []string{"journald"}}, rec("h", "a", "s", "x", 6), false},
	}
	for _, c := range cases {
		if got := c.f.Match(c.r); got != c.want {
			t.Errorf("%s: Match = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestEncodeRFC5424_PayloadUnchanged(t *testing.T) {
	app := "sshd"
	pid := 1234
	fac, sev := 10, 3
	r := &model.Record{
		Raw:  `<38>Oct 12 10:00:00 web01 sshd[1234]: Failed password for root`,
		Msg:  "Failed password for root",
		Host: "web01", App: app, PID: &pid,
		Facility: &fac, SeverityNumber: &sev,
		EventTime:    time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
		ClassPrimary: "auth", RetentionProfile: "security-180d", TimeTrusted: true,
	}
	out := string(encodeRFC5424(r))

	if !strings.HasPrefix(out, "<83>1 ") { // 10*8+3 = 83
		t.Errorf("PRI 封装错误: %q", out[:16])
	}
	if !strings.Contains(out, `[logserver@0 class="auth" retention="security-180d"`) {
		t.Errorf("SD 字段缺失: %s", out)
	}
	// ★ payload 必须无损：消息体原样跟在 SD 之后
	if !strings.HasSuffix(out, " Failed password for root") {
		t.Errorf("payload 被改动: %q", out)
	}
	if !strings.Contains(out, "2026-10-12T10:00:00Z") {
		t.Errorf("时间戳缺失: %s", out)
	}
}

func TestEncode_HTTPIsJSON(t *testing.T) {
	b, err := encode(ProtoHTTP, rec("h", "a", "system", "raw-line", 6))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.HasPrefix(s, "{") || !strings.Contains(s, `"_msg":"raw-line"`) || !strings.Contains(s, `"ls_host":"h"`) {
		t.Errorf("HTTP 编码应为 JSON 且带 ls_ 前缀: %s", s)
	}
}

// 真实投递：起一个本地 UDP 监听，验证 Manager 端到端把消息送出去
func TestManager_EndToEnd_UDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	m := NewManager(Config{QueueRoot: t.TempDir(), Timeout: time.Second, BatchMax: 16})
	if err := m.Apply([]Target{{
		ID: "t1", Name: "测试目标", Protocol: ProtoUDP,
		Addr: pc.LocalAddr().String(), Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Close()

	m.Enqueue(rec("web01", "sshd", "auth", "hello-forward", 3))

	buf := make([]byte, 4096)
	_ = pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("未收到转发消息: %v", err)
	}
	got := string(buf[:n])
	if !strings.Contains(got, "hello-forward") {
		t.Errorf("转发内容错误: %q", got)
	}

	// 等待队列被 Ack 清空（证明"投递成功才 Ack"）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Stats()
		if len(st) == 1 && st[0].Queue.Depth == 0 && st[0].Sent > 0 {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	st := m.Stats()
	t.Fatalf("投递后队列未被 Ack: %+v", st[0].Queue)
}

// 目标不可达时必须**保留队列**（不丢），并记录错误
func TestManager_UnreachableKeepsQueue(t *testing.T) {
	m := NewManager(Config{QueueRoot: t.TempDir(), Timeout: 300 * time.Millisecond, BatchMax: 8})
	// 指向一个几乎确定不可达的地址
	if err := m.Apply([]Target{{
		ID: "bad", Name: "坏目标", Protocol: ProtoTCP,
		Addr: "127.0.0.1:1", Enabled: true,
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Close()

	m.Enqueue(rec("h", "a", "s", "must-not-vanish", 6))

	time.Sleep(700 * time.Millisecond)
	st := m.Stats()
	if len(st) != 1 {
		t.Fatalf("目标数错误: %d", len(st))
	}
	if st[0].Queue.Depth == 0 {
		t.Error("投递失败时**必须保留队列**（至少一次语义），但深度为 0")
	}
	if st[0].LastErr == "" {
		t.Error("应记录失败原因（供 UI 显示，FR-507）")
	}
	if st[0].Queue.Retried == 0 {
		t.Error("应记录重试次数")
	}
}

func TestManager_ApplyIsIdempotentAndKeepsQueue(t *testing.T) {
	root := t.TempDir()
	m := NewManager(Config{QueueRoot: root})
	tgts := []Target{{ID: "a", Name: "A", Protocol: ProtoUDP, Addr: "127.0.0.1:9", Enabled: true}}
	if err := m.Apply(tgts); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(tgts); err != nil {
		t.Fatal(err)
	}
	if got := len(m.Targets()); got != 1 {
		t.Fatalf("幂等 Apply 应保持 1 个目标，得到 %d", got)
	}
	// 移除
	if err := m.Apply(nil); err != nil {
		t.Fatal(err)
	}
	if got := len(m.Targets()); got != 0 {
		t.Errorf("应移除全部目标，得到 %d", got)
	}
	m.Close()
}

func TestManager_InvalidTargetRejected(t *testing.T) {
	m := NewManager(Config{QueueRoot: t.TempDir()})
	if err := m.Apply([]Target{{ID: "", Protocol: ProtoUDP, Addr: "x"}}); err == nil {
		t.Error("缺 id 应被拒")
	}
	if err := m.Apply([]Target{{ID: "a", Protocol: "smtp", Addr: "x"}}); err == nil {
		t.Error("非法协议应被拒")
	}
	if err := m.Apply([]Target{{ID: "a", Protocol: ProtoUDP, Addr: ""}}); err == nil {
		t.Error("缺地址应被拒")
	}
}
