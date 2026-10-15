package bootstrap_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	novelapp "github.com/voocel/ainovel-cli/internal/app/novel"
	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/activity"
)

// TestRunActivityRequiresAttachedHub 守卫活动通道的可选装配：未装配时订阅方
// 得到 ok=false（TUI 静默降级为纯轮询），装配后发布-唤醒-整读贯通。
func TestRunActivityRequiresAttachedHub(t *testing.T) {
	api := newTestApp(openTestStore(t))
	if _, _, ok := api.Workbench.SubscribeRunActivity("book-1"); ok {
		t.Fatal("subscribe without hub must report ok=false")
	}
	if _, ok := api.Workbench.RunActivity("book-1"); ok {
		t.Fatal("activity without hub must report ok=false")
	}
	hub := activity.NewHub()
	api.Workbench.AttachActivityFeed(hub)
	wake, cancel, ok := api.Workbench.SubscribeRunActivity("book-1")
	if !ok || wake == nil {
		t.Fatal("subscribe with hub must work")
	}
	defer cancel()
	hub.Publish(activity.Event{ProjectID: "book-1", RunID: "run-1", Kind: activity.ToolStart, Tool: "authority_read"})
	<-wake
	if snapshot, ok := api.Workbench.RunActivity("book-1"); !ok || snapshot.RunID != "run-1" {
		t.Fatalf("activity snapshot = %#v ok=%v", snapshot, ok)
	}
}

func TestNewIDIsUniqueAndPrefixed(t *testing.T) {
	now := testTime()
	first, second := projectdoc.NewID("book", now), projectdoc.NewID("book", now)
	if !strings.HasPrefix(first, "book-20260818-") || first == second {
		t.Fatalf("ids = %q, %q", first, second)
	}
}

func TestPauseAndCancelCreationRun(t *testing.T) {
	ctx := context.Background()
	authorityStore := openTestStore(t)
	api := newTestApp(authorityStore)
	now := testTime()
	if _, err := api.Projects.CreateProject(ctx, projectdoc.CreateProjectCommand{
		ProjectID: "control-book", ChangeID: "create-control", UserID: "user-1",
		Reason: "运行控制测试", Draft: testProjectDraft(), CreatedAt: now,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	runID := ensureTestRun(t, ctx, authorityStore, "control-book", now)

	paused, err := api.Runs.PauseCreationRun(ctx, runID, now.Add(time.Minute))
	if err != nil || paused.State != model.RunPaused {
		t.Fatalf("pause = %#v, %v", paused, err)
	}
	again, err := api.Runs.PauseCreationRun(ctx, runID, now.Add(2*time.Minute))
	if err != nil || again.State != model.RunPaused {
		t.Fatalf("pause idempotent = %#v, %v", again, err)
	}
	cancelled, err := api.Runs.CancelCreationRun(ctx, runID, now.Add(3*time.Minute))
	if err != nil || cancelled.State != model.RunCancelled {
		t.Fatalf("cancel = %#v, %v", cancelled, err)
	}
	if _, err := api.Runs.PauseCreationRun(ctx, runID, now.Add(4*time.Minute)); err == nil {
		t.Fatal("pausing a cancelled run must fail")
	}
	// 终态落点仍要能被界面呈现：Latest 返回已取消的一轮，Active 则不再返回。
	latest, ok, err := api.Runs.LatestCreationRun(ctx, "control-book")
	if err != nil || !ok || latest.State != model.RunCancelled {
		t.Fatalf("latest run = %#v ok=%v err=%v", latest, ok, err)
	}
	if _, active, err := api.Runs.ActiveCreationRun(ctx, "control-book"); err != nil || active {
		t.Fatalf("cancelled run should not be active: active=%v err=%v", active, err)
	}
}

// TestPauseDuringDriveStopsAfterCurrentStep 守卫写作中控制（页面设计 §5 p/x）：
// 驱动循环执行期间用户暂停，当前步骤完成后优雅收束、不再创建新任务；续跑到完成。
func TestPauseDuringDriveStopsAfterCurrentStep(t *testing.T) {
	ctx := context.Background()
	executor := &scriptedQuickExecutor{now: testTime()}
	api := newQuickTestApp(t, executor)
	command := novelapp.QuickWriteCommand{
		ProjectID: "pause-mid-book", UserID: "user-1", Premise: "写作中暂停测试",
		Chapters: 2, WorkerID: "quick-worker", LeaseDuration: time.Minute, CreatedAt: testTime(),
	}
	executor.onExecute = func(operation model.Operation) {
		if operation.Kind == model.OperationWriteChapter {
			if _, err := api.Runs.PauseCreationRun(ctx, operation.RunID, testTime().Add(time.Minute)); err != nil {
				t.Errorf("pause during drive: %v", err)
			}
		}
	}
	result, err := api.Novels.QuickWrite(ctx, command)
	if err != nil {
		t.Fatalf("quick write: %v", err)
	}
	if result.RunState != model.RunPaused {
		t.Fatalf("run state = %v, want paused", result.RunState)
	}
	// 暂停时正在写的第一章这一步已完成入库，之后没有开启新章。
	project, err := api.Projects.Project(ctx, "pause-mid-book", model.InitialRevision)
	if err != nil || len(project.Manuscript) != 1 {
		t.Fatalf("manuscript after pause = %d, %v", len(project.Manuscript), err)
	}
	callsAtPause := executor.calls

	executor.onExecute = nil
	command.CreatedAt = testTime().Add(time.Hour)
	result, err = api.Novels.QuickWrite(ctx, command)
	if err != nil || result.RunState != model.RunCompleted {
		t.Fatalf("resume result = %#v, %v", result, err)
	}
	if executor.calls == callsAtPause {
		t.Fatal("resume did not execute remaining work")
	}
}

// TestCancelDuringDriveWinsOverWaitingSettle 守卫乐观并发的裁决优先级：任务执行
// 期间用户取消，协调器收束时不得把 Run 改回等待态，也不得报状态冲突。
func TestCancelDuringDriveWinsOverWaitingSettle(t *testing.T) {
	ctx := context.Background()
	executor := &scriptedQuickExecutor{now: testTime()}
	api := newQuickTestApp(t, executor)
	command := novelapp.QuickWriteCommand{
		ProjectID: "cancel-mid-book", UserID: "user-1", Premise: "写作中取消测试",
		Chapters: 1, Approval: model.ApprovalManual,
		WorkerID: "quick-worker", LeaseDuration: time.Minute, CreatedAt: testTime(),
	}
	executor.onExecute = func(operation model.Operation) {
		if operation.Kind == model.OperationDevelopPlan {
			if _, err := api.Runs.CancelCreationRun(ctx, operation.RunID, testTime().Add(time.Minute)); err != nil {
				t.Errorf("cancel during drive: %v", err)
			}
		}
	}
	result, err := api.Novels.QuickWrite(ctx, command)
	if err != nil {
		t.Fatalf("quick write: %v", err)
	}
	if result.RunState != model.RunCancelled {
		t.Fatalf("run state = %v, want cancelled", result.RunState)
	}
}

func TestImportProjectCreatesBookFromProjection(t *testing.T) {
	ctx := context.Background()
	api := newTestApp(openTestStore(t))
	now := testTime()
	origin, err := api.Projects.CreateProject(ctx, projectdoc.CreateProjectCommand{
		ProjectID: "origin-book", ChangeID: "create-origin", UserID: "user-1",
		Reason: "导出源", Draft: testProjectDraft(), CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create origin: %v", err)
	}
	projection, err := api.Projects.ExportProject(ctx, origin.ID, origin.Revision)
	if err != nil {
		t.Fatalf("export origin: %v", err)
	}
	projection.ProjectID = "copied-book"

	proposal, err := api.Projects.ImportProject(ctx, "import-copy", "user-1", "导入作品文件", projection, now.Add(time.Minute))
	if err != nil || proposal.ApprovalState != model.ApprovalPending {
		t.Fatalf("import proposal = %#v, %v", proposal, err)
	}
	if _, err := api.Decisions.Approve(ctx, proposal.ID, "user-1", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("approve import: %v", err)
	}
	copied, err := api.Projects.Project(ctx, "copied-book", model.InitialRevision)
	if err != nil || len(copied.Plan) != len(origin.Plan) || len(copied.Canon) != len(origin.Canon) {
		t.Fatalf("copied = %#v, %v", copied, err)
	}
}

func TestDeleteProjectRemovesAllDataAndAllowsRecreate(t *testing.T) {
	ctx := context.Background()
	executor := &scriptedQuickExecutor{now: testTime()}
	api := newQuickTestApp(t, executor)
	// 跑完两本书让全链数据（蓝图/正文/Canon/审阅/Run/事件/工作区）落库，
	// 删除一本必须不伤及另一本。
	for _, projectID := range []string{"delete-book", "keep-book"} {
		if _, err := api.Novels.QuickWrite(ctx, novelapp.QuickWriteCommand{
			ProjectID: projectID, UserID: "user-1", Premise: "删除功能测试",
			Chapters: 2, WorkerID: "quick-worker", LeaseDuration: time.Minute, CreatedAt: testTime(),
		}); err != nil {
			t.Fatalf("quick write %s: %v", projectID, err)
		}
	}
	if err := api.Projects.DeleteProject(ctx, "delete-book"); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	projects, err := api.Workbench.Library(ctx)
	if err != nil || len(projects) != 1 || projects[0].ID != "keep-book" {
		t.Fatalf("projects after delete = %#v, %v", projects, err)
	}
	if _, ok, err := api.Runs.LatestCreationRun(ctx, "delete-book"); err != nil || ok {
		t.Fatalf("deleted project still has runs: ok=%v err=%v", ok, err)
	}
	if project, err := api.Projects.Project(ctx, "keep-book", 0); err != nil || len(project.Manuscript) != 2 {
		t.Fatalf("keep-book after delete = %#v, %v", project, err)
	}
	// 同 ID 从零重跑必须成功：任何表有残留都会撞唯一键或幂等键。
	if _, err := api.Novels.QuickWrite(ctx, novelapp.QuickWriteCommand{
		ProjectID: "delete-book", UserID: "user-1", Premise: "重新来过",
		Chapters: 2, WorkerID: "quick-worker", LeaseDuration: time.Minute, CreatedAt: testTime(),
	}); err != nil {
		t.Fatalf("recreate after delete: %v", err)
	}
	if err := api.Projects.DeleteProject(ctx, "ghost-book"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("deleting missing project = %v, want ErrNotFound", err)
	}
}

func TestDeleteProjectRejectsRunningRun(t *testing.T) {
	ctx := context.Background()
	authorityStore := openTestStore(t)
	api := newTestApp(authorityStore)
	now := testTime()
	if _, err := api.Projects.CreateProject(ctx, projectdoc.CreateProjectCommand{
		ProjectID: "busy-book", ChangeID: "create-busy", UserID: "user-1",
		Reason: "删除守卫测试", Draft: testProjectDraft(), CreatedAt: now,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	runID := ensureTestRun(t, ctx, authorityStore, "busy-book", now)
	if err := api.Projects.DeleteProject(ctx, "busy-book"); !errors.Is(err, model.ErrStateConflict) {
		t.Fatalf("delete with running run = %v, want ErrStateConflict", err)
	}
	if _, err := api.Runs.CancelCreationRun(ctx, runID, now.Add(time.Minute)); err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	if err := api.Projects.DeleteProject(ctx, "busy-book"); err != nil {
		t.Fatalf("delete after cancel: %v", err)
	}
}

// TestRunOperationsListsCreationOrder 守卫诊断下钻的数据源（M3）：按创建序
// 返回本轮全部 Operation——run 事件表只有编排流水，真实错误在 Operation 里。
func TestRunOperationsListsCreationOrder(t *testing.T) {
	ctx := context.Background()
	executor := &scriptedQuickExecutor{now: testTime()}
	api := newQuickTestApp(t, executor)
	result, err := api.Novels.QuickWrite(ctx, novelapp.QuickWriteCommand{
		ProjectID: "ops-book", UserID: "user-1", Premise: "诊断下钻测试",
		Chapters: 1, WorkerID: "quick-worker", LeaseDuration: time.Minute, CreatedAt: testTime(),
	})
	if err != nil || result.RunState != model.RunCompleted {
		t.Fatalf("quick write = %#v, %v", result, err)
	}
	operations, err := runOperations(ctx, api.App, result.RunID)
	if err != nil || len(operations) < 2 {
		t.Fatalf("run operations = %d, %v", len(operations), err)
	}
	if operations[0].Kind != model.OperationDevelopPlan {
		t.Fatalf("first operation kind = %v", operations[0].Kind)
	}
	for _, operation := range operations {
		if operation.RunID != result.RunID {
			t.Fatalf("operation %s belongs to run %s", operation.ID, operation.RunID)
		}
	}
}
