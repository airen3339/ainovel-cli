package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

func seedDiagProject(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO authority_streams(target_kind,target_id,current_revision) VALUES ('project','book-1',1)`); err != nil {
		t.Fatal(err)
	}
}

// claimForDiag 领取任务建立尝试摘要，按 records 依次写入对话记录（用量、工具报错）。
func claimForDiag(t *testing.T, s *Store, id string) model.Operation {
	t.Helper()
	claimed, err := s.ClaimOperationForExecutor(context.Background(), id, "worker", testExecutor, time.Minute, operationTime())
	if err != nil {
		t.Fatal(err)
	}
	return claimed
}

func transcribe(t *testing.T, s *Store, operationID string, attempt int, payload []byte, usage model.Usage, toolErrors int) {
	t.Helper()
	if _, err := s.AppendTranscript(context.Background(), model.TranscriptEntry{
		OperationID: operationID, Attempt: attempt, Kind: model.TranscriptMessage, Payload: payload, CreatedAt: operationTime(),
	}, usage, toolErrors); err != nil {
		t.Fatal(err)
	}
}

func TestDiagScopePaginationAndPayloadBudget(t *testing.T) {
	s := openOperationStore(t)
	seedDiagProject(t, s)
	ctx := context.Background()
	for i := 0; i < 55; i++ {
		if _, err := s.CreateOperation(ctx, testOperation(fmt.Sprintf("op-%03d", i), 0, operationTime())); err != nil {
			t.Fatal(err)
		}
	}
	claimForDiag(t, s, "op-000")
	payload := []byte(`"` + strings.Repeat("sensitive", 20000) + `"`)
	for i := 1; i <= 205; i++ {
		transcribe(t, s, "op-000", 1, payload, model.Usage{}, 0)
	}
	snap, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.OperationCount != 55 || len(snap.Operations) != 50 || snap.NextOperationID != "op-049" ||
		len(snap.Attempts) != 1 || snap.Attempts[0].OperationID != "op-000" || len(snap.Transcript) != 0 {
		t.Fatalf("unexpected pagination: %+v", snap)
	}
	next, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1", AfterOperationID: snap.NextOperationID})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Operations) != 5 || next.NextOperationID != "" || next.OperationCount != 55 || len(next.Attempts) != 0 {
		t.Fatalf("unexpected final page: %+v", next)
	}
	selected, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1", OperationID: "op-000"})
	if err != nil {
		t.Fatal(err)
	}
	if selected.OperationCount != 1 || len(selected.Transcript) != 100 || selected.NextTranscriptSequence != 100 || !selected.TranscriptTruncated {
		t.Fatalf("unexpected transcript page: %+v", selected)
	}
	if len(selected.Transcript[0].Payload) != 65536 || !selected.Transcript[0].PayloadTruncated {
		t.Fatal("payload budget was not applied")
	}
	end, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1", OperationID: "op-000", AfterTranscriptSequence: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(end.Transcript) != 5 || end.NextTranscriptSequence != 0 || end.TranscriptTruncated {
		t.Fatal("incorrect final transcript page")
	}
	shared, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1", OperationID: "op-000", ForShare: true})
	if err != nil || len(shared.Transcript) != 0 || len(shared.Attempts) != 1 {
		t.Fatalf("share read transcript: %+v %v", shared, err)
	}
	for _, req := range []DiagRequest{{ProjectID: "book-1", RunID: "other", OperationID: "op-000"}, {ProjectID: "book-1", AfterOperationID: "other"}, {ProjectID: "book-1", AfterTranscriptSequence: 1}} {
		if _, err = s.ReadDiagSnapshot(ctx, req); !errors.Is(err, model.ErrInvalid) {
			t.Fatalf("scope accepted: %+v %v", req, err)
		}
	}
	claimed := claimForDiag(t, s, "op-054")
	if _, err := s.FailOperation(ctx, "op-054", claimed.Attempt, "", "boom", operationTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	// 正常任务更晚有进展：尝试仍跟随任务的先后，失败任务的尝试不被挤到后面。
	later := operationTime().Add(2 * time.Second)
	normal, err := s.ClaimOperationForExecutor(ctx, "op-001", "worker", testExecutor, time.Minute, later)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConcludeOperation(ctx, "op-001", normal.Attempt, model.OperationSucceeded, "", later.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	prioritized, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1", ForShare: true})
	if err != nil {
		t.Fatal(err)
	}
	if prioritized.Operations[0].ID != "op-054" || prioritized.Attempts[0].OperationID != "op-054" || prioritized.Attempts[0].Outcome != model.OperationFailed {
		t.Fatalf("share lost failure context: %+v", prioritized.Attempts)
	}
}

// 用量取尝试摘要全量加总：跨尝试相加（中断的尝试也算上），选定任务时只算这个任务，各次
// 尝试带各自的用量。
func TestDiagSumsUsageAcrossAttempts(t *testing.T) {
	s := openOperationStore(t)
	seedDiagProject(t, s)
	ctx := context.Background()
	for _, id := range []string{"op-a", "op-b"} {
		if _, err := s.CreateOperation(ctx, testOperation(id, 0, operationTime())); err != nil {
			t.Fatal(err)
		}
		claimForDiag(t, s, id)
	}
	transcribe(t, s, "op-a", 1, []byte(`{}`), model.Usage{Input: 100, Output: 10, CacheRead: 60, Cost: 0.1}, 0) // 第 1 次尝试中断
	if _, err := s.RecoverExpiredOperations(ctx, operationTime().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	claimForDiag(t, s, "op-a")
	transcribe(t, s, "op-a", 2, []byte(`{}`), model.Usage{Input: 50, Output: 5}, 0)
	transcribe(t, s, "op-a", 2, []byte(`{}`), model.Usage{Input: 7, Output: 1}, 0)
	transcribe(t, s, "op-b", 1, []byte(`{}`), model.Usage{Input: 1000}, 0)

	run, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Usage != (model.Usage{Input: 1157, Output: 16, CacheRead: 60, Cost: 0.1}) {
		t.Fatalf("diag run usage = %+v", run.Usage)
	}
	task, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1", OperationID: "op-a"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Usage != (model.Usage{Input: 157, Output: 16, CacheRead: 60, Cost: 0.1}) || len(task.Attempts) != 2 {
		t.Fatalf("task usage = %+v attempts = %+v", task.Usage, task.Attempts)
	}
	for _, attempt := range task.Attempts {
		if attempt.Attempt == 1 && (!attempt.LeaseExpired || attempt.Usage.Input != 100) {
			t.Fatalf("interrupted attempt = %+v", attempt)
		}
	}
}

// 工具报错在写入时计入尝试摘要，运行级全量统计、按尝试聚合，不读对话记录。回归的样子是
// 这里归零而选中任务时才有数——那正是用户看不到错误的那个缺口。
func TestDiagCountsToolErrorsFromAttemptSummaries(t *testing.T) {
	s := openOperationStore(t)
	seedDiagProject(t, s)
	ctx := context.Background()
	for _, id := range []string{"op-a", "op-b"} {
		if _, err := s.CreateOperation(ctx, testOperation(id, 0, operationTime())); err != nil {
			t.Fatal(err)
		}
		claimForDiag(t, s, id)
	}
	transcribe(t, s, "op-a", 1, []byte(`{}`), model.Usage{}, 1)
	transcribe(t, s, "op-a", 1, []byte(`{}`), model.Usage{}, 1)
	if _, err := s.ConcludeOperation(ctx, "op-a", 1, model.OperationQueued, "released", operationTime()); err != nil {
		t.Fatal(err)
	}
	claimForDiag(t, s, "op-a")
	transcribe(t, s, "op-a", 2, []byte(`{}`), model.Usage{}, 1) // 同一任务的另一次尝试单独成组
	transcribe(t, s, "op-a", 2, []byte(`{}`), model.Usage{}, 0)
	transcribe(t, s, "op-b", 1, []byte(`{}`), model.Usage{}, 1)

	snap, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.ToolErrorCount != 4 || len(snap.Transcript) != 0 {
		t.Fatalf("工具错误计数 = %d, want 4", snap.ToolErrorCount)
	}
	got := map[string]int{}
	for _, group := range snap.ToolErrors {
		got[fmt.Sprintf("%s#%d", group.OperationID, group.Attempt)] = group.Count
	}
	want := map[string]int{"op-a#1": 2, "op-a#2": 1, "op-b#1": 1}
	if len(got) != len(want) {
		t.Fatalf("聚合分组 = %+v, want %v", snap.ToolErrors, want)
	}
	for key, count := range want {
		if got[key] != count {
			t.Errorf("%s = %d, want %d", key, got[key], count)
		}
	}
}

func TestDiagEmptyProjectAndReadOnlyOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "diag.db")
	if _, err := OpenReadOnly(ctx, path); err == nil {
		t.Fatal("missing database opened")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readonly open created file: %v", err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seedDiagProject(t, s)
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	empty, err := ro.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1"})
	if err != nil || empty.Run != nil || empty.OperationCount != 0 {
		t.Fatalf("empty project: %+v %v", empty, err)
	}
	if _, err = ro.db.Exec(`DELETE FROM authority_streams`); err == nil {
		t.Fatal("read-only connection allowed mutation")
	}
	if _, err = ro.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "missing"}); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("missing project: %v", err)
	}
	if _, err = s.db.Exec(`PRAGMA user_version=999`); err != nil {
		t.Fatal(err)
	}
	if unsupported, err := OpenReadOnly(ctx, path); err == nil {
		unsupported.Close()
		t.Fatal("unsupported schema accepted")
	}
}

func TestDiagAggregatesCoverHiddenTasksAndCurrentAttempt(t *testing.T) {
	s := openOperationStore(t)
	seedDiagProject(t, s)
	ctx := context.Background()
	for i := 0; i < 55; i++ {
		if _, err := s.CreateOperation(ctx, testOperation(fmt.Sprintf("op-%03d", i), 0, operationTime())); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.db.Exec(`UPDATE operations SET state='failed',attempt=2,executor='llm.agent@1',failure_code='result_unknown' WHERE id='op-054'`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`UPDATE operations SET state='running',attempt=1,lease_owner='owner',lease_until_unix_ms=1 WHERE id='op-053'`)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts[model.OperationFailed] != 1 || snap.AttemptCount != 3 || snap.RetriedOperationCount != 1 || snap.ExpiredLeaseCount != 1 || snap.ResultUnknownCount != 1 {
		t.Fatalf("scope aggregate missed hidden tasks: %+v", snap)
	}
	_, err = s.db.Exec(`UPDATE creation_runs SET state='completed' WHERE id='run:book-1'`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO creation_runs SELECT 'new-run',content_digest,project_id,goal,strategy,preset,'running','',0,'',created_at_unix_ms+1,updated_at_unix_ms FROM creation_runs WHERE id='run:book-1'`)
	if err != nil {
		t.Fatal(err)
	}
	historical, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book-1", OperationID: "op-054"})
	if err != nil {
		t.Fatal(err)
	}
	if historical.Run.ID != "run:book-1" {
		t.Fatal("operation scope selected latest run instead of historical run")
	}
}

func BenchmarkDiagSnapshot(b *testing.B) {
	for _, count := range []int{500, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			ctx := context.Background()
			s, err := Open(ctx, filepath.Join(b.TempDir(), "diag.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				b.Fatal(err)
			}
			defer tx.Rollback()
			for _, statement := range []string{
				`INSERT INTO authority_streams(target_kind,target_id,current_revision) VALUES ('project','book',1)`,
				`INSERT INTO creation_runs(id,content_digest,project_id,goal,strategy,preset,state,created_at_unix_ms,updated_at_unix_ms) VALUES ('run','digest','book',x'7b7d',x'7b7d',x'7b7d','running',1,1)`,
			} {
				if _, err = tx.Exec(statement); err != nil {
					b.Fatal(err)
				}
			}
			payload := []byte(strings.Repeat("private-content-", 4096))
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("op-%04d", i)
				if _, err = tx.Exec(`INSERT INTO operations(id,content_digest,kind,target_kind,target_id,priority,state,attempt,execution_snapshot,input,executor,run_id,created_at_unix_ms,updated_at_unix_ms) VALUES (?,'digest','write_chapter','project','book',0,'succeeded',1,?,?,'llm.agent@1','run',1,1)`, id, []byte(`{"config_digest":"profile"}`), payload); err != nil {
					b.Fatal(err)
				}
				if _, err = tx.Exec(`INSERT INTO operation_attempts(operation_id,attempt,worker,outcome,started_at_unix_ms,active_at_unix_ms) VALUES (?,1,'worker','succeeded',1,1)`, id); err != nil {
					b.Fatal(err)
				}
				if _, err = tx.Exec(`INSERT INTO operation_transcripts(operation_id,sequence,attempt,kind,payload,created_at_unix_ms) VALUES (?,1,1,'message',?,1)`, id, payload); err != nil {
					b.Fatal(err)
				}
			}
			if err = tx.Commit(); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				snap, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book"})
				if err != nil {
					b.Fatal(err)
				}
				if snap.OperationCount != count || len(snap.Operations) != 50 || len(snap.Attempts) != 50 || len(snap.Transcript) != 0 {
					b.Fatal("unbounded diagnostic projection")
				}
			}
		})
	}
}
