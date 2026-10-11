package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// 执行记录（D72）：尝试摘要随任务生命周期在同一事务里建立与收尾，对话记录只追加、读取
// 与回收。读取方只读摘要的列，不解析对话记录的载荷。

const attemptColumns = `operation_id, attempt, worker,
	agent_role, agent_provider, agent_model, agent_thinking, agent_digest,
	input_tokens, output_tokens, cache_read_tokens, cost, tool_errors,
	outcome, error, failure_code, lease_expired, started_at_unix_ms, active_at_unix_ms`

// startAttemptTx 在领取事务里为新的 attempt 建立摘要。
func startAttemptTx(ctx context.Context, tx *sql.Tx, operation model.Operation, worker string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO operation_attempts (operation_id, attempt, worker, started_at_unix_ms, active_at_unix_ms)
		VALUES (?, ?, ?, ?, ?)`,
		operation.ID, operation.Attempt, worker, now.UnixMilli(), now.UnixMilli()); err != nil {
		return fmt.Errorf("start attempt %d of operation %q: %w", operation.Attempt, operation.ID, err)
	}
	return nil
}

// concludeAttemptTx 在任务离开 running 的同一事务里收尾当前尝试：记下任务进入的状态与
// 原因。租约回收说明执行者已失联，不是进展，不推进 ActiveAt。
func concludeAttemptTx(
	ctx context.Context,
	tx *sql.Tx,
	operation model.Operation,
	code model.FailureCode,
	leaseExpired bool,
	now time.Time,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE operation_attempts
		SET outcome = ?, error = ?, failure_code = ?, lease_expired = ?,
			active_at_unix_ms = CASE WHEN ? THEN active_at_unix_ms ELSE MAX(active_at_unix_ms, ?) END
		WHERE operation_id = ? AND attempt = ? AND outcome = ''`,
		operation.State, operation.Error, code, leaseExpired, leaseExpired, now.UnixMilli(), operation.ID, operation.Attempt)
	if err != nil {
		return fmt.Errorf("conclude attempt %d of operation %q: %w", operation.Attempt, operation.ID, err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("attempt %d of operation %q has no open summary: %w", operation.Attempt, operation.ID, model.ErrStateConflict)
	}
	return nil
}

// RecordAgentBinding 记下这次尝试里 Agent 会话实际用的模型（D57）。
func (s *Store) RecordAgentBinding(ctx context.Context, operationID string, attempt int, binding model.AgentBinding, at time.Time) error {
	if strings.TrimSpace(operationID) == "" || attempt <= 0 || at.IsZero() ||
		binding.Role == "" || binding.Provider == "" || binding.Model == "" || binding.Digest == "" {
		return fmt.Errorf("agent binding fields are invalid: %w", model.ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE operation_attempts
		SET agent_role = ?, agent_provider = ?, agent_model = ?, agent_thinking = ?, agent_digest = ?,
			active_at_unix_ms = MAX(active_at_unix_ms, ?)
		WHERE operation_id = ? AND attempt = ?`,
		binding.Role, binding.Provider, binding.Model, binding.Thinking, binding.Digest, at.UnixMilli(), operationID, attempt)
	if err != nil {
		return fmt.Errorf("record agent binding: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return fmt.Errorf("attempt %d of operation %q: %w", attempt, operationID, model.ErrNotFound)
	}
	return nil
}

// AppendTranscript 追加一条对话记录，并在同一事务里把它花掉的用量与工具报错并入尝试
// 摘要。不做执行围栏：被接替的执行花掉的用量同样是真实开销，只记在它自己的尝试上。
func (s *Store) AppendTranscript(ctx context.Context, entry model.TranscriptEntry, spent model.Usage, toolErrors int) (model.TranscriptEntry, error) {
	if strings.TrimSpace(entry.OperationID) == "" || entry.Attempt <= 0 || entry.CreatedAt.IsZero() || toolErrors < 0 ||
		(entry.Kind != model.TranscriptMessage && entry.Kind != model.TranscriptJudgment) || !json.Valid(entry.Payload) {
		return model.TranscriptEntry{}, fmt.Errorf("transcript entry fields are invalid: %w", model.ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.TranscriptEntry{}, fmt.Errorf("begin transcript entry: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE operation_attempts
		SET input_tokens = input_tokens + ?, output_tokens = output_tokens + ?,
			cache_read_tokens = cache_read_tokens + ?, cost = cost + ?, tool_errors = tool_errors + ?,
			active_at_unix_ms = MAX(active_at_unix_ms, ?)
		WHERE operation_id = ? AND attempt = ?`,
		spent.Input, spent.Output, spent.CacheRead, spent.Cost, toolErrors, entry.CreatedAt.UnixMilli(),
		entry.OperationID, entry.Attempt)
	if err != nil {
		return model.TranscriptEntry{}, fmt.Errorf("account transcript entry: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return model.TranscriptEntry{}, fmt.Errorf("attempt %d of operation %q: %w", entry.Attempt, entry.OperationID, model.ErrNotFound)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM operation_transcripts WHERE operation_id = ?`,
		entry.OperationID).Scan(&entry.Sequence); err != nil {
		return model.TranscriptEntry{}, fmt.Errorf("allocate transcript sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO operation_transcripts (operation_id, sequence, attempt, kind, payload, created_at_unix_ms)
		VALUES (?, ?, ?, ?, ?, ?)`,
		entry.OperationID, entry.Sequence, entry.Attempt, entry.Kind, []byte(entry.Payload), entry.CreatedAt.UnixMilli()); err != nil {
		return model.TranscriptEntry{}, fmt.Errorf("insert transcript entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.TranscriptEntry{}, fmt.Errorf("commit transcript entry: %w", err)
	}
	return entry, nil
}

func (s *Store) ListTranscript(ctx context.Context, operationID string) ([]model.TranscriptEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sequence, attempt, kind, payload, created_at_unix_ms
		FROM operation_transcripts WHERE operation_id = ? ORDER BY sequence`, operationID)
	if err != nil {
		return nil, fmt.Errorf("list transcript: %w", err)
	}
	defer rows.Close()
	entries := make([]model.TranscriptEntry, 0)
	for rows.Next() {
		entry := model.TranscriptEntry{OperationID: operationID}
		var created int64
		if err := rows.Scan(&entry.Sequence, &entry.Attempt, &entry.Kind, &entry.Payload, &created); err != nil {
			return nil, fmt.Errorf("scan transcript entry: %w", err)
		}
		entry.CreatedAt = time.UnixMilli(created).UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate transcript: %w", err)
	}
	return entries, nil
}

func (s *Store) ListOperationAttempts(ctx context.Context, operationID string) ([]model.OperationAttempt, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+attemptColumns+` FROM operation_attempts WHERE operation_id = ? ORDER BY attempt`, operationID)
	if err != nil {
		return nil, fmt.Errorf("list operation attempts: %w", err)
	}
	defer rows.Close()
	attempts := make([]model.OperationAttempt, 0)
	for rows.Next() {
		attempt, err := scanAttempt(rows)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate operation attempts: %w", err)
	}
	return attempts, nil
}

func scanAttempt(row rowScanner) (model.OperationAttempt, error) {
	var a model.OperationAttempt
	var started, active int64
	if err := row.Scan(&a.OperationID, &a.Attempt, &a.Worker,
		&a.Agent.Role, &a.Agent.Provider, &a.Agent.Model, &a.Agent.Thinking, &a.Agent.Digest,
		&a.Usage.Input, &a.Usage.Output, &a.Usage.CacheRead, &a.Usage.Cost, &a.ToolErrors,
		&a.Outcome, &a.Error, &a.FailureCode, &a.LeaseExpired, &started, &active); err != nil {
		return model.OperationAttempt{}, fmt.Errorf("scan operation attempt: %w", err)
	}
	a.StartedAt, a.ActiveAt = time.UnixMilli(started).UTC(), time.UnixMilli(active).UTC()
	return a, nil
}

// CountOperationFailures 数 Operation 有几次尝试以失败收尾：执行失败与租约过期判失败
// 都算。D56 的自动重开预算只数失败——进程退出释放的尝试以 queued 收尾，不在其中。
func (s *Store) CountOperationFailures(ctx context.Context, id string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM operation_attempts WHERE operation_id = ? AND outcome = ?`,
		id, model.OperationFailed).Scan(&count); err != nil {
		return 0, fmt.Errorf("count failures of operation %q: %w", id, err)
	}
	return count, nil
}

// PruneTranscripts 回收不会再执行的任务的对话记录（D72）：成功、取消与失效的任务不再有
// 后续尝试，续跑恢复用不到它们；尝试摘要保留。只由用户显式触发。
func (s *Store) PruneTranscripts(ctx context.Context) (int, error) {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM operation_transcripts WHERE operation_id IN (
			SELECT id FROM operations WHERE state IN (?, ?, ?))`,
		model.OperationSucceeded, model.OperationCancelled, model.OperationStale)
	if err != nil {
		return 0, fmt.Errorf("prune transcripts: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect pruned transcripts: %w", err)
	}
	return int(removed), nil
}
