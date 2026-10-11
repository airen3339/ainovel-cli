package diag

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

func TestShareExcludesPrivateFieldsAndPreservesAssociation(t *testing.T) {
	const secret = "PRIVATE_SENTINEL_正文_思考_sk-key_/Users/me/https://private"
	now := time.Now()
	r := Report{
		Header:     Header{BuildVersion: secret, Platform: secret, CapturedAt: now, Scope: Request{ProjectID: secret, RunID: secret, OperationID: secret}},
		Summary:    Summary{State: "failed", Reason: secret},
		Metrics:    Metrics{States: map[model.OperationState]int{model.OperationState(secret): 1}},
		Operations: []Operation{{ID: secret, RunID: secret, Kind: model.OperationKind(secret), State: "failed", Error: secret, Executor: secret, ConfigDigest: secret, FailureCode: model.FailureCode(secret), CreatedAt: now, UpdatedAt: now}},
		Findings:   []Finding{{Code: "execution.failed", Observed: secret, Suggestion: secret, Severity: secret, Certainty: secret, OperationID: secret, Count: 1}, {Code: secret}},
		Coverage:   []Coverage{{"attempts", "partial", secret}, {secret, secret, secret}},
		Attempts: []model.OperationAttempt{{
			OperationID: secret, Attempt: 1, Worker: secret, Outcome: model.OperationFailed, Error: secret, FailureCode: model.FailureCode(secret),
			Agent: model.AgentBinding{Role: secret, Provider: secret, Model: secret, Thinking: secret, Digest: secret}, StartedAt: now, ActiveAt: now,
		}, {OperationID: secret, Outcome: model.OperationState(secret)}},
		Transcript: []TranscriptEntry{{Sequence: 1, Attempt: 1, Kind: model.TranscriptMessage, Text: secret}},
	}
	s := Share(r)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "PRIVATE") {
		t.Fatalf("private data leaked: %s", raw)
	}
	if s.Attempts[0].Operation != s.Operations[0].ID || s.Findings[0].Operation != s.Operations[0].ID {
		t.Fatal("task association was lost")
	}
	if s.Attempts[0].Outcome != "failed" || s.Attempts[0].FailureCode != "unknown" || s.Attempts[1].Outcome != "unknown" {
		t.Fatalf("attempt vocabulary not closed: %+v", s.Attempts)
	}
	if s.Omitted != 2 || s.BuildVersion != "unknown" || s.Metrics.States["unknown"] != 1 {
		t.Fatalf("unknown fields not accounted for: %+v", s)
	}
}

func TestSharePublicationRefusesOverwriteAndCancelledWrite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "diagnostics.json")
	if err := writeShare(ctx, path, ShareReport{Shareable: true}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := writeShare(ctx, path, ShareReport{}); err == nil {
		t.Fatal("overwrote existing report")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing report changed")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	other := filepath.Join(filepath.Dir(path), "cancelled.json")
	if err := writeShare(cancelled, other, ShareReport{}); err == nil {
		t.Fatal("cancelled export succeeded")
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("temporary files remain: %v %v", files, err)
	}
}

func TestRulesDoNotInferFailureFromWaitingOrRetries(t *testing.T) {
	if f := findings(0, 0, 0); len(f) != 0 {
		t.Fatalf("normal scope flagged: %v", f)
	}
	f := findings(1, 1, 1)
	if len(f) != 3 {
		t.Fatalf("findings=%v", f)
	}
	for _, finding := range f {
		if finding.Certainty != "confirmed" {
			t.Fatal(finding)
		}
	}
	if f := findings(0, 0, 2); f[1].Certainty != "signal" {
		t.Fatal("repeat signal claimed root cause")
	}
}

func TestShareReportsOmittedWindow(t *testing.T) {
	r := Report{Metrics: Metrics{Operations: 1000, Attempts: 5000}, Operations: make([]Operation, 50), Attempts: make([]model.OperationAttempt, 200), Findings: []Finding{{Code: "execution.failed", Count: 20, Evidence: make([]Evidence, 2)}}}
	for i := range r.Findings[0].Evidence {
		r.Findings[0].Evidence[i].Source = "operation.state"
	}
	s := Share(r)
	if s.OmittedAttempts != 4800 || s.OmittedTaskDetails != 950 || s.OmittedEvidenceReferences != 18 {
		t.Fatalf("window omissions lost: %+v", s)
	}
}

func TestEnvironmentAndInvalidScopes(t *testing.T) {
	q := New(nil, "v1-dev", nil)
	r, err := q.Inspect(context.Background(), Request{})
	if err != nil || r.Summary.State != "environment" || r.Header.Shareable {
		t.Fatalf("environment: %+v %v", r, err)
	}
	for _, req := range []Request{{RunID: "run"}, {OperationID: "task"}, {ProjectID: "book"}, {TranscriptAfter: 1}, {TranscriptAfter: -1}} {
		if _, err := q.Inspect(context.Background(), req); err == nil {
			t.Fatalf("invalid/unavailable scope accepted: %+v", req)
		}
		if err := q.ExportShare(context.Background(), req, filepath.Join(t.TempDir(), "invalid.json")); err == nil {
			t.Fatalf("export accepted invalid scope: %+v", req)
		}
	}
}
