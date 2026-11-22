package queue

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newQ(t *testing.T, segBytes int64, maxBytes int64) (*Queue, string) {
	t.Helper()
	dir := t.TempDir()
	q, err := Open(Config{Dir: dir, SegmentBytes: segBytes, MaxBytes: maxBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q, dir
}

func TestQueue_PushPopAck(t *testing.T) {
	q, _ := newQ(t, 1<<20, 0)
	for _, s := range []string{"a", "bb", "ccc"} {
		if err := q.Push([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if st := q.Stats(); st.Depth != 3 {
		t.Fatalf("深度应为 3，得到 %d", st.Depth)
	}
	got, err := q.Pop(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || string(got[0]) != "a" || string(got[2]) != "ccc" {
		t.Fatalf("读取错误: %q", got)
	}
	// Pop 不推进：再 Pop 仍是同样 3 条（未 Ack）
	again, _ := q.Pop(10)
	if len(again) != 3 {
		t.Fatalf("未 Ack 前 Pop 不应推进位点，得到 %d 条", len(again))
	}
	if err := q.Ack(2); err != nil {
		t.Fatal(err)
	}
	if st := q.Stats(); st.Depth != 1 {
		t.Fatalf("Ack 2 条后深度应为 1，得到 %d", st.Depth)
	}
	rest, _ := q.Pop(10)
	if len(rest) != 1 || string(rest[0]) != "ccc" {
		t.Fatalf("剩余应为 ccc，得到 %q", rest)
	}
}

func TestQueue_CrashRecovery_ReplaysUnacked(t *testing.T) {
	dir := t.TempDir()
	q, err := Open(Config{Dir: dir, SegmentBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"x1", "x2", "x3"} {
		if err := q.Push([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Pop(2); err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(2); err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃：不 Close，直接重新打开
	q2, err := Open(Config{Dir: dir, SegmentBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer q2.Close()

	if st := q2.Stats(); st.Depth != 1 {
		t.Fatalf("恢复后深度应为 1（已 Ack 的 2 条不重放），得到 %d", st.Depth)
	}
	recs, err := q2.Pop(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || string(recs[0]) != "x3" {
		t.Fatalf("恢复后应只剩 x3，得到 %q", recs)
	}
}

func TestQueue_SegmentRotation(t *testing.T) {
	q, dir := newQ(t, 32, 0) // 极小段 → 强制轮转
	for i := 0; i < 10; i++ {
		if err := q.Push([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	segs := q.listSegments()
	if len(segs) < 2 {
		t.Fatalf("应产生多个段文件，得到 %d", len(segs))
	}
	recs, err := q.Pop(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 10 {
		t.Fatalf("跨段读取应拿到 10 条，得到 %d", len(recs))
	}
	if err := q.Ack(10); err != nil {
		t.Fatal(err)
	}
	if st := q.Stats(); st.Depth != 0 {
		t.Errorf("全部 Ack 后深度应为 0，得到 %d", st.Depth)
	}
	// 已消费的段文件应被删除（只留可能正在写的那一个）
	left, _ := os.ReadDir(dir)
	segLeft := 0
	for _, e := range left {
		if filepath.Ext(e.Name()) == ".log" {
			segLeft++
		}
	}
	if segLeft > 1 {
		t.Errorf("已消费段应被删除，仍剩 %d 个段文件", segLeft)
	}
}

func TestQueue_MaxBytesRejects(t *testing.T) {
	q, _ := newQ(t, 1<<20, 40)
	body := make([]byte, 16)
	okCount := 0
	for i := 0; i < 10; i++ {
		if err := q.Push(body); err == nil {
			okCount++
			continue
		}
		break
	}
	if okCount == 0 || okCount >= 10 {
		t.Fatalf("应在达到上限后拒绝（已写入 %d 条）", okCount)
	}
	if st := q.Stats(); st.Dropped == 0 {
		t.Error("被拒绝的推送应计入 Dropped（水位保护可见）")
	}
}

func TestQueue_EmptyPop(t *testing.T) {
	q, _ := newQ(t, 1<<20, 0)
	if _, err := q.Pop(10); err != ErrEmpty {
		t.Fatalf("空队列应返回 ErrEmpty，得到 %v", err)
	}
}

func TestQueue_OldestAge(t *testing.T) {
	q, _ := newQ(t, 1<<20, 0)
	if err := q.Push([]byte("z")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if st := q.Stats(); st.OldestAge <= 0 {
		t.Errorf("应统计最老消息年龄，得到 %v", st.OldestAge)
	}
}

func TestQueue_PushAfterClose(t *testing.T) {
	q, _ := newQ(t, 1<<20, 0)
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Push([]byte("x")); err != ErrClosed {
		t.Fatalf("关闭后写入应返回 ErrClosed，得到 %v", err)
	}
}

// TestQueue_PushAfterAckDeletesActiveSegment：Ack 删掉"当前正在写的段"后，
// 下一次 Push 必须落到新段，且能被 Pop 读到（否则新记录静默丢失）。
func TestQueue_PushAfterAckDeletesActiveSegment(t *testing.T) {
	q, _ := newQ(t, 1<<20, 0)
	if err := q.Push([]byte("a")); err != nil {
		t.Fatal(err)
	}
	msgs, err := q.Pop(10)
	if err != nil || len(msgs) != 1 || string(msgs[0]) != "a" {
		t.Fatalf("Pop 初始记录失败: %v %q", err, msgs)
	}
	if err := q.Ack(1); err != nil {
		t.Fatal(err)
	}
	if err := q.Push([]byte("b")); err != nil {
		t.Fatal(err)
	}
	msgs, err = q.Pop(10)
	if err != nil || len(msgs) != 1 || string(msgs[0]) != "b" {
		t.Fatalf("Ack 删段后的 Push 记录读不到（数据丢失）: err=%v n=%d", err, len(msgs))
	}
}
