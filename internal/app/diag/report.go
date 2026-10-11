// Package diag interprets persisted execution facts without changing them.
package diag

import (
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// FormatVersion 3：执行记录拆成尝试摘要与对话记录（D72），事件流退出报告。
const FormatVersion = 3
const RulesVersion = 1

type Request struct {
	ProjectID   string `json:"project_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	After       string `json:"after,omitempty"`
	// TranscriptAfter 是选定任务对话记录的翻页游标。
	TranscriptAfter int64 `json:"transcript_after,omitempty"`
}

type Header struct {
	FormatVersion    int            `json:"format_version"`
	RulesVersion     int            `json:"rules_version"`
	BuildVersion     string         `json:"build_version"`
	Platform         string         `json:"platform"`
	SchemaVersion    int            `json:"schema_version,omitempty"`
	CapturedAt       time.Time      `json:"captured_at"`
	Scope            Request        `json:"scope"`
	Revision         model.Revision `json:"revision"`
	RunEventBoundary int64          `json:"run_event_boundary"`
	Shareable        bool           `json:"shareable"`
	DatabaseIssue    string         `json:"database_issue,omitempty"`
	Selection        string         `json:"selection"`
}

type Summary struct {
	State          string     `json:"state"`
	Reason         string     `json:"reason,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	UpdatedAt      *time.Time `json:"updated_at,omitempty"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
}

type Finding struct {
	Code        string     `json:"code"`
	Severity    string     `json:"severity"`
	Certainty   string     `json:"certainty"`
	OperationID string     `json:"operation_id,omitempty"`
	Attempt     int        `json:"attempt,omitempty"`
	Count       int        `json:"count"`
	Observed    string     `json:"observed"`
	Suggestion  string     `json:"suggestion"`
	Evidence    []Evidence `json:"evidence,omitempty"`
}

type Evidence struct {
	Source      string `json:"source"`
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`
}

type Metrics struct {
	Operations        int                          `json:"operations"`
	States            map[model.OperationState]int `json:"states"`
	Attempts          int                          `json:"attempts"`
	RetriedOperations int                          `json:"retried_operations"`
	ToolErrors        int                          `json:"tool_errors"`
	// Usage 是所选运行或任务全部尝试的模型用量，含中断与恢复的尝试。
	Usage model.Usage `json:"usage,omitzero"`
}

type Coverage struct {
	Source string `json:"source"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type Operation struct {
	ID           string               `json:"id"`
	RunID        string               `json:"run_id"`
	Kind         model.OperationKind  `json:"kind"`
	State        model.OperationState `json:"state"`
	Attempt      int                  `json:"attempt"`
	FailureCode  model.FailureCode    `json:"failure_code,omitempty"`
	Error        string               `json:"error,omitempty"`
	Executor     string               `json:"executor"`
	ConfigDigest string               `json:"config_digest"`
	LeaseUntil   *time.Time           `json:"lease_until,omitempty"`
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
}

// TranscriptEntry 是选定任务的一条对话记录，只出现在本地报告里，从不进入 ShareReport。
type TranscriptEntry struct {
	Sequence         int64                `json:"sequence"`
	Attempt          int                  `json:"attempt"`
	Kind             model.TranscriptKind `json:"kind"`
	CreatedAt        time.Time            `json:"created_at"`
	Text             string               `json:"text"`
	PayloadTruncated bool                 `json:"payload_truncated,omitempty"`
}

type Report struct {
	Header     Header      `json:"header"`
	Summary    Summary     `json:"summary"`
	Findings   []Finding   `json:"findings"`
	Metrics    Metrics     `json:"metrics"`
	Coverage   []Coverage  `json:"coverage"`
	Operations []Operation `json:"operations"`
	// Attempts 是所列任务的尝试摘要，最近有进展的在前；原始错误只在选定任务后保留。
	Attempts               []model.OperationAttempt `json:"attempts"`
	Transcript             []TranscriptEntry        `json:"transcript,omitempty"`
	NextOperationID        string                   `json:"next_operation_id,omitempty"`
	NextTranscriptSequence int64                    `json:"next_transcript_sequence,omitempty"`
}
