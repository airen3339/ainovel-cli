package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

func (s *Store) SaveProposal(ctx context.Context, proposal model.Proposal) (model.Proposal, error) {
	return s.saveProposal(ctx, s.db, proposal)
}

// SaveExecutionProposal 将候选提案的落盘纳入同一个 attempt 围栏，避免旧执行
// 在检查归属之后失去租约，仍写出可被后继恢复采纳的提案。
func (s *Store) SaveExecutionProposal(ctx context.Context, proposal model.Proposal, attempt int) (model.Proposal, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Proposal{}, err
	}
	defer tx.Rollback()
	if err := assertActiveAttempt(ctx, tx, proposal.OperationID, attempt); err != nil {
		return model.Proposal{}, err
	}
	saved, err := s.saveProposal(ctx, tx, proposal)
	if err != nil {
		return model.Proposal{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Proposal{}, err
	}
	return saved, nil
}

func (s *Store) saveProposal(ctx context.Context, query execQuerier, proposal model.Proposal) (model.Proposal, error) {
	payload, digest, err := encodeProposal(proposal)
	if err != nil {
		return model.Proposal{}, err
	}

	result, err := query.ExecContext(ctx, `
		INSERT INTO proposals (
			id, content_digest, payload, state, target_kind, target_id, target_scope,
			base_revision, created_at_unix_ms, updated_at_unix_ms, operation_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		proposal.ID, digest, payload, proposal.ApprovalState,
		proposal.Target.Kind, proposal.Target.ID, proposal.Target.Scope,
		proposal.BaseRevision, proposal.CreatedAt.UnixMilli(), proposal.CreatedAt.UnixMilli(), nullableString(proposal.OperationID))
	if err != nil {
		return model.Proposal{}, fmt.Errorf("save proposal: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return model.Proposal{}, fmt.Errorf("inspect proposal insert: %w", err)
	}
	if rows == 1 {
		return proposal, nil
	}

	stored, storedDigest, err := s.getProposal(ctx, query, proposal.ID)
	if err != nil {
		return model.Proposal{}, err
	}
	if storedDigest != digest {
		return model.Proposal{}, fmt.Errorf("proposal %q: %w", proposal.ID, model.ErrIdempotencyConflict)
	}
	if stored.ApprovalState != model.ApprovalPending {
		return model.Proposal{}, fmt.Errorf("proposal %q is %s: %w", proposal.ID, stored.ApprovalState, model.ErrStateConflict)
	}
	return stored, nil
}

// UpdatePendingProposal 改写待裁决提案的基线与影响（D51 重定位、合规证据回写）：同一份
// 提交（model.SameSubmission）、仍为 pending、基线不回退；attempt 为正时在同一事务内确认
// 它仍是当前执行（D42）。
func (s *Store) UpdatePendingProposal(ctx context.Context, proposal model.Proposal, attempt int, now time.Time) error {
	if err := proposal.Validate(); err != nil {
		return err
	}
	if proposal.ApprovalState != model.ApprovalPending || now.IsZero() {
		return fmt.Errorf("proposal update requires a pending proposal and a time: %w", model.ErrInvalid)
	}
	payload, digest, err := encodeProposal(proposal)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin proposal update: %w", err)
	}
	defer tx.Rollback()
	stored, _, err := s.getProposal(ctx, tx, proposal.ID)
	if err != nil {
		return err
	}
	if stored.ApprovalState != model.ApprovalPending {
		return fmt.Errorf("proposal %q is %s: %w", proposal.ID, stored.ApprovalState, model.ErrStateConflict)
	}
	if !model.SameSubmission(stored, proposal) || proposal.BaseRevision < stored.BaseRevision {
		return fmt.Errorf("proposal %q update may only move its base forward and refresh its impact: %w", proposal.ID, model.ErrIdempotencyConflict)
	}
	if attempt > 0 {
		if err := assertActiveAttempt(ctx, tx, proposal.OperationID, attempt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE proposals
		SET payload = ?, content_digest = ?, base_revision = ?, updated_at_unix_ms = ?
		WHERE id = ? AND state = ?`,
		payload, digest, proposal.BaseRevision, now.UnixMilli(), proposal.ID, model.ApprovalPending); err != nil {
		return fmt.Errorf("update pending proposal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit proposal update: %w", err)
	}
	return nil
}

func (s *Store) GetProposal(ctx context.Context, id string) (model.Proposal, error) {
	proposal, _, err := s.getProposal(ctx, s.db, id)
	return proposal, err
}

// PendingProposals 列出作品上不属于任何任务、等用户裁决的提案，新的在前（D77）：主编
// 提出的修改与导入的草案。
func (s *Store) PendingProposals(ctx context.Context, target model.AuthorityTarget) ([]model.Proposal, error) {
	return s.pendingProposals(ctx, target, true)
}

// PendingDocuments 是作品上全部等裁决的提案（任务交的与不属于任务的）改到的文档：新文档
// 分配 ID 时让开它们。
func (s *Store) PendingDocuments(ctx context.Context, target model.AuthorityTarget) ([]model.DocumentRef, error) {
	proposals, err := s.pendingProposals(ctx, target, false)
	if err != nil {
		return nil, err
	}
	var refs []model.DocumentRef
	for _, proposal := range proposals {
		for _, patch := range proposal.Patches {
			refs = append(refs, patch.Document)
		}
	}
	return refs, nil
}

func (s *Store) pendingProposals(ctx context.Context, target model.AuthorityTarget, tasklessOnly bool) ([]model.Proposal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM proposals
		WHERE target_kind = ? AND target_id = ? AND target_scope = ? AND state = ? AND (NOT ? OR operation_id IS NULL)
		ORDER BY created_at_unix_ms DESC, id DESC`,
		target.Kind, target.ID, target.Scope, model.ApprovalPending, tasklessOnly)
	if err != nil {
		return nil, fmt.Errorf("list pending proposals: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pending proposal: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("iterate pending proposals: %w", err)
	}
	proposals := make([]model.Proposal, 0, len(ids))
	for _, id := range ids {
		proposal, err := s.GetProposal(ctx, id)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	return proposals, nil
}

func (s *Store) GetProposalByOperation(ctx context.Context, operationID string) (model.Proposal, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM proposals WHERE operation_id = ?`, operationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Proposal{}, model.ErrNotFound
	}
	if err != nil {
		return model.Proposal{}, fmt.Errorf("find proposal for operation: %w", err)
	}
	return s.GetProposal(ctx, id)
}

// CommitProposal 是用户裁决路径：审批决定与新 Revision 同事务落库，不受执行归属限制。
// 任务提案只在任务仍等待审批时可批准，任务同一事务内转为 succeeded——已取消或被新推导
// 取代的任务，其旧稿不能入账（D64）。
func (s *Store) CommitProposal(ctx context.Context, proposal model.Proposal) (model.ChangeSet, error) {
	return s.commitProposal(ctx, proposal, 0)
}

// CommitExecutionProposal 是执行侧的自动提交路径（D42）：归属检查与 ChangeSet 写入在同一
// 事务内完成，任务已取消或执行已被接替时提交被拒；分成两步会让取消落在缝隙里。
func (s *Store) CommitExecutionProposal(ctx context.Context, proposal model.Proposal, attempt int) (model.ChangeSet, error) {
	return s.commitProposal(ctx, proposal, attempt)
}

func (s *Store) commitProposal(ctx context.Context, proposal model.Proposal, attempt int) (model.ChangeSet, error) {
	preparedDigest, err := preparedProposalDigest(proposal)
	if err != nil {
		return model.ChangeSet{}, err
	}
	payload, changeDigest, err := encodeProposal(proposal)
	if err != nil {
		return model.ChangeSet{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ChangeSet{}, fmt.Errorf("begin proposal commit: %w", err)
	}
	defer tx.Rollback()

	stored, storedDigest, err := s.getProposal(ctx, tx, proposal.ID)
	if err != nil {
		return model.ChangeSet{}, err
	}
	if storedDigest != preparedDigest {
		return model.ChangeSet{}, fmt.Errorf("proposal %q changed after preparation: %w", proposal.ID, model.ErrIdempotencyConflict)
	}
	switch stored.ApprovalState {
	case model.ApprovalRejected:
		return model.ChangeSet{}, fmt.Errorf("proposal %q was rejected: %w", proposal.ID, model.ErrStateConflict)
	case model.ApprovalApproved:
		_, storedDecisionDigest, err := encodeProposal(stored)
		if err != nil {
			return model.ChangeSet{}, err
		}
		if storedDecisionDigest != changeDigest {
			return model.ChangeSet{}, fmt.Errorf("proposal %q has a different approval decision: %w", proposal.ID, model.ErrIdempotencyConflict)
		}
		// 同一决定已入账即是重试：批准与 ChangeSet 在同一事务写入，二者同在。
		revision, err := committedRevision(ctx, tx, proposal.ID)
		return model.ChangeSet{Proposal: proposal, NewRevision: revision}, err
	case model.ApprovalPending:
	default:
		return model.ChangeSet{}, fmt.Errorf("proposal %q has invalid state %q: %w", proposal.ID, stored.ApprovalState, model.ErrStateConflict)
	}

	switch {
	case attempt > 0:
		if err := assertActiveAttempt(ctx, tx, proposal.OperationID, attempt); err != nil {
			return model.ChangeSet{}, err
		}
	case proposal.OperationID != "" && stored.ApprovalState == model.ApprovalPending:
		if _, err := transitionOperationTx(ctx, tx, proposal.OperationID,
			model.OperationAwaitingApproval, model.OperationSucceeded, "", "", *proposal.DecidedAt, 0); errors.Is(err, model.ErrStateConflict) {
			return model.ChangeSet{}, fmt.Errorf("operation %q no longer awaits approval: %w", proposal.OperationID, model.ErrStateConflict)
		} else if err != nil {
			return model.ChangeSet{}, err
		}
	}
	committed, err := commitProposalTx(ctx, tx, proposal, changeDigest)
	if err != nil {
		return model.ChangeSet{}, err
	}
	if stored.ApprovalState == model.ApprovalPending {
		result, err := tx.ExecContext(ctx, `
			UPDATE proposals
			SET payload = ?, state = ?, updated_at_unix_ms = ?
			WHERE id = ? AND state = ?`,
			payload, proposal.ApprovalState, proposal.DecidedAt.UnixMilli(),
			proposal.ID, model.ApprovalPending)
		if err != nil {
			return model.ChangeSet{}, fmt.Errorf("record proposal approval: %w", err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return model.ChangeSet{}, fmt.Errorf("inspect proposal approval: %w", err)
		}
		if rows != 1 {
			return model.ChangeSet{}, fmt.Errorf("proposal %q approval raced: %w", proposal.ID, model.ErrStateConflict)
		}
	}
	if err := tx.Commit(); err != nil {
		return model.ChangeSet{}, fmt.Errorf("commit approved proposal: %w", err)
	}
	return committed, nil
}

func (s *Store) RejectProposal(ctx context.Context, proposal model.Proposal) (model.Proposal, error) {
	preparedDigest, err := preparedProposalDigest(proposal)
	if err != nil {
		return model.Proposal{}, err
	}
	payload, decisionDigest, err := encodeProposal(proposal)
	if err != nil {
		return model.Proposal{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Proposal{}, fmt.Errorf("begin proposal rejection: %w", err)
	}
	defer tx.Rollback()
	stored, storedDigest, err := s.getProposal(ctx, tx, proposal.ID)
	if err != nil {
		return model.Proposal{}, err
	}
	if storedDigest != preparedDigest {
		return model.Proposal{}, fmt.Errorf("proposal %q changed after preparation: %w", proposal.ID, model.ErrIdempotencyConflict)
	}
	if stored.ApprovalState == model.ApprovalApproved {
		return model.Proposal{}, fmt.Errorf("proposal %q was approved: %w", proposal.ID, model.ErrStateConflict)
	}
	if stored.ApprovalState == model.ApprovalRejected {
		_, storedDecisionDigest, err := encodeProposal(stored)
		if err != nil {
			return model.Proposal{}, err
		}
		if storedDecisionDigest != decisionDigest {
			return model.Proposal{}, fmt.Errorf("proposal %q has a different rejection decision: %w", proposal.ID, model.ErrIdempotencyConflict)
		}
		return stored, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE proposals
		SET payload = ?, state = ?, updated_at_unix_ms = ?
		WHERE id = ? AND state = ?`,
		payload, proposal.ApprovalState, proposal.DecidedAt.UnixMilli(),
		proposal.ID, model.ApprovalPending)
	if err != nil {
		return model.Proposal{}, fmt.Errorf("record proposal rejection: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return model.Proposal{}, fmt.Errorf("inspect proposal rejection: %w", err)
	}
	if rows != 1 {
		return model.Proposal{}, fmt.Errorf("proposal %q rejection raced: %w", proposal.ID, model.ErrStateConflict)
	}
	// 与批准对称：等待批准的任务在同一事务里随否决转为失败；已失效、取消或失败的保持原状。
	if proposal.OperationID != "" {
		if _, err := transitionOperationTx(ctx, tx, proposal.OperationID,
			model.OperationAwaitingApproval, model.OperationFailed, "proposal rejected by user", "", *proposal.DecidedAt, 0); err != nil && !errors.Is(err, model.ErrStateConflict) {
			return model.Proposal{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.Proposal{}, fmt.Errorf("commit proposal rejection: %w", err)
	}
	return proposal, nil
}

func (s *Store) getProposal(ctx context.Context, query rowQuerier, id string) (model.Proposal, string, error) {
	var payload []byte
	var digest string
	err := query.QueryRowContext(ctx, `SELECT payload, content_digest FROM proposals WHERE id = ?`, id).
		Scan(&payload, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Proposal{}, "", model.ErrNotFound
	}
	if err != nil {
		return model.Proposal{}, "", fmt.Errorf("read proposal: %w", err)
	}
	var proposal model.Proposal
	if err := json.Unmarshal(payload, &proposal); err != nil {
		return model.Proposal{}, "", fmt.Errorf("decode stored proposal %q: %w", id, err)
	}
	if err := proposal.Validate(); err != nil {
		return model.Proposal{}, "", fmt.Errorf("validate stored proposal %q: %w", id, err)
	}
	return proposal, digest, nil
}

func preparedProposalDigest(proposal model.Proposal) (string, error) {
	proposal.ApprovalState = model.ApprovalPending
	proposal.DecidedBy = nil
	proposal.DecidedAt = nil
	proposal.DecisionReason = ""
	_, digest, err := encodeProposal(proposal)
	return digest, err
}

func encodeProposal(proposal model.Proposal) ([]byte, string, error) {
	payload, err := json.Marshal(proposal)
	if err != nil {
		return nil, "", fmt.Errorf("marshal proposal: %w", err)
	}
	return payload, model.Digest(payload), nil
}
