package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	domainmodel "github.com/voocel/ainovel-cli/internal/domain/model"
)

// 退出时先取消再等在途创作：跟踪的命令没结束 wait 就超时，结束后立刻返回。
func TestInflightWaitsForTrackedCommands(t *testing.T) {
	var f inflight
	release := make(chan struct{})
	cmd := f.track(func() tea.Msg { <-release; return nil })
	go cmd()
	if f.wait(20 * time.Millisecond) {
		t.Fatal("wait must time out while the command is still running")
	}
	close(release)
	if !f.wait(time.Second) {
		t.Fatal("wait must return once tracked commands finish")
	}
}

// 创作驱动命令必须被跟踪：否则退出时无人等它把任务放回队列。
func TestQuickWriteCommandIsTrackedUntilItReturns(t *testing.T) {
	m := studioModel(t, 150, 40)
	cmd := m.startQuickWriteCmd(quickParams{projectID: "letters", premise: "亡者来信", chapters: 8})
	if m.inflight.wait(10 * time.Millisecond) {
		t.Fatal("quick write must be tracked before it runs")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	<-done
	if !m.inflight.wait(time.Second) {
		t.Fatal("quick write must be released once it returns")
	}
}

// 被占用的租约用创作语言解释，不露原始英文。
func TestHeldLeaseIsExplainedInCreationLanguage(t *testing.T) {
	m := studioModel(t, 150, 40)
	updated, _ := m.Update(quickDoneMsg{gen: m.bench.gen, err: fmt.Errorf("op: %w", domainmodel.ErrOperationHeld)})
	m = updated.(model)
	if !strings.Contains(m.bench.err, "租约") || !strings.Contains(m.bench.err, "回车继续") || strings.Contains(m.bench.err, "held") {
		t.Fatalf("held lease must be explained: %q", m.bench.err)
	}
}

// Ctrl+C 第一次只把底栏提示换成退出确认、提示行上的操作不可点；时限到期恢复原提示，
// 时限内再按一次才退出。
func TestCtrlCAsksForASecondPressBeforeQuitting(t *testing.T) {
	m := studioModel(t, 150, 40)
	m, cmd := press(t, m, tea.KeyCtrlC)
	if view := m.View(); cmd == nil || !strings.Contains(view, "再按一次 Ctrl+C 退出") || strings.Contains(view, "Esc 暂停") {
		t.Fatalf("the first Ctrl+C must only arm the quit: cmd=%v", cmd)
	}
	if _, ok := m.actionAt(1); ok {
		t.Fatal("the hint line shows the quit warning, so the primary action must not be clickable")
	}
	updated, _ := m.Update(quitExpiredMsg{})
	m = updated.(model)
	if view := m.View(); strings.Contains(view, "再按一次") || !strings.Contains(view, "Esc 暂停") {
		t.Fatal("the hint must come back once the window expires")
	}
	m, _ = press(t, m, tea.KeyCtrlC)
	_, cmd = press(t, m, tea.KeyCtrlC)
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("a second Ctrl+C within the window must quit")
	}
}

// Esc 在创作进行中是暂停：先落盘暂停，再打断在途的模型调用；任务放回队列等回车
// 接着写，不算失败、不报错。
func TestEscapePausesAndInterruptsTheModelCall(t *testing.T) {
	ctx := context.Background()
	executor := &testExecutor{started: make(chan struct{}, 1)}
	deps, api := testDeps(t, executor)
	m := newModel(ctx, deps)
	m = typeText(t, m, "一个失忆的邮差替亡者送完最后一封信")
	m, cmd := press(t, m, tea.KeyEnter)
	done := make(chan tea.Msg, 1)
	drive := cmd().(tea.BatchMsg)[0]
	go func() { done <- drive() }()
	<-executor.started
	run, ok, err := api.Runs.LatestCreationRun(ctx, m.bench.projectID)
	if err != nil || !ok || run.State != domainmodel.RunRunning {
		t.Fatalf("run = %+v %v %v", run, ok, err)
	}
	m.bench.snap.Run, m.bench.loaded = &run, true

	m, cmd = press(t, m, tea.KeyEsc)
	if m.page != pageWorkbench || cmd == nil {
		t.Fatal("Esc while writing must pause, not leave")
	}
	updated, _ := m.Update(findMsg[runControlMsg](t, cmd))
	var finished tea.Msg
	select {
	case finished = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pausing must interrupt the model call in flight")
	}
	updated, _ = updated.(model).Update(finished)
	m = updated.(model)
	if m.bench.writing || m.bench.err != "" {
		t.Fatalf("an interrupted drive is not an error: writing=%v err=%q", m.bench.writing, m.bench.err)
	}
	run, _, err = api.Runs.LatestCreationRun(ctx, m.bench.projectID)
	if err != nil || run.State != domainmodel.RunPaused {
		t.Fatalf("run must be paused: %+v %v", run, err)
	}
	operations, err := api.Runs.RunOperations(ctx, run.ID)
	if err != nil || len(operations) != 1 || operations[0].State != domainmodel.OperationQueued {
		t.Fatalf("the interrupted task must go back to the queue: %+v %v", operations, err)
	}
}
