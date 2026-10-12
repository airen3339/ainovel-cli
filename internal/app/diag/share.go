package diag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/export"
)

// ShareReport is a closed projection: no raw IDs, errors, payloads or free text.
type ShareReport struct {
	FormatVersion             int              `json:"format_version"`
	RulesVersion              int              `json:"rules_version"`
	BuildVersion              string           `json:"build_version"`
	Platform                  string           `json:"platform"`
	SchemaVersion             int              `json:"schema_version,omitempty"`
	Shareable                 bool             `json:"shareable"`
	DatabaseIssue             string           `json:"database_issue,omitempty"`
	Selection                 string           `json:"selection"`
	Project                   string           `json:"project,omitempty"`
	Run                       string           `json:"run,omitempty"`
	State                     string           `json:"state"`
	Metrics                   ShareMetrics     `json:"metrics"`
	Findings                  []ShareFinding   `json:"findings"`
	Coverage                  []ShareCoverage  `json:"coverage"`
	Operations                []ShareOperation `json:"operations"`
	Attempts                  []ShareAttempt   `json:"attempts"`
	Omitted                   int              `json:"omitted_items"`
	OmittedAttempts           int              `json:"omitted_attempts"`
	OmittedTaskDetails        int              `json:"omitted_task_details"`
	OmittedEvidenceReferences int              `json:"omitted_evidence_references"`
}

type ShareMetrics struct {
	Operations        int            `json:"operations"`
	States            map[string]int `json:"states"`
	Attempts          int            `json:"attempts"`
	RetriedOperations int            `json:"retried_operations"`
	ToolErrors        int            `json:"tool_errors"`
	Usage             model.Usage    `json:"usage,omitzero"`
}
type ShareFinding struct {
	Code      string          `json:"code"`
	Severity  string          `json:"severity"`
	Certainty string          `json:"certainty"`
	Operation string          `json:"operation,omitempty"`
	Attempt   int             `json:"attempt,omitempty"`
	Count     int             `json:"count"`
	Evidence  []ShareEvidence `json:"evidence,omitempty"`
}
type ShareEvidence struct {
	Source    string `json:"source"`
	Operation string `json:"operation"`
	Attempt   int    `json:"attempt"`
}
type ShareCoverage struct {
	Source string `json:"source"`
	Status string `json:"status"`
}
type ShareOperation struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	State           string `json:"state"`
	Attempt         int    `json:"attempt"`
	FailureCode     string `json:"failure_code,omitempty"`
	Executor        string `json:"executor"`
	Configuration   string `json:"configuration"`
	CreatedOffsetMS int64  `json:"created_offset_ms"`
	UpdatedOffsetMS int64  `json:"updated_offset_ms"`
}

// ShareAttempt 是尝试摘要的封闭投影：不带错误原文、执行者与模型名称。
type ShareAttempt struct {
	Operation       string      `json:"operation"`
	Attempt         int         `json:"attempt"`
	Outcome         string      `json:"outcome,omitempty"`
	FailureCode     string      `json:"failure_code,omitempty"`
	LeaseExpired    bool        `json:"lease_expired,omitempty"`
	ToolErrors      int         `json:"tool_errors,omitempty"`
	Usage           model.Usage `json:"usage,omitzero"`
	StartedOffsetMS int64       `json:"started_offset_ms"`
	ActiveOffsetMS  int64       `json:"active_offset_ms"`
}

var buildPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$|^v1-dev$`)

func safeBuild(version string) string {
	if buildPattern.MatchString(version) {
		return version
	}
	return "unknown"
}

func allowed(value string, choices ...string) string {
	for _, choice := range choices {
		if value == choice {
			return value
		}
	}
	return "unknown"
}
func safeState(value string) string {
	return allowed(value, "queued", "running", "paused", "awaiting_approval", "succeeded", "failed", "cancelled", "stale", "waiting_user", "completed", "no_run", "environment")
}
func safeKind(value string) string {
	return allowed(value, "develop_plan", "write_chapter", "revise_plan", "revise_canon", "rewrite_chapter", "rewrite_affected", "review_range", "generate_asset", "inspect_asset")
}

// Share reconstructs every string from a fixed vocabulary or a report-local alias.
func Share(report Report) ShareReport {
	s := ShareReport{FormatVersion: FormatVersion, RulesVersion: RulesVersion,
		Selection:     allowed(report.Header.Selection, "paged_details", "issue_context_and_attempts", "environment"),
		DatabaseIssue: databaseIssue(report.Header.DatabaseIssue),
		BuildVersion:  safeBuild(report.Header.BuildVersion), Platform: runtime.GOOS + "/" + runtime.GOARCH,
		SchemaVersion: report.Header.SchemaVersion, Shareable: true, State: safeState(report.Summary.State),
		Metrics:  ShareMetrics{report.Metrics.Operations, map[string]int{}, report.Metrics.Attempts, report.Metrics.RetriedOperations, report.Metrics.ToolErrors, report.Metrics.Usage},
		Findings: []ShareFinding{}, Coverage: []ShareCoverage{}, Operations: []ShareOperation{}, Attempts: []ShareAttempt{}}
	if report.Header.Scope.ProjectID != "" {
		s.Project = "book-1"
	}
	s.OmittedAttempts = max(0, report.Metrics.Attempts-len(report.Attempts))
	s.OmittedTaskDetails = max(0, report.Metrics.Operations-len(report.Operations))
	if report.Header.Scope.RunID != "" {
		s.Run = "run-1"
	}
	aliases := map[string]map[string]string{}
	alias := func(kind, raw string) string {
		if raw == "" {
			return ""
		}
		if aliases[kind] == nil {
			aliases[kind] = map[string]string{}
		}
		if v := aliases[kind][raw]; v != "" {
			return v
		}
		v := fmt.Sprintf("%s-%d", kind, len(aliases[kind])+1)
		aliases[kind][raw] = v
		return v
	}
	for key, count := range report.Metrics.States {
		s.Metrics.States[safeState(string(key))] += count
	}
	for _, op := range report.Operations {
		s.Operations = append(s.Operations, ShareOperation{
			ID: alias("task", op.ID), Kind: safeKind(string(op.Kind)), State: safeState(string(op.State)), Attempt: op.Attempt,
			FailureCode: allowed(string(op.FailureCode), "", "result_unknown", "submission_blocked", "model_unreachable", "model_busy", "model_refused"), Executor: alias("executor", op.Executor), Configuration: alias("config", op.ConfigDigest),
			CreatedOffsetMS: offset(op.CreatedAt, report.Header.CapturedAt), UpdatedOffsetMS: offset(op.UpdatedAt, report.Header.CapturedAt)})
	}
	for _, f := range report.Findings {
		template := finding(f.Code, f.Count)
		if template.Code == "unknown" {
			s.Omitted++
			continue
		}
		shared := ShareFinding{Code: template.Code, Severity: template.Severity, Certainty: template.Certainty, Operation: alias("task", f.OperationID), Attempt: f.Attempt, Count: f.Count}
		s.OmittedEvidenceReferences += max(0, f.Count-len(f.Evidence))
		for _, e := range f.Evidence {
			source := allowed(e.Source, "operation.state", "operation.lease", "operation.failure_code", "attempt.tool_errors")
			if source == "unknown" {
				s.Omitted++
				s.OmittedEvidenceReferences++
				continue
			}
			shared.Evidence = append(shared.Evidence, ShareEvidence{source, alias("task", e.OperationID), e.Attempt})
		}
		s.Findings = append(s.Findings, shared)
	}
	for _, c := range report.Coverage {
		source := allowed(c.Source, "database", "statistics", "operations", "attempts", "transcript", "tool_errors", "usage", "environment", "content_commits", "models", "findings")
		if source == "unknown" {
			s.Omitted++
			continue
		}
		s.Coverage = append(s.Coverage, ShareCoverage{source, allowed(c.Status, "complete", "partial", "unavailable", "not_collected", "truncated")})
	}
	for _, a := range report.Attempts {
		outcome := ""
		if a.Outcome != "" {
			outcome = safeState(string(a.Outcome))
		}
		s.Attempts = append(s.Attempts, ShareAttempt{
			Operation: alias("task", a.OperationID), Attempt: a.Attempt, Outcome: outcome,
			FailureCode: allowed(string(a.FailureCode), "", "result_unknown", "submission_blocked", "model_unreachable", "model_busy", "model_refused"), LeaseExpired: a.LeaseExpired,
			ToolErrors: a.ToolErrors, Usage: a.Usage,
			StartedOffsetMS: offset(a.StartedAt, report.Header.CapturedAt), ActiveOffsetMS: offset(a.ActiveAt, report.Header.CapturedAt)})
	}
	return s
}

func offset(at, captured time.Time) int64 { return at.Sub(captured).Milliseconds() }

func (q *Query) ExportShare(ctx context.Context, request Request, path string) error {
	report, err := q.inspect(ctx, request, true)
	if err != nil {
		return err
	}
	return writeShare(ctx, path, Share(report))
}

// Exclusive publication: an existing report is never replaced.
func writeShare(ctx context.Context, path string, report ShareReport) error {
	if path == "" {
		return fmt.Errorf("diagnostic output path is required")
	}
	err := export.Publish(ctx, path, false, func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	})
	if err != nil {
		return fmt.Errorf("publish diagnostic file: %w", err)
	}
	return nil
}
