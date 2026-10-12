package diag

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/store"
)

type Query struct {
	store         *store.Store
	version       string
	now           func() time.Time
	databaseIssue string
}

// WithDatabaseIssue preserves a classified startup failure without its private message.
func (q *Query) WithDatabaseIssue(code string) *Query {
	copy := *q
	copy.databaseIssue = databaseIssue(code)
	return &copy
}

func databaseIssue(code string) string {
	return allowed(code, "", "missing", "permission", "corrupt", "unsupported_schema", "open_failed", "path_unavailable")
}

func New(s *store.Store, version string, now func() time.Time) *Query {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if version == "" {
		version = "unknown"
	}
	return &Query{store: s, version: version, now: now}
}

func (q *Query) Inspect(ctx context.Context, request Request) (Report, error) {
	return q.inspect(ctx, request, false)
}

func (q *Query) inspect(ctx context.Context, request Request, share bool) (Report, error) {
	r := Report{
		Header:   Header{FormatVersion: FormatVersion, RulesVersion: RulesVersion, BuildVersion: q.version, Platform: runtime.GOOS + "/" + runtime.GOARCH, CapturedAt: q.now(), Scope: request},
		Findings: []Finding{}, Coverage: []Coverage{}, Operations: []Operation{}, Attempts: []model.OperationAttempt{},
		Metrics: Metrics{States: map[model.OperationState]int{}},
	}
	r.Header.DatabaseIssue = q.databaseIssue
	r.Header.Selection = "paged_details"
	if share {
		r.Header.Selection = "issue_context_and_attempts"
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if request.TranscriptAfter < 0 || (request.TranscriptAfter != 0 && request.OperationID == "") || (request.After != "" && request.OperationID != "") {
		return r, fmt.Errorf("invalid diagnostic cursor: %w", model.ErrInvalid)
	}
	if request.ProjectID == "" {
		if request.RunID != "" || request.OperationID != "" || request.After != "" {
			return r, fmt.Errorf("diagnostic scope requires project: %w", model.ErrInvalid)
		}
		r.Summary.State = "environment"
		r.Header.Selection = "environment"
		r.Coverage = append(r.Coverage, Coverage{"environment", "partial", "仅采集构建版本和平台；未检查模型连通性或读取配置内容。"})
		status := "unavailable"
		if q.store != nil {
			status = "not_collected"
		}
		r.Coverage = append(r.Coverage, Coverage{"database", status, "未选择作品，不生成运行健康结论。"})
		return r, nil
	}
	if q.store == nil {
		return r, fmt.Errorf("diagnostic database is unavailable")
	}
	if share {
		// Validate first; a browsing cursor never selects the support evidence.
		request.After, request.TranscriptAfter = "", 0
		r.Header.Scope = request
	}
	s, err := q.store.ReadDiagSnapshot(ctx, store.DiagRequest{ProjectID: request.ProjectID, RunID: request.RunID, OperationID: request.OperationID, AfterOperationID: request.After, AfterTranscriptSequence: request.TranscriptAfter, ForShare: share})
	if err != nil {
		return r, err
	}
	local := request.OperationID != "" && !share // 原始错误与对话记录只在本地选定任务后读取
	r.Header.CapturedAt, r.Header.Revision, r.Header.SchemaVersion = s.CapturedAt, s.Revision, s.SchemaVersion
	r.Header.RunEventBoundary = s.RunEventBoundary
	r.Summary.State = "no_run"
	if s.Run != nil {
		r.Header.Scope.RunID = s.Run.ID
		r.Summary.State, r.Summary.Reason = string(s.Run.State), s.Run.StateReason
		r.Summary.CreatedAt, r.Summary.UpdatedAt = &s.Run.CreatedAt, &s.Run.UpdatedAt
	}
	r.Summary.LastActivityAt = s.LastActivityAt
	r.Metrics = Metrics{Operations: s.OperationCount, States: s.Counts, Attempts: s.AttemptCount, RetriedOperations: s.RetriedOperationCount, ToolErrors: s.ToolErrorCount, Usage: s.Usage}
	r.Findings = findings(s.Counts[model.OperationFailed], s.ExpiredLeaseCount, s.ResultUnknownCount)
	r.NextOperationID, r.NextTranscriptSequence = s.NextOperationID, s.NextTranscriptSequence
	for _, op := range s.Operations {
		r.Operations = append(r.Operations, Operation{ID: op.ID, RunID: op.RunID, Kind: op.Kind, State: op.State, Attempt: op.Attempt, FailureCode: op.FailureCode, Error: op.Error, Executor: op.Executor, ConfigDigest: op.ConfigDigest, LeaseUntil: op.LeaseUntil, CreatedAt: op.CreatedAt, UpdatedAt: op.UpdatedAt})
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		for _, op := range r.Operations {
			source := ""
			switch f.Code {
			case "execution.failed":
				if op.State == model.OperationFailed {
					source = "operation.state"
				}
			case "execution.lease_expired":
				if op.State == model.OperationRunning && op.LeaseUntil != nil && !op.LeaseUntil.After(r.Header.CapturedAt) {
					source = "operation.lease"
				}
			case "execution.result_unknown", "execution.repeated_unknown_result":
				if op.FailureCode == model.FailureResultUnknown {
					source = "operation.failure_code"
				}
			}
			if source != "" {
				f.Evidence = append(f.Evidence, Evidence{source, op.ID, op.Attempt})
			}
		}
		if len(f.Evidence) < f.Count {
			r.Coverage = append(r.Coverage, Coverage{"findings", "partial", fmt.Sprintf("%s：本页附 %d/%d 个任务的证据引用，其余任务需翻页查看。", f.Code, len(f.Evidence), f.Count)})
		}
	}
	for _, attempt := range s.Attempts {
		if !local {
			attempt.Error = ""
		}
		r.Attempts = append(r.Attempts, attempt)
	}
	for _, entry := range s.Transcript {
		r.Transcript = append(r.Transcript, TranscriptEntry{Sequence: entry.Sequence, Attempt: entry.Attempt, Kind: entry.Kind, CreatedAt: entry.CreatedAt, Text: string(entry.Payload), PayloadTruncated: entry.PayloadTruncated})
		if entry.PayloadTruncated {
			r.Coverage = append(r.Coverage, Coverage{"transcript", "truncated", fmt.Sprintf("对话记录 #%d 超过读取预算，仅展示前缀。", entry.Sequence)})
		}
	}
	// 总数常驻 Metrics，任何范围都看得见。但单次自纠不是问题——任务成功了还报警就是误报，
	// 所以运行级只在同一次尝试里反复撞同一堵墙时才立发现（与 repeated_unknown_result 同理）；
	// 选定任务是用户主动要细节，有就给。
	stuck := request.OperationID != ""
	for _, group := range s.ToolErrors {
		stuck = stuck || group.Count > 1
	}
	if s.ToolErrorCount > 0 && stuck {
		f := finding("execution.tool_error", s.ToolErrorCount)
		f.OperationID = request.OperationID
		for _, group := range s.ToolErrors {
			f.Evidence = append(f.Evidence, Evidence{"attempt.tool_errors", group.OperationID, group.Attempt})
		}
		r.Findings = append(r.Findings, f)
	}
	r.Coverage = append(r.Coverage, Coverage{"database", "complete", "运行、任务状态与尝试摘要来自同一次只读事务；每次翻页重新采集。"})
	r.Coverage = append(r.Coverage, Coverage{"statistics", "complete", "任务状态、尝试、用量与工具报错覆盖所选整个运行或任务，不受明细窗口影响。"})
	status := "complete"
	if len(r.Operations) < r.Metrics.Operations {
		status = "truncated"
	}
	detail := "本地任务明细每页最多 50 条；原始错误仅在选定任务后读取。"
	if share {
		detail = "按异常、运行中/重试、最近更新选取最多 200 个任务的上下文，与界面页码无关。"
	}
	r.Coverage = append(r.Coverage, Coverage{"operations", status, detail})
	status = "complete"
	if s.AttemptsTruncated {
		status = "truncated"
	}
	r.Coverage = append(r.Coverage, Coverage{"attempts", status, "所列任务的尝试摘要，按任务的先后排列，最多 200 条。"})
	switch {
	case local && (s.TranscriptTruncated || request.TranscriptAfter > 0):
		r.Coverage = append(r.Coverage, Coverage{"transcript", "truncated", "对话记录按序号分页，每页最多 100 条；本页之外的记录需翻页查看。"})
	case local:
		r.Coverage = append(r.Coverage, Coverage{"transcript", "complete", "已读取该任务现存的全部对话记录；经 operation prune 回收过的任务只剩尝试摘要。"})
	default:
		r.Coverage = append(r.Coverage, Coverage{"transcript", "not_collected", "对话记录只在本地选定单个任务后读取。"})
	}
	toolStatus, toolDetail := "complete", "工具报错在写入时计入尝试摘要，所选范围全量统计，证据按任务与 attempt 聚合。"
	if s.ToolErrorsTruncated {
		toolStatus = "truncated"
		toolDetail = fmt.Sprintf("工具错误总数完整，但只附前 %d 个任务 attempt 的证据引用。", len(s.ToolErrors))
	}
	models := Coverage{"models", "complete", "尝试摘要记下 Agent 会话实际用的模型；单次判断用的模型只在对话记录里。"}
	if share {
		models = Coverage{"models", "not_collected", "分享报告不含模型名称。"}
	}
	r.Coverage = append(r.Coverage,
		Coverage{"tool_errors", toolStatus, toolDetail},
		Coverage{"usage", "complete", "用量在模型消息与语义合规核对落盘时计入尝试摘要，覆盖所选范围的全部尝试（含中断与恢复的）；花费只计有定价的模型。"},
		models,
		Coverage{"content_commits", "not_collected", "未查询内容提交时间；最近进展时间不等于最后创作进展。"})
	return r, nil
}
