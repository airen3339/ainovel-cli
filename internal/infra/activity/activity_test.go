package activity

import (
	"fmt"
	"testing"
	"time"
)

func at() time.Time { return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC) }

func toolEvent(kind Kind, tool string) Event {
	return Event{ProjectID: "book-1", RunID: "run-1", OperationID: "op-1", Kind: kind, Tool: tool, At: at()}
}

// 契约 3 + 真实事件时序：参数 delta 先于工具执行（agentcore 在整条消息完成后
// 才发 ToolExecStart）——首个 delta 即建立进行中条目并累计进度，ToolStart 不
// 重复追加也不清进度，ToolEnd 收尾清零。
func TestPublishFoldsLifecycleAndMergesDeltas(t *testing.T) {
	hub := NewHub()
	// 快工具（authority_read 参数极短，可能没有可感知的 delta）：执行起止直达。
	hub.Publish(toolEvent(ToolStart, "authority_read"))
	hub.Publish(toolEvent(ToolEnd, "authority_read"))
	// 慢工具（落笔）：正文 JSON 参数流入 100 段 → 消息完成后才开始执行。
	for i := 0; i < 100; i++ {
		event := toolEvent(ToolDelta, "workspace_put_chapter")
		event.Bytes = 10
		hub.Publish(event)
	}
	snapshot, ok := hub.Snapshot("book-1")
	if !ok || len(snapshot.Entries) != 2 {
		t.Fatalf("entries = %#v ok=%v", snapshot.Entries, ok)
	}
	if !snapshot.Entries[0].Done || snapshot.Entries[1].Done || snapshot.Entries[1].Tool != "workspace_put_chapter" {
		t.Fatalf("entry states = %#v", snapshot.Entries)
	}
	if snapshot.Entries[1].Bytes != 1000 {
		t.Fatalf("entry bytes during generation = %d", snapshot.Entries[1].Bytes)
	}

	// 参数生成完、执行开始：不得出现重复条目，进度停留在最终接收值。
	hub.Publish(toolEvent(ToolStart, "workspace_put_chapter"))
	snapshot, _ = hub.Snapshot("book-1")
	if len(snapshot.Entries) != 2 || snapshot.Entries[1].Bytes != 1000 {
		t.Fatalf("after exec start: entries=%d bytes=%d", len(snapshot.Entries), snapshot.Entries[1].Bytes)
	}

	failed := toolEvent(ToolEnd, "workspace_put_chapter")
	failed.Err = "workspace version conflict"
	hub.Publish(failed)
	snapshot, _ = hub.Snapshot("book-1")
	if !snapshot.Entries[1].Done || snapshot.Entries[1].Err == "" {
		t.Fatalf("settled entry = %#v", snapshot.Entries[1])
	}

	// 一条消息里多个调用：字节按条目归属互不串行；已收尾条目不再吸收
	// 匿名增量（匿名增量归属最近的进行中调用）。
	first := toolEvent(ToolDelta, "workspace_put_candidate")
	first.CallID, first.Bytes = "call-a", 100
	hub.Publish(first)
	second := toolEvent(ToolDelta, "proposal_submit")
	second.CallID, second.Bytes = "call-b", 7
	hub.Publish(second)
	anonymous := toolEvent(ToolDelta, "")
	anonymous.Bytes = 3
	hub.Publish(anonymous)
	snapshot, _ = hub.Snapshot("book-1")
	entries := snapshot.Entries
	if entries[len(entries)-2].Bytes != 100 || entries[len(entries)-1].Bytes != 10 {
		t.Fatalf("per-call bytes = %d, %d", entries[len(entries)-2].Bytes, entries[len(entries)-1].Bytes)
	}

	// 说明性文字折叠为辅助行尾部；构思只是状态位；重试是可见条目。
	retry := toolEvent(Retry, "")
	retry.Attempt = 2
	hub.Publish(retry)
	snapshot, _ = hub.Snapshot("book-1")
	if last := snapshot.Entries[len(snapshot.Entries)-1]; last.Kind != Retry || last.Attempt != 2 {
		t.Fatalf("retry entry = %#v", last)
	}

	// 条目有界：同名工具的多次调用按 CallID 各成一条，超出后丢最旧的。
	for i := 0; i < maxEntries*2; i++ {
		event := toolEvent(ToolStart, "workspace_read")
		event.CallID = fmt.Sprintf("call-%d", i)
		hub.Publish(event)
	}
	snapshot, _ = hub.Snapshot("book-1")
	if len(snapshot.Entries) != maxEntries {
		t.Fatalf("bounded entries = %d", len(snapshot.Entries))
	}
}

// 契约 1/6：发布永不阻塞——订阅者完全不消费时，海量发布也立即返回；
// 唤醒信号合并，消费一次即可读到最新快照。
func TestTurnStartMarksWaitingUntilModelResponds(t *testing.T) {
	hub := NewHub()
	turn := toolEvent(TurnStart, "")
	hub.Publish(turn)
	snapshot, _ := hub.Snapshot("book-1")
	if !snapshot.Waiting || !snapshot.WaitingSince.Equal(turn.At) || len(snapshot.Entries) != 0 {
		t.Fatalf("after turn start: waiting=%v since=%v entries=%d", snapshot.Waiting, snapshot.WaitingSince, len(snapshot.Entries))
	}
	thinking := toolEvent(Thinking, "")
	thinking.Text = "先看前情"
	hub.Publish(thinking)
	if snapshot, _ = hub.Snapshot("book-1"); snapshot.Waiting {
		t.Fatal("first delta must end waiting")
	}
	hub.Publish(toolEvent(TurnStart, ""))
	retry := toolEvent(Retry, "")
	retry.Attempt = 2
	hub.Publish(retry)
	if snapshot, _ = hub.Snapshot("book-1"); !snapshot.Waiting || len(snapshot.Entries) != 1 {
		t.Fatalf("retry keeps waiting: waiting=%v entries=%d", snapshot.Waiting, len(snapshot.Entries))
	}
	hub.Publish(toolEvent(ToolStart, "authority_read"))
	if snapshot, _ = hub.Snapshot("book-1"); snapshot.Waiting {
		t.Fatal("tool execution must end waiting")
	}
}

func TestToolEndRecordsDoneAtAndNewRunResets(t *testing.T) {
	hub := NewHub()
	hub.Publish(toolEvent(ToolStart, "authority_read"))
	end := toolEvent(ToolEnd, "authority_read")
	end.At = at().Add(2 * time.Second)
	hub.Publish(end)
	snapshot, _ := hub.Snapshot("book-1")
	if len(snapshot.Entries) != 1 || !snapshot.Entries[0].Done || snapshot.Entries[0].DoneAt.Sub(snapshot.Entries[0].At) != 2*time.Second {
		t.Fatalf("tool end must record DoneAt: %#v", snapshot.Entries)
	}
	next := toolEvent(ToolStart, "authority_read")
	next.RunID = "run-2"
	hub.Publish(next)
	if snapshot, _ = hub.Snapshot("book-1"); len(snapshot.Entries) != 1 || snapshot.Entries[0].Done {
		t.Fatalf("a new run must start from an empty feed: %#v", snapshot.Entries)
	}
}

func TestPublishNeverBlocksWithStalledSubscriber(t *testing.T) {
	hub := NewHub()
	wake, cancel := hub.Subscribe("book-1")
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			hub.Publish(toolEvent(ToolDelta, ""))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on a stalled subscriber")
	}
	<-wake
	if snapshot, ok := hub.Snapshot("book-1"); !ok || snapshot.Seq != 10000 {
		t.Fatalf("snapshot after merged wakeups = %#v ok=%v", snapshot, ok)
	}
}

// 契约 2：取消只退订——channel 关闭（读端 ok=false）、发布端无泄漏、
// 再次取消与继续发布都安全。
func TestCancelClosesChannelWithoutLeak(t *testing.T) {
	hub := NewHub()
	wake, cancel := hub.Subscribe("book-1")
	cancel()
	if _, open := <-wake; open {
		t.Fatal("cancelled subscription channel must be closed")
	}
	cancel() // 幂等
	hub.Publish(toolEvent(ToolStart, "authority_read"))
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.subs) != 0 {
		t.Fatalf("subscriber registry leaked: %#v", hub.subs)
	}
}

// 契约 4：活动按作品隔离——别的作品的发布不唤醒本作品的订阅者。
func TestSubscribeIsScopedToProject(t *testing.T) {
	hub := NewHub()
	wake, cancel := hub.Subscribe("book-1")
	defer cancel()
	hub.Publish(toolEvent(ToolStart, "authority_read")) // book-1
	other := toolEvent(ToolStart, "authority_read")
	other.ProjectID = "book-2"
	hub.Publish(other)
	<-wake
	select {
	case <-wake:
		t.Fatal("publish to another project must not wake this subscriber")
	default:
	}
}

// 新一轮创作：旧快照作废，上一轮条目不得冒充当前进展；Seq 保持递增。
func TestNewRunResetsSnapshot(t *testing.T) {
	hub := NewHub()
	hub.Publish(toolEvent(ToolStart, "authority_read"))
	previous, _ := hub.Snapshot("book-1")
	next := toolEvent(ToolStart, "workspace_read")
	next.RunID, next.OperationID = "run-2", "op-9"
	hub.Publish(next)
	snapshot, _ := hub.Snapshot("book-1")
	if snapshot.RunID != "run-2" || len(snapshot.Entries) != 1 || snapshot.Entries[0].Tool != "workspace_read" {
		t.Fatalf("snapshot after new run = %#v", snapshot)
	}
	if snapshot.Seq <= previous.Seq {
		t.Fatalf("seq must keep increasing: %d -> %d", previous.Seq, snapshot.Seq)
	}
	// 快照是副本：改动返回值不影响 Hub 内部状态。
	snapshot.Entries[0].Tool = "tampered"
	fresh, _ := hub.Snapshot("book-1")
	if fresh.Entries[0].Tool != "workspace_read" {
		t.Fatal("snapshot must be a copy")
	}
}

func TestPublishProseStallAppendsWarningEntry(t *testing.T) {
	hub := NewHub()
	stall := toolEvent(ProseStall, "workspace_put_chapter")
	stall.CallID = "call-1"
	hub.Publish(stall)
	snapshot, _ := hub.Snapshot("book-1")
	if len(snapshot.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(snapshot.Entries))
	}
	entry := snapshot.Entries[0]
	if entry.Kind != ProseStall || entry.CallID != "call-1" || !entry.Done {
		t.Fatalf("stall entry = %+v", entry)
	}
}
