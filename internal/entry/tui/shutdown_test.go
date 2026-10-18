package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/voocel/ainovel-cli/internal/app/chief"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	domainmodel "github.com/voocel/ainovel-cli/internal/domain/model"
)

// 退出时先取消再等在途创作：会话的推进收到取消、把任务放回队列后，等待才返回。
func TestQuitWaitsForTheDriveToReleaseItsTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	executor := &testExecutor{started: make(chan struct{}, 1)}
	deps, api := testDeps(t, executor)
	m := newModel(ctx, deps)
	m = typeText(t, m, "一个失忆的邮差替亡者送完最后一封信")
	m, _ = press(t, m, tea.KeyEnter)
	<-executor.started
	if api.Chief.Wait(20 * time.Millisecond) {
		t.Fatal("waiting must not return while the drive is still running")
	}
	cancel()
	if !api.Chief.Wait(5 * time.Second) {
		t.Fatal("the drive must stop once the interface quits")
	}
	run, _, err := api.Runs.LatestCreationRun(context.Background(), m.bench.projectID)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := runOperations(context.Background(), api, run.ID)
	if err != nil || len(operations) != 1 || operations[0].State != domainmodel.OperationQueued {
		t.Fatalf("the interrupted task must go back to the queue: %+v %v", operations, err)
	}
}

// 被占用的租约用创作语言解释，不露原始英文。
func TestHeldLeaseIsExplainedInCreationLanguage(t *testing.T) {
	m := studioModel(t, 150, 40)
	updated, _ := m.handleDriveEnd(chief.DriveEnd{Err: fmt.Errorf("op: %w", domainmodel.ErrOperationHeld)})
	m = updated.(model)
	if !strings.Contains(m.bench.err, "租约") || !strings.Contains(m.bench.err, "回车继续") || strings.Contains(m.bench.err, "held") {
		t.Fatalf("held lease must be explained: %q", m.bench.err)
	}
}

// Ctrl+C 第一次只把底栏提示换成退出确认、提示行上的操作不可点；时限到期恢复原提示，
// 时限内再按一次才退出。
func TestCtrlCAsksForASecondPressBeforeQuitting(t *testing.T) {
	m := studioModel(t, 150, 40)
	m, cmd := pressKey(t, m, ctrlC)
	if view := ansi.Strip(m.screen()); cmd == nil || !strings.Contains(view, "再按一次 Ctrl+C 退出") || strings.Contains(view, "Esc 暂停") {
		t.Fatalf("the first Ctrl+C must only arm the quit: cmd=%v", cmd)
	}
	if _, ok := m.actionAt(1); ok {
		t.Fatal("the hint line shows the quit warning, so the primary action must not be clickable")
	}
	updated, _ := m.Update(quitExpiredMsg{})
	m = updated.(model)
	if view := ansi.Strip(m.screen()); strings.Contains(view, "再按一次") || !strings.Contains(view, "Esc 暂停") {
		t.Fatal("the hint must come back once the window expires")
	}
	m, _ = pressKey(t, m, ctrlC)
	_, cmd = pressKey(t, m, ctrlC)
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
	m, _ = press(t, m, tea.KeyEnter)
	<-executor.started
	run, ok, err := api.Runs.LatestCreationRun(ctx, m.bench.projectID)
	if err != nil || !ok || run.State != domainmodel.RunRunning {
		t.Fatalf("run = %+v %v %v", run, ok, err)
	}
	m.bench.snap.Run, m.bench.loaded = &run, true

	m, cmd := press(t, m, tea.KeyEsc)
	if m.page != pageWorkbench || cmd == nil {
		t.Fatal("Esc while writing must pause, not leave")
	}
	updated, _ := m.Update(findMsg[runControlMsg](t, cmd))
	m = syncSession(t, updated.(model))
	if m.bench.writing || m.bench.err != "" {
		t.Fatalf("an interrupted drive is not an error: writing=%v err=%q", m.bench.writing, m.bench.err)
	}
	run, _, err = api.Runs.LatestCreationRun(ctx, m.bench.projectID)
	if err != nil || run.State != domainmodel.RunPaused {
		t.Fatalf("run must be paused: %+v %v", run, err)
	}
	operations, err := runOperations(ctx, api, run.ID)
	if err != nil || len(operations) != 1 || operations[0].State != domainmodel.OperationQueued {
		t.Fatalf("the interrupted task must go back to the queue: %+v %v", operations, err)
	}
}

// runOperations 按创建序取一轮创作的全部 Operation。
func runOperations(ctx context.Context, api *bootstrap.App, runID string) ([]domainmodel.Operation, error) {
	ids, err := api.Runs.RunOperationIDs(ctx, runID)
	if err != nil {
		return nil, err
	}
	operations := make([]domainmodel.Operation, len(ids))
	for i, id := range ids {
		if operations[i], err = api.Tasks.Operation(ctx, id); err != nil {
			return nil, err
		}
	}
	return operations, nil
}
