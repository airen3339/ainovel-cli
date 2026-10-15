package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// 创作团队的用量取自尝试摘要：进程中断的尝试只到最后一次进展；崩溃后只做合规核对的尝试
// 沿用任务的角色；租约过期的尝试不再算正在执行；没有 Agent 会话的任务与别的书不混进来。
func TestProjectAttemptsReadAttemptSummaries(t *testing.T) {
	s := openOperationStore(t)
	ctx := context.Background()
	start := operationTime()
	if _, err := s.CreateOperation(ctx, testOperation("plan", 1, start)); err != nil {
		t.Fatalf("create: %v", err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		at := start.Add(time.Duration(attempt-1) * 3 * time.Minute)
		if attempt > 1 {
			if _, err := s.RecoverExpiredOperations(ctx, at.Add(-time.Second)); err != nil {
				t.Fatalf("recover: %v", err)
			}
		}
		if claimed, err := s.ClaimNextOperation(ctx, "worker", time.Minute, at); err != nil || claimed.Attempt != attempt {
			t.Fatalf("claim %d = %#v, %v", attempt, claimed, err)
		}
	}
	other := testOperation("elsewhere", 1, start)
	other.RunID, other.Target.ID = "run:book-2", "book-2"
	if _, err := s.CreateCreationRun(ctx, model.CreationRun{
		ID: other.RunID, ProjectID: "book-2", Goal: model.NovelGoal{TargetChapters: 3}.Goal(),
		Strategy: model.CreationRunStrategy{AutoRepairBudget: 3},
		Preset:   model.CreationRunPreset{Source: "test", Digest: "test-preset", Approval: model.ApprovalAuto},
		State:    model.RunRunning, CreatedAt: start, UpdatedAt: start,
	}); err != nil {
		t.Fatalf("create other run: %v", err)
	}
	for _, operation := range []model.Operation{other, testOperation("external", 1, start)} {
		if _, err := s.CreateOperation(ctx, operation); err != nil {
			t.Fatalf("create %s: %v", operation.ID, err)
		}
		if _, err := s.ClaimOperationForExecutor(ctx, operation.ID, "worker", testExecutor, time.Minute, start); err != nil {
			t.Fatalf("claim %s: %v", operation.ID, err)
		}
	}
	bind := func(operationID string, attempt int, role, modelName string, at time.Time) {
		t.Helper()
		if err := s.RecordAgentBinding(ctx, operationID, attempt, model.AgentBinding{Role: role, Provider: "p", Model: modelName, Digest: "d"}, at); err != nil {
			t.Fatalf("bind %s/%d: %v", operationID, attempt, err)
		}
	}
	record := func(operationID string, attempt int, kind model.TranscriptKind, usage model.Usage, at time.Time) {
		t.Helper()
		if _, err := s.AppendTranscript(ctx, model.TranscriptEntry{
			OperationID: operationID, Attempt: attempt, Kind: kind, Payload: json.RawMessage(`{}`), CreatedAt: at,
		}, usage, 0); err != nil {
			t.Fatalf("record %s/%d: %v", operationID, attempt, err)
		}
	}
	bind("plan", 1, "architect", "old-model", start)
	record("plan", 1, model.TranscriptMessage, model.Usage{Input: 100, Output: 10, CacheRead: 50, Cost: 0.1}, start.Add(20*time.Second))
	record("plan", 1, model.TranscriptMessage, model.Usage{Input: 100, Output: 10, CacheRead: 50, Cost: 0.1}, start.Add(40*time.Second))
	record("plan", 1, model.TranscriptJudgment, model.Usage{Input: 7, Output: 1}, start.Add(61*time.Second))
	bind("plan", 2, "architect", "new-model", start.Add(3*time.Minute))
	record("plan", 2, model.TranscriptMessage, model.Usage{Input: 30, Output: 5, Cost: 0.03}, start.Add(4*time.Minute))
	record("plan", 3, model.TranscriptJudgment, model.Usage{Input: 9, Output: 2}, start.Add(6*time.Minute+20*time.Second))
	bind("elsewhere", 1, "writer", "other", start)
	record("elsewhere", 1, model.TranscriptMessage, model.Usage{Input: 999}, start.Add(time.Minute))

	leased := start.Add(6*time.Minute + 30*time.Second) // 第 3 次尝试的租约到 start+7m
	attempts, err := s.ProjectAttempts(ctx, "book-1", leased)
	if err != nil || len(attempts) != 3 {
		t.Fatalf("attempts = %+v, %v", attempts, err)
	}
	ended, crashed, judged := attempts[0], attempts[1], attempts[2]
	if ended.Attempt != 1 || ended.Role != "architect" || ended.Model != "old-model" || ended.Live ||
		ended.Usage != (model.Usage{Input: 207, Output: 21, CacheRead: 100, Cost: 0.2}) ||
		!ended.StartedAt.Equal(start) || !ended.ActiveAt.Equal(start.Add(61*time.Second)) {
		t.Fatalf("ended attempt = %+v", ended)
	}
	if crashed.Attempt != 2 || crashed.Model != "new-model" || crashed.Live ||
		crashed.Usage != (model.Usage{Input: 30, Output: 5, Cost: 0.03}) || !crashed.ActiveAt.Equal(start.Add(4*time.Minute)) {
		t.Fatalf("crashed attempt = %+v", crashed)
	}
	if judged.Attempt != 3 || judged.Role != "architect" || judged.Model != "" || !judged.Live ||
		judged.State != model.OperationRunning || judged.Usage != (model.Usage{Input: 9, Output: 2}) {
		t.Fatalf("judged attempt = %+v", judged)
	}
	// 进程中断：任务还标着执行中，但租约已过期，用时不能一直涨到此刻。
	expired, err := s.ProjectAttempts(ctx, "book-1", start.Add(8*time.Minute))
	if err != nil || expired[2].Live || expired[2].State != model.OperationRunning {
		t.Fatalf("expired attempt = %+v, %v", expired, err)
	}
}
