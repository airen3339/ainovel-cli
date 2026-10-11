package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Each operation carries three attempts and a substantial event history. Fixture
// construction is outside timing: the benchmark measures an interactive read.
func seedDiagHistory(tb testing.TB, count int) *Store {
	tb.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(tb.TempDir(), "history.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`INSERT INTO authority_streams(target_kind,target_id,current_revision) VALUES ('project','book',1)`,
		`INSERT INTO creation_runs(id,content_digest,project_id,goal,strategy,preset,state,created_at_unix_ms,updated_at_unix_ms) VALUES ('run','digest','book',x'7b7d',x'7b7d',x'7b7d','running',1,1)`,
	} {
		if _, err = tx.Exec(statement); err != nil {
			tb.Fatal(err)
		}
	}
	op, err := tx.Prepare(`INSERT INTO operations(id,content_digest,kind,target_kind,target_id,priority,state,attempt,execution_snapshot,input,executor,run_id,created_at_unix_ms,updated_at_unix_ms) VALUES (?,'digest','write_chapter','project','book',0,'succeeded',3,?,x'7b7d','llm.agent@1','run',1,200)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer op.Close()
	attempt, err := tx.Prepare(`INSERT INTO operation_attempts(operation_id,attempt,worker,agent_role,agent_model,input_tokens,output_tokens,outcome,started_at_unix_ms,active_at_unix_ms) VALUES (?,?,'worker','writer','model',?,?,?,?,?)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer attempt.Close()
	entry, err := tx.Prepare(`INSERT INTO operation_transcripts(operation_id,sequence,attempt,kind,payload,created_at_unix_ms) VALUES (?,?,?,'message',?,?)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer entry.Close()
	message := []byte(`{"role":"assistant","blocks":[{"type":"text","text":"` + strings.Repeat("content ", 256) + `"}]}`)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("op-%04d", i)
		if _, err = op.Exec(id, []byte(`{"executor":"llm.agent@1","config_digest":"profile"}`)); err != nil {
			tb.Fatal(err)
		}
		for n := 1; n <= 3; n++ {
			outcome := "failed"
			if n == 3 {
				outcome = "succeeded"
			}
			if _, err = attempt.Exec(id, n, 6600, 660, outcome, n*100, n*100+66); err != nil {
				tb.Fatal(err)
			}
		}
		for sequence := 1; sequence <= 198; sequence++ {
			if _, err = entry.Exec(id, sequence, 1+(sequence-1)/66, message, sequence); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return s
}

func BenchmarkDiagLongHistory(b *testing.B) {
	for _, count := range []int{500, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := seedDiagHistory(b, count)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				snap, err := s.ReadDiagSnapshot(context.Background(), DiagRequest{ProjectID: "book"})
				if err != nil {
					b.Fatal(err)
				}
				if snap.OperationCount != count || snap.AttemptCount != count*3 || len(snap.Operations) != 50 || len(snap.Attempts) != 150 ||
					len(snap.Transcript) != 0 || snap.Usage.Input != count*3*6600 {
					b.Fatalf("incorrect long-history projection: operations=%d attempts=%d task page=%d attempt page=%d", snap.OperationCount, snap.AttemptCount, len(snap.Operations), len(snap.Attempts))
				}
			}
		})
	}
}

func TestDiagHistoryCancelledContext(t *testing.T) {
	s := seedDiagHistory(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ReadDiagSnapshot(ctx, DiagRequest{ProjectID: "book"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query: %v", err)
	}
	// A cancelled read must release its transaction and leave the store usable.
	if snap, err := s.ReadDiagSnapshot(context.Background(), DiagRequest{ProjectID: "book"}); err != nil || len(snap.Attempts) != 9 {
		t.Fatalf("read after cancellation: attempts=%d err=%v", len(snap.Attempts), err)
	}
}

func BenchmarkDiagLongHistoryShare(b *testing.B) {
	s := seedDiagHistory(b, 1000)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		snapshot, err := s.ReadDiagSnapshot(context.Background(), DiagRequest{ProjectID: "book", ForShare: true})
		if err != nil {
			b.Fatal(err)
		}
		if len(snapshot.Operations) != 200 || len(snapshot.Attempts) != 200 || !snapshot.AttemptsTruncated || len(snapshot.Transcript) != 0 {
			b.Fatal("share exceeded its context budget")
		}
		ids := map[string]bool{}
		for _, op := range snapshot.Operations {
			ids[op.ID] = true
		}
		for _, attempt := range snapshot.Attempts {
			if !ids[attempt.OperationID] {
				b.Fatal("attempt lacks task context")
			}
		}
	}
}
