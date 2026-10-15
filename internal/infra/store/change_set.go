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

func (s *Store) GetChangeSetByRevision(
	ctx context.Context,
	target model.AuthorityTarget,
	revision model.Revision,
) (model.ChangeSet, error) {
	if err := target.Validate(); err != nil {
		return model.ChangeSet{}, err
	}
	if revision <= model.InitialRevision {
		return model.ChangeSet{}, fmt.Errorf("revision must be positive: %w", model.ErrInvalid)
	}
	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM change_sets
		WHERE target_kind = ? AND target_id = ? AND target_scope = ? AND new_revision = ?`,
		target.Kind, target.ID, target.Scope, revision).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ChangeSet{}, model.ErrNotFound
	}
	if err != nil {
		return model.ChangeSet{}, fmt.Errorf("find change set by revision: %w", err)
	}
	return s.GetChangeSet(ctx, id)
}

func (s *Store) GetChangeSet(ctx context.Context, id string) (model.ChangeSet, error) {
	var changeSet model.ChangeSet
	var structural, semantic, compliance []byte
	var decidedByKind, decidedByID, operationID sql.NullString
	var decidedAt sql.NullInt64
	var createdAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT target_kind, target_id, target_scope, base_revision, new_revision,
			author_kind, author_id, reason, source, structural_impact, semantic_impact, compliance_impact,
			approval_state, decision_author_kind, decision_author_id,
			decided_at_unix_ms, created_at_unix_ms, operation_id
		FROM change_sets WHERE id = ?`, id).Scan(
		&changeSet.Target.Kind, &changeSet.Target.ID, &changeSet.Target.Scope,
		&changeSet.BaseRevision, &changeSet.NewRevision,
		&changeSet.Author.Kind, &changeSet.Author.ID, &changeSet.Reason, &changeSet.Source,
		&structural, &semantic, &compliance, &changeSet.ApprovalState,
		&decidedByKind, &decidedByID, &decidedAt, &createdAt, &operationID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ChangeSet{}, model.ErrNotFound
	}
	if err != nil {
		return model.ChangeSet{}, fmt.Errorf("read change set: %w", err)
	}
	changeSet.ID = id
	changeSet.OperationID = operationID.String
	if changeSet.Impact, err = impactOf(structural, semantic, compliance); err != nil {
		return model.ChangeSet{}, err
	}
	changeSet.CreatedAt = time.UnixMilli(createdAt).UTC()
	if decidedByKind.Valid && decidedByID.Valid {
		changeSet.DecidedBy = &model.Author{Kind: model.AuthorKind(decidedByKind.String), ID: decidedByID.String}
	}
	if decidedAt.Valid {
		value := time.UnixMilli(decidedAt.Int64).UTC()
		changeSet.DecidedAt = &value
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT document_kind, document_id, operation, content
		FROM document_versions WHERE change_set_id = ?
		ORDER BY document_kind, document_id`, id)
	if err != nil {
		return model.ChangeSet{}, fmt.Errorf("read change set patches: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var patch model.Patch
		var content []byte
		if err := rows.Scan(&patch.Document.Kind, &patch.Document.ID, &patch.Operation, &content); err != nil {
			return model.ChangeSet{}, fmt.Errorf("scan change set patch: %w", err)
		}
		if patch.Operation == model.PatchPut {
			patch.Content = append(json.RawMessage(nil), content...)
		}
		changeSet.Patches = append(changeSet.Patches, patch)
	}
	if err := rows.Err(); err != nil {
		return model.ChangeSet{}, fmt.Errorf("iterate change set patches: %w", err)
	}
	if err := changeSet.Validate(); err != nil {
		return model.ChangeSet{}, fmt.Errorf("stored change set is invalid: %w", err)
	}
	return changeSet, nil
}

// ManuscriptCommit 是正文的一次入稿：哪一章、在哪个 revision、何时生效。
type ManuscriptCommit struct {
	ChapterID string
	Revision  model.Revision
	At        time.Time
}

// ManuscriptCommits 列出作品正文的每次入稿，按 revision 先后：生产里程碑取自落盘的
// 记录，重开作品也还在（D77）。
func (s *Store) ManuscriptCommits(ctx context.Context, projectID string) ([]ManuscriptCommit, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.document_id, c.new_revision, COALESCE(c.decided_at_unix_ms, c.created_at_unix_ms)
		FROM document_versions v JOIN change_sets c ON c.id = v.change_set_id
		WHERE v.target_kind = ? AND v.target_id = ? AND v.target_scope = '' AND v.document_kind = ? AND v.operation = 'put'
		ORDER BY c.new_revision`, model.AuthorityProject, projectID, model.DocumentManuscript)
	if err != nil {
		return nil, fmt.Errorf("list manuscript commits: %w", err)
	}
	defer rows.Close()
	var commits []ManuscriptCommit
	for rows.Next() {
		var commit ManuscriptCommit
		var at int64
		if err := rows.Scan(&commit.ChapterID, &commit.Revision, &at); err != nil {
			return nil, fmt.Errorf("scan manuscript commit: %w", err)
		}
		commit.At = time.UnixMilli(at).UTC()
		commits = append(commits, commit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list manuscript commits: %w", err)
	}
	return commits, nil
}
