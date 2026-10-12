package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

type DiagRequest struct {
	ProjectID, RunID, OperationID, AfterOperationID string
	AfterTranscriptSequence                         int64
	ForShare                                        bool
}
type DiagSnapshot struct {
	ProjectID             string
	Revision              model.Revision
	SchemaVersion         int
	CapturedAt            time.Time
	Run                   *DiagRun
	Counts                map[model.OperationState]int
	AttemptCount          int
	OperationCount        int
	ExpiredLeaseCount     int
	ResultUnknownCount    int
	ToolErrorCount        int
	ToolErrors            []DiagToolError
	ToolErrorsTruncated   bool
	Usage                 model.Usage // 所选范围全部尝试的用量，按尝试摘要加总
	RetriedOperationCount int
	RunEventBoundary      int64
	LastActivityAt        *time.Time
	Operations            []DiagOperation
	// Attempts 是所列任务的尝试摘要，按任务的先后排列，最多 diagAttemptLimit 条。
	Attempts          []model.OperationAttempt
	AttemptsTruncated bool
	// Transcript 只在本地选定单个任务时读取，按序号分页。
	Transcript             []DiagTranscriptEntry
	NextOperationID        string
	NextTranscriptSequence int64
	TranscriptTruncated    bool
}
type DiagRun struct {
	ID, ProjectID        string
	State                model.CreationRunState
	StateReason          string
	CompletedRevision    model.Revision
	CreatedAt, UpdatedAt time.Time
}
type DiagOperation struct {
	ID, RunID                     string
	Kind                          model.OperationKind
	State                         model.OperationState
	Attempt                       int
	FailureCode                   model.FailureCode
	Error, Executor, ConfigDigest string
	LeaseUntil                    *time.Time
	CreatedAt, UpdatedAt          time.Time
}

// DiagTranscriptEntry 是一条对话记录的本地明细；载荷超过读取预算时只带前缀。
type DiagTranscriptEntry struct {
	model.TranscriptEntry
	PayloadTruncated bool
}

// DiagToolError 是一次报过工具错误的尝试：计数取自尝试摘要，不读对话记录。
type DiagToolError struct {
	OperationID string
	Attempt     int
	Count       int
}

const (
	diagOperationPage   = 50
	diagShareOperations = 200
	diagAttemptLimit    = 200
	diagToolErrorGroups = 200
	diagTranscriptPage  = 100
	diagPayloadBudget   = 65536
)

// ReadDiagSnapshot reads bounded details and scope-wide aggregates from a single
// SQLite snapshot. No project documents or operation inputs enter this projection;
// transcripts are read only for a single selected operation.
func (s *Store) ReadDiagSnapshot(ctx context.Context, req DiagRequest) (DiagSnapshot, error) {
	out := DiagSnapshot{ProjectID: req.ProjectID, CapturedAt: time.Now().UTC(), Counts: map[model.OperationState]int{}}
	if strings.TrimSpace(req.ProjectID) == "" || req.AfterTranscriptSequence < 0 || (req.AfterTranscriptSequence != 0 && req.OperationID == "") || (req.AfterOperationID != "" && req.OperationID != "") {
		return out, model.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&out.SchemaVersion); err != nil {
		return out, err
	}
	err = tx.QueryRowContext(ctx, `SELECT current_revision FROM authority_streams WHERE target_kind=? AND target_id=? AND target_scope=''`, model.AuthorityProject, req.ProjectID).Scan(&out.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return out, model.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if req.OperationID != "" {
		var projectID, runID, kind string
		err = tx.QueryRowContext(ctx, `SELECT target_id,run_id,target_kind FROM operations WHERE id=?`, req.OperationID).Scan(&projectID, &runID, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return out, model.ErrNotFound
		}
		if err != nil {
			return out, err
		}
		if projectID != req.ProjectID || kind != string(model.AuthorityProject) || (req.RunID != "" && req.RunID != runID) {
			return out, fmt.Errorf("operation does not belong to diagnostic scope: %w", model.ErrInvalid)
		}
		req.RunID = runID
	}
	runSQL := `SELECT id,project_id,state,state_reason,completed_revision,created_at_unix_ms,updated_at_unix_ms FROM creation_runs WHERE id=?`
	arg := req.RunID
	if req.RunID == "" {
		runSQL = `SELECT id,project_id,state,state_reason,completed_revision,created_at_unix_ms,updated_at_unix_ms FROM creation_runs WHERE project_id=? ORDER BY created_at_unix_ms DESC,id DESC LIMIT 1`
		arg = req.ProjectID
	}
	run := DiagRun{}
	var created, updated int64
	err = tx.QueryRowContext(ctx, runSQL, arg).Scan(&run.ID, &run.ProjectID, &run.State, &run.StateReason, &run.CompletedRevision, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		if req.RunID != "" {
			return out, model.ErrNotFound
		}
		return out, tx.Commit()
	}
	if err != nil {
		return out, err
	}
	if run.ProjectID != req.ProjectID {
		return out, fmt.Errorf("run does not belong to project: %w", model.ErrInvalid)
	}
	switch run.State {
	case model.RunRunning, model.RunWaitingUser, model.RunPaused, model.RunCompleted, model.RunFailed, model.RunCancelled:
	default:
		return out, fmt.Errorf("invalid stored run state %q", run.State)
	}
	run.CreatedAt = time.UnixMilli(created).UTC()
	run.UpdatedAt = time.UnixMilli(updated).UTC()
	out.Run = &run
	var lastRun sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0),MAX(created_at_unix_ms) FROM creation_run_events WHERE run_id=?`, run.ID).Scan(&out.RunEventBoundary, &lastRun); err != nil {
		return out, err
	}
	scope := `o.run_id=? AND o.target_kind=? AND o.target_id=?`
	args := []any{run.ID, model.AuthorityProject, req.ProjectID}
	if req.OperationID != "" {
		scope += ` AND o.id=?`
		args = append(args, req.OperationID)
	}
	if err = readDiagCounts(ctx, tx, scope, args, &out); err != nil {
		return out, err
	}
	if err = readDiagAttemptAggregates(ctx, tx, scope, args, &out); err != nil {
		return out, err
	}
	if lastRun.Valid && req.OperationID == "" && (out.LastActivityAt == nil || lastRun.Int64 > out.LastActivityAt.UnixMilli()) {
		v := time.UnixMilli(lastRun.Int64).UTC()
		out.LastActivityAt = &v
	}
	if err = readDiagOperations(ctx, tx, scope, args, req, &out); err != nil {
		return out, err
	}
	if err = readDiagAttempts(ctx, tx, &out); err != nil {
		return out, err
	}
	if req.OperationID != "" && !req.ForShare {
		if err = readDiagTranscript(ctx, tx, req, &out); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

func readDiagCounts(ctx context.Context, tx *sql.Tx, scope string, args []any, out *DiagSnapshot) error {
	rows, err := tx.QueryContext(ctx, `SELECT o.state,COUNT(*),COALESCE(SUM(o.attempt),0),COALESCE(SUM(o.attempt>1),0) FROM operations o WHERE `+scope+` GROUP BY o.state`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var state model.OperationState
		var count, attempts, retries int
		if err = rows.Scan(&state, &count, &attempts, &retries); err != nil {
			return err
		}
		switch state {
		case model.OperationQueued, model.OperationRunning, model.OperationPaused, model.OperationAwaitingApproval, model.OperationSucceeded, model.OperationFailed, model.OperationCancelled, model.OperationStale:
		default:
			return fmt.Errorf("invalid stored operation state %q", state)
		}
		out.Counts[state] = count
		out.OperationCount += count
		out.AttemptCount += attempts
		out.RetriedOperationCount += retries
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(o.state='running' AND o.lease_until_unix_ms<=?),0),COALESCE(SUM(o.failure_code='result_unknown'),0) FROM operations o WHERE `+scope,
		append([]any{out.CapturedAt.UnixMilli()}, args...)...).Scan(&out.ExpiredLeaseCount, &out.ResultUnknownCount)
}

// readDiagAttemptAggregates 汇总所选范围全部尝试的用量、工具报错与最近进展，并按尝试列出
// 报过工具错误的坐标：全量统计，不受明细窗口影响。
func readDiagAttemptAggregates(ctx context.Context, tx *sql.Tx, scope string, args []any, out *DiagSnapshot) error {
	from := ` FROM operation_attempts a JOIN operations o ON o.id=a.operation_id WHERE ` + scope
	var lastActive sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(a.input_tokens),0),COALESCE(SUM(a.output_tokens),0),COALESCE(SUM(a.cache_read_tokens),0),COALESCE(SUM(a.cost),0),
		COALESCE(SUM(a.tool_errors),0),MAX(a.active_at_unix_ms)`+from, args...).
		Scan(&out.Usage.Input, &out.Usage.Output, &out.Usage.CacheRead, &out.Usage.Cost, &out.ToolErrorCount, &lastActive)
	if err != nil {
		return err
	}
	if lastActive.Valid {
		v := time.UnixMilli(lastActive.Int64).UTC()
		out.LastActivityAt = &v
	}
	if out.ToolErrorCount == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.operation_id,a.attempt,a.tool_errors`+from+` AND a.tool_errors>0
		ORDER BY a.tool_errors DESC,a.operation_id,a.attempt LIMIT ?`, append(append([]any{}, args...), diagToolErrorGroups+1)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var group DiagToolError
		if err = rows.Scan(&group.OperationID, &group.Attempt, &group.Count); err != nil {
			return err
		}
		out.ToolErrors = append(out.ToolErrors, group)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(out.ToolErrors) > diagToolErrorGroups {
		out.ToolErrors = out.ToolErrors[:diagToolErrorGroups]
		out.ToolErrorsTruncated = true
	}
	return nil
}

func readDiagOperations(ctx context.Context, tx *sql.Tx, scope string, args []any, req DiagRequest, out *DiagSnapshot) error {
	pageScope := scope
	pageArgs := append([]any{}, args...)
	if req.AfterOperationID != "" {
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations o WHERE `+scope+` AND o.id=?`, append(append([]any{}, args...), req.AfterOperationID)...).Scan(&found); err != nil {
			return err
		}
		if found != 1 {
			return fmt.Errorf("cursor does not belong to diagnostic scope: %w", model.ErrInvalid)
		}
		pageScope += ` AND o.id>?`
		pageArgs = append(pageArgs, req.AfterOperationID)
	}
	errorColumn := `''`
	if req.OperationID != "" && !req.ForShare {
		errorColumn = `o.error`
	}
	limit, order := diagOperationPage, `o.id`
	if req.ForShare {
		limit = diagShareOperations
		order = `CASE WHEN ` + diagAffectedTask + ` THEN 0 WHEN o.state='running' OR o.attempt>1 THEN 1 ELSE 2 END, o.updated_at_unix_ms DESC,o.id`
		pageArgs = append(pageArgs, out.CapturedAt.UnixMilli())
	}
	pageArgs = append(pageArgs, limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT o.id,o.run_id,o.kind,o.state,o.attempt,o.failure_code,`+errorColumn+`,o.executor,COALESCE(json_extract(o.execution_snapshot,'$.config_digest'),''),o.lease_until_unix_ms,o.created_at_unix_ms,o.updated_at_unix_ms
		FROM operations o WHERE `+pageScope+` ORDER BY `+order+` LIMIT ?`, pageArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var op DiagOperation
		var lease sql.NullInt64
		var created, updated int64
		if err = rows.Scan(&op.ID, &op.RunID, &op.Kind, &op.State, &op.Attempt, &op.FailureCode, &op.Error, &op.Executor, &op.ConfigDigest, &lease, &created, &updated); err != nil {
			return err
		}
		if op.Executor == "" || op.ConfigDigest == "" {
			return fmt.Errorf("operation %q is missing execution identity: %w", op.ID, model.ErrInvalid)
		}
		op.CreatedAt = time.UnixMilli(created).UTC()
		op.UpdatedAt = time.UnixMilli(updated).UTC()
		if lease.Valid {
			v := time.UnixMilli(lease.Int64).UTC()
			op.LeaseUntil = &v
		}
		out.Operations = append(out.Operations, op)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(out.Operations) > limit {
		out.Operations = out.Operations[:limit]
		if !req.ForShare {
			out.NextOperationID = out.Operations[limit-1].ID
		}
	}
	return nil
}

// readDiagAttempts 按所列任务的先后读它们的尝试摘要（分享时异常任务在前），同一任务按
// attempt 先后；截断只丢排在后面的任务，所列异常任务的尝试不会被别的任务挤掉。
func readDiagAttempts(ctx context.Context, tx *sql.Tx, out *DiagSnapshot) error {
	if len(out.Operations) == 0 {
		return nil
	}
	args := make([]any, 0, 2*len(out.Operations)+1)
	for position, op := range out.Operations {
		args = append(args, op.ID, position)
	}
	args = append(args, diagAttemptLimit+1)
	rows, err := tx.QueryContext(ctx, `WITH listed(id,position) AS (VALUES (?,?)`+strings.Repeat(",(?,?)", len(out.Operations)-1)+`)
		SELECT `+attemptColumns+` FROM listed JOIN operation_attempts ON operation_id=listed.id
		ORDER BY listed.position,attempt LIMIT ?`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return err
		}
		out.Attempts = append(out.Attempts, attempt)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(out.Attempts) > diagAttemptLimit {
		out.Attempts = out.Attempts[:diagAttemptLimit]
		out.AttemptsTruncated = true
	}
	return nil
}

func readDiagTranscript(ctx context.Context, tx *sql.Tx, req DiagRequest, out *DiagSnapshot) error {
	rows, err := tx.QueryContext(ctx, `SELECT sequence,attempt,kind,substr(payload,1,?),length(payload)>?,created_at_unix_ms
		FROM operation_transcripts WHERE operation_id=? AND sequence>? ORDER BY sequence LIMIT ?`,
		diagPayloadBudget, diagPayloadBudget, req.OperationID, req.AfterTranscriptSequence, diagTranscriptPage+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		entry := DiagTranscriptEntry{TranscriptEntry: model.TranscriptEntry{OperationID: req.OperationID}}
		var created int64
		if err = rows.Scan(&entry.Sequence, &entry.Attempt, &entry.Kind, &entry.Payload, &entry.PayloadTruncated, &created); err != nil {
			return err
		}
		entry.CreatedAt = time.UnixMilli(created).UTC()
		out.Transcript = append(out.Transcript, entry)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(out.Transcript) > diagTranscriptPage {
		out.Transcript = out.Transcript[:diagTranscriptPage]
		out.TranscriptTruncated = true
		out.NextTranscriptSequence = out.Transcript[diagTranscriptPage-1].Sequence
	}
	return nil
}

// The parameter is the snapshot capture time. This is an observation predicate,
// never a lifecycle transition or a recovery authorization.
const diagAffectedTask = `(o.state='failed' OR o.failure_code='result_unknown' OR (o.state='running' AND o.lease_until_unix_ms<=?))`
