package file

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestTailer_Append_OnlyCompleteLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	write(t, p, "line1\nline2\nline3-partial")

	tl := New(p, Offset{}, Multiline{})
	got, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "line1" || got[1] != "line2" {
		t.Fatalf("应只读到 2 个完整行，得到 %q", got)
	}

	// 补上换行 → 之前的不完整行应作为一条新记录出现（不重复 line1/line2）
	write(t, p, "\nline4\n")
	got2, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 2 || got2[0] != "line3-partial" || got2[1] != "line4" {
		t.Fatalf("续读错误（不丢不重）: %q", got2)
	}
}

func TestTailer_CopyTruncate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	write(t, p, "aaa\nbbb\n")
	tl := New(p, Offset{}, Multiline{})
	if got, _ := tl.Poll(); len(got) != 2 {
		t.Fatalf("首次读取错误: %q", got)
	}

	// copytruncate：内容被截断（inode 不变，size 变小）
	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	write(t, p, "ccc\n")
	got, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "ccc" {
		t.Fatalf("copytruncate 后应读到 ccc，得到 %q", got)
	}
	if tl.State().Truncates != 1 {
		t.Errorf("应记录 1 次截断，得到 %d", tl.State().Truncates)
	}
}

func TestTailer_RenameRotation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	write(t, p, "old1\n")
	tl := New(p, Offset{}, Multiline{})
	if got, _ := tl.Poll(); len(got) != 1 || got[0] != "old1" {
		t.Fatalf("首次读取错误: %q", got)
	}

	// rename 型轮转：改名旧文件 + 新建同名文件
	if err := os.Rename(p, filepath.Join(dir, "app.log.1")); err != nil {
		t.Fatal(err)
	}
	write(t, p, "new1\n")
	got, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "new1" {
		t.Fatalf("轮转后应读到新文件内容，得到 %q", got)
	}
	if tl.State().Rotations != 1 {
		t.Errorf("应记录 1 次轮转，得到 %d", tl.State().Rotations)
	}
	if tl.State().Inode == 0 {
		t.Error("位点必须带 inode（仅 path+offset 无法识别轮转）")
	}
}

func TestTailer_MultilineMerge(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	write(t, p, "2026-10-12 10:00:00 ERROR boom\n\tat a.b(C.java:1)\n\tat d.e(F.java:2)\n2026-10-12 10:00:01 INFO ok\n")

	tl := New(p, Offset{}, DefaultMultiline())
	got, err := tl.Poll()
	if err != nil {
		t.Fatal(err)
	}
	// 正确语义：合并时**最后一条会被暂存**（要等下一行才能判断是否续行）
	if len(got) != 1 {
		t.Fatalf("应吐出第 1 条合并记录（第 2 条暂存），得到 %d: %q", len(got), got)
	}
	if got[0] != "2026-10-12 10:00:00 ERROR boom\n\tat a.b(C.java:1)\n\tat d.e(F.java:2)" {
		t.Errorf("多行合并错误: %q", got[0])
	}
	// Flush 吐出暂存的最后一条
	tail := tl.Flush()
	if len(tail) != 1 || tail[0] != "2026-10-12 10:00:01 INFO ok" {
		t.Errorf("Flush 应吐出暂存记录，得到 %q", tail)
	}
}

func TestTailer_MultilineStaleFlush(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	write(t, p, "2026-10-12 10:00:00 ERROR boom\n")
	ml := DefaultMultiline()
	ml.FlushAfter = 30 * time.Millisecond
	tl := New(p, Offset{}, ml)

	// 启用合并时，**首条也会被暂存**（无法预知是否有续行）→ 此轮不吐任何东西
	if got, _ := tl.Poll(); len(got) != 0 {
		t.Fatalf("启用合并时首条应暂存，得到 %q", got)
	}
	// 等超过 FlushAfter → 无需新数据也应自动吐出
	time.Sleep(50 * time.Millisecond)
	got, _ := tl.Poll()
	if len(got) != 1 || got[0] != "2026-10-12 10:00:00 ERROR boom" {
		t.Fatalf("超时后应自动吐出暂存记录，得到 %q", got)
	}

	// 新行到来 → 同样先暂存（合并语义：最后一条要等下一行或超时）
	write(t, p, "2026-10-12 10:00:05 INFO two\n")
	if got, _ := tl.Poll(); len(got) != 0 {
		t.Fatalf("新行也应暂存，得到 %q", got)
	}
	time.Sleep(50 * time.Millisecond)
	if got, _ := tl.Poll(); len(got) != 1 || got[0] != "2026-10-12 10:00:05 INFO two" {
		t.Fatalf("超时后应吐出，得到 %q", got)
	}
}

// 合并语义下的关键特征：**同一批到达的多行，除了最后一条，其余立即吐出**
func TestTailer_MultilineBatchLatency(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	write(t, p, "2026-10-12 10:00:01 A\n2026-10-12 10:00:02 B\n2026-10-12 10:00:03 C\n")
	tl := New(p, Offset{}, DefaultMultiline())
	got, _ := tl.Poll()
	if len(got) != 2 || got[0] != "2026-10-12 10:00:01 A" || got[1] != "2026-10-12 10:00:02 B" {
		t.Fatalf("A/B 应立即吐出（C 证明 B 已结束），得到 %q", got)
	}
	if tail := tl.Flush(); len(tail) != 1 || tail[0] != "2026-10-12 10:00:03 C" {
		t.Fatalf("C 应由 Flush 吐出，得到 %q", tail)
	}
}

func TestTailer_MultilineFlush(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "app.log")
	// 启用合并时首条暂存 → Flush 应吐出；再 Flush 不应重复
	write(t, p, "2026-10-12 10:00:00 ERROR x\n")
	tl := New(p, Offset{}, DefaultMultiline())
	if got, _ := tl.Poll(); len(got) != 0 {
		t.Fatalf("启用合并时首条应暂存，得到 %q", got)
	}
	first := tl.Flush()
	if len(first) != 1 || first[0] != "2026-10-12 10:00:00 ERROR x" {
		t.Fatalf("Flush 应吐出暂存记录: %q", first)
	}
	if extra := tl.Flush(); len(extra) != 0 {
		t.Errorf("重复 Flush 不应再输出: %q", extra)
	}
}

func TestOffsetStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := &Store{Path: filepath.Join(dir, "state", "offsets.json")}
	in := map[string]Offset{
		"/var/log/x.log": {Path: "/var/log/x.log", Dev: 1, Inode: 42, Offset: 100},
	}
	if err := st.Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := out["/var/log/x.log"]; got.Inode != 42 || got.Offset != 100 {
		t.Fatalf("位点回读错误: %+v", got)
	}
}

func TestOffsetStore_MissingFileIsNotError(t *testing.T) {
	st := &Store{Path: filepath.Join(t.TempDir(), "none", "offsets.json")}
	m, err := st.Load()
	if err != nil {
		t.Fatalf("文件不存在不应报错: %v", err)
	}
	if len(m) != 0 {
		t.Errorf("应返回空表，得到 %d 项", len(m))
	}
}
