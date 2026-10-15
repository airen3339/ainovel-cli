package bootstrap_test

import (
	"context"
	"encoding/json"

	"github.com/voocel/ainovel-cli/internal/domain/change"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/domain/operation"
	"github.com/voocel/ainovel-cli/internal/infra/store"
)

// evidenceReader 按选定 revision 取输入仍成立的工件与检查证据：第二种目标（渲染）的
// 状态适配器只靠它读证据，证明内核机制不依赖小说（D52）。
type evidenceReader struct {
	store      *store.Store
	changes    *change.Engine
	operations *operation.Engine
}

func newEvidenceReader(s *store.Store, changes *change.Engine, operations *operation.Engine) *evidenceReader {
	return &evidenceReader{store: s, changes: changes, operations: operations}
}

type evidenceCheck struct {
	OperationID string
	Kind        model.OperationKind
	Basis       model.EvidenceBasis
	Content     json.RawMessage
}

func (r *evidenceReader) Valid(ctx context.Context, target model.AuthorityTarget, revision model.Revision, basis model.EvidenceBasis) (bool, error) {
	return r.changes.BasisHolds(ctx, target, basis, revision)
}

func (r *evidenceReader) Artifacts(ctx context.Context, projectID string, revision model.Revision) ([]model.Artifact, error) {
	artifacts, err := r.store.ListArtifacts(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var valid []model.Artifact
	for _, artifact := range artifacts {
		ok, err := r.Valid(ctx, model.AuthorityTarget{Kind: model.AuthorityProject, ID: projectID}, revision, artifact.Basis)
		if err != nil {
			return nil, err
		}
		if ok {
			valid = append(valid, artifact)
		}
	}
	return valid, nil
}

// Checks filters by requested operation kinds before decoding their domain payloads.
func (r *evidenceReader) Checks(ctx context.Context, projectID string, revision model.Revision, kinds ...model.OperationKind) ([]evidenceCheck, error) {
	selected := make(map[model.OperationKind]bool, len(kinds))
	for _, kind := range kinds {
		selected[kind] = true
	}
	documents, err := r.store.ListDerivedDocumentsByKind(ctx, projectID, model.DerivedVerdictKind)
	if err != nil {
		return nil, err
	}
	var valid []evidenceCheck
	for _, document := range documents {
		op, err := r.store.GetOperation(ctx, document.Key)
		if err != nil {
			return nil, err
		}
		if op.State != model.OperationSucceeded || !selected[op.Kind] {
			continue
		}
		basis, err := r.operations.ValidateEvidence(ctx, op, document.Content)
		if err != nil {
			return nil, err
		}
		ok, err := r.Valid(ctx, op.Target, revision, basis)
		if err != nil {
			return nil, err
		}
		if ok {
			valid = append(valid, evidenceCheck{OperationID: op.ID, Kind: op.Kind, Basis: basis, Content: document.Content})
		}
	}
	return valid, nil
}
