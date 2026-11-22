package selflog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeClock 让"跨天""过期"这些时间相关行为可确定地测试。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newSink(t *testing.T, opt FileOptions) (*FileSink, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	opt.Now = clk.now
	s, err := OpenFileSink(opt)
	if err != nil {
		t.Fatalf("OpenFileSink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, clk
}

// TestFileSink_WritesJSONLinesAndMasksSecrets 覆盖：① JSON Lines 格式可解析；
// ② 凭据按键名与值样式**都**被脱敏（红线 7）；③ 统计可用。
func TestFileSink_WritesJSONLinesAndMasksSecrets(t *testing.T) {
	dir := t.TempDir()
	s, _ := newSink(t, FileOptions{Dir: dir})

	entries := []Entry{
		{Ts: time.Now(), Level: "info", Module: "main", Msg: "启动"},
		{Ts: time.Now(), Level: "warn", Module: "forward", Msg: "目标不可达",
			Fields: map[string]string{"url": "https://user:secretpw@host:514/", "token": "ghp_abcdefghijklmnopqrst"}},
		{Ts: time.Now(), Level: "error", Module: "auth", Msg: "Bearer abcdefghijklmnopqrstuvwxyz123456 被拒绝"},
	}
	for _, e := range entries {
		if err := s.Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	b, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	for _, secret := range []string{"secretpw", "ghp_abcdefghijklmnopqrst", "abcdefghijklmnopqrstuvwxyz123456"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("凭据落盘（%s 未脱敏）:\n%s", secret, raw)
		}
	}
	if !strings.Contains(raw, "***") {
		t.Fatalf("未见脱敏占位符:\n%s", raw)
	}
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("应写 3 行，实际 %d", len(lines))
	}
	for i, ln := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v (%s)", i+1, err, ln)
		}
		if e.Level == "" || e.Module == "" {
			t.Fatalf("第 %d 行字段缺失: %s", i+1, ln)
		}
	}
	st := s.Stats()
	if st.Files != 1 || st.Written != 3 || st.Bytes == 0 || st.Path == "" {
		t.Fatalf("统计异常: %+v", st)
	}
	if st.MaxTotalBytes != DefaultMaxTotalBytes || st.MaxAgeDays != 7 {
		t.Fatalf("默认上限应为 200MB/7 天: %+v", st)
	}
}

// TestFileSink_RotatesBySize 覆盖单文件上限：写入超过上限 → 轮转出新文件。
func TestFileSink_RotatesBySize(t *testing.T) {
	dir := t.TempDir()
	s, _ := newSink(t, FileOptions{Dir: dir, MaxFileBytes: 512, MaxTotalBytes: 1 << 20})
	for i := 0; i < 40; i++ {
		if err := s.Write(Entry{Ts: time.Now(), Level: "info", Module: "x", Msg: strings.Repeat("a", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	st := s.Stats()
	if st.Files < 2 {
		t.Fatalf("应轮转出多个文件，实际 %d（bytes=%d）", st.Files, st.Bytes)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		fi, _ := e.Info()
		if fi.Size() > 512 {
			t.Fatalf("文件 %s 超过单文件上限: %d", e.Name(), fi.Size())
		}
	}
}

// TestFileSink_PrunesByAgeAndTotal 覆盖 7 天过期清理与总量上限清理。
func TestFileSink_PrunesByAgeAndTotal(t *testing.T) {
	dir := t.TempDir()
	s, clk := newSink(t, FileOptions{Dir: dir, MaxFileBytes: 256, MaxTotalBytes: 700, MaxAge: 24 * time.Hour})

	// 造 8 个"历史文件"（每个 100B），修改时间逐个往前推 1 天
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, "app-old-"+string(rune('a'+i))+".log")
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 100), 0o640); err != nil {
			t.Fatal(err)
		}
		old := clk.now().Add(-time.Duration(i+1) * 24 * time.Hour)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	// 触发一次写入（写入路径里 GC 每分钟最多一次 → 换分钟以强制 GC）
	clk.add(2 * time.Minute)
	if err := s.Write(Entry{Ts: clk.now(), Level: "info", Module: "gc", Msg: "trigger"}); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.Bytes > 700 {
		t.Fatalf("总量超上限未被清理: %+v", st)
	}
	// 超过 1 天的历史文件都应被删（除了当天新写的）
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		fi, _ := e.Info()
		if fi.Name() != "app.log" && clk.now().Sub(fi.ModTime()) > 24*time.Hour {
			t.Fatalf("过期文件未被清理: %s (mod=%s)", fi.Name(), fi.ModTime())
		}
	}
	if st.Files == 0 {
		t.Fatal("不应把当前文件也删掉")
	}
}

// TestTailLines 覆盖下载接口依赖的尾部读取（不截断行、行数正确）。
func TestTailLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	var sb strings.Builder
	for i := 1; i <= 5000; i++ {
		sb.WriteString("line-" + string(rune('0'+i%10)) + "-" + strings.Repeat("y", 40) + "\n")
	}
	if err := os.WriteFile(p, []byte(sb.String()), 0o640); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out bytes.Buffer
	if err := TailLines(f, 10, &out); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(got) != 10 {
		t.Fatalf("应返回 10 行，实际 %d", len(got))
	}
	if !strings.HasPrefix(got[0], "line-") || len(got[0]) != 47 {
		t.Fatalf("行被截断: %q", got[0])
	}
	// 全量读取也不能报错
	out.Reset()
	if err := TailLines(f, 100000, &out); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "\n"); n != 5000 {
		t.Fatalf("全量应为 5000 行，实际 %d", n)
	}
}

// TestLog_AddFanOutToSink：内存环形缓冲与落盘同时生效（Add 一处即两处）。
func TestLog_AddFanOutToSink(t *testing.T) {
	dir := t.TempDir()
	l := New(4)
	sink, err := OpenFileSink(FileOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	l.SetFileSink(sink)

	for i := 0; i < 6; i++ { // 超过内存容量 4 → 内存会丢，落盘不丢
		l.Add("info", "test", "第 N 条", "n", string(rune('0'+i)))
	}
	if st := l.Stats(); st.Used != 4 {
		t.Fatalf("内存环形缓冲应只保留 4 条，实际 %d", st.Used)
	}
	b, err := os.ReadFile(sink.Path())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 6 {
		t.Fatalf("落盘应保留全部 6 条，实际 %d", n)
	}
	fs := l.FileStats()
	if fs.Path == "" || fs.Written != 6 {
		t.Fatalf("FileStats 异常: %+v", fs)
	}
}

// TestOpenFileSink_BadDir：目录不可用时返回错误（调用方降级为仅内存，不阻断启动）。
func TestOpenFileSink_BadDir(t *testing.T) {
	if _, err := OpenFileSink(FileOptions{Dir: ""}); err == nil {
		t.Fatal("空目录应报错")
	}
	// 用一个"父路径是文件"的目录制造失败
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileSink(FileOptions{Dir: filepath.Join(f, "logs")}); err == nil {
		t.Fatal("父路径为文件时应报错")
	}
}

// TestFileSink_RestartAppendsSameDay：重启（同一天）必须**续写** app.log，不归档、不丢历史。
func TestFileSink_RestartAppendsSameDay(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	open := func() *FileSink {
		s, err := OpenFileSink(FileOptions{Dir: dir, Now: clk.now})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s1 := open()
	if err := s1.Write(Entry{Ts: clk.now(), Level: "info", Module: "a", Msg: "第一次运行"}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Write(Entry{Ts: clk.now(), Level: "info", Module: "a", Msg: "第二次"}); err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()

	// 文件 mtime 来自真实时钟，而本测试用假时钟；不显式对齐的话，
	// 「同一天续写」的判断会随测试运行日期而变（只有 2026-10-14 当天跑才通过，
	// 其它日子会被误判为跨天并归档 → 断言 3 行实际 1 行）。这里把 mtime
	// 固定到假时钟，使测试与运行日期无关。
	if err := os.Chtimes(filepath.Join(dir, selfLogBaseName), clk.now(), clk.now()); err != nil {
		t.Fatal(err)
	}

	clk.add(6 * time.Hour) // 同一天稍晚重启
	s2 := open()
	defer s2.Close()
	if err := s2.Write(Entry{Ts: clk.now(), Level: "info", Module: "b", Msg: "重启后"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s2.Path())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Fatalf("同一天重启应续写为 3 行，实际 %d:\n%s", n, b)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("同一天重启不应产生归档文件，实际目录有 %d 个文件", len(entries))
	}

	// 跨天重启 → 归档旧文件，另起当天的 app.log
	clk.add(24 * time.Hour)
	s3 := open()
	defer s3.Close()
	if err := s3.Write(Entry{Ts: clk.now(), Level: "info", Module: "c", Msg: "次日"}); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("跨天重启应归档出 2 个文件（旧 + 新），实际 %d", len(entries))
	}
	b, _ = os.ReadFile(s3.Path())
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Fatalf("跨天后 app.log 应为新文件（1 行），实际 %d", n)
	}
}
