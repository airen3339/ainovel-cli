package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

type ProjectProjection struct {
	ProjectID    string           `json:"project_id"`
	BaseRevision model.Revision   `json:"base_revision"`
	Intent       model.Intent     `json:"intent"`
	Compass      *model.Compass   `json:"compass,omitempty"`
	Plan         []model.PlanNode `json:"plan"`
	Entities     []model.Entity   `json:"entities,omitempty"`
	// 附件只携带本作品库内的不可变引用；投影不是包含媒体文件的备份包。
	Attachments []model.Attachment        `json:"attachments,omitempty"`
	Canon       []model.CanonFact         `json:"canon"`
	Manuscript  []model.ManuscriptChapter `json:"manuscript"`
	Ownership   []model.OwnershipRule     `json:"ownership"`
	Directives  []model.Directive         `json:"directives,omitempty"`
	Approval    model.ApprovalPolicy      `json:"approval,omitempty"`
	Overlay     []string                  `json:"overlay,omitempty"`
	Assets      *model.ProjectAssetRefs   `json:"assets,omitempty"`
}

func (s *Repository) ExportProject(ctx context.Context, projectID string, revision model.Revision) (ProjectProjection, error) {
	project, err := s.Project(ctx, projectID, revision)
	if err != nil {
		return ProjectProjection{}, err
	}
	return projectionOf(project), nil
}

// projectionOf 是快照的投影：导出与导入比较差异共用。
func projectionOf(project Snapshot) ProjectProjection {
	return ProjectProjection{
		ProjectID: project.ID, BaseRevision: project.Revision,
		Intent: project.Intent, Compass: project.Compass, Plan: project.Plan, Entities: project.Entities, Attachments: project.Attachments, Canon: project.Canon,
		Manuscript: project.Manuscript, Ownership: project.Ownership, Directives: project.Directives,
		Approval: project.Approval, Overlay: project.Overlay, Assets: project.Assets,
	}
}

// ImportProject 把作品文件作为一份待批准草案导入：库中已有的作品按差异导入；还没有的先以
// 投影的 Intent/Approval 建出权威流，其余内容作为草案，用户批准后成书。
func (s *Repository) ImportProject(
	ctx context.Context,
	changeID, userID, reason string,
	projection ProjectProjection,
	createdAt time.Time,
) (model.Proposal, error) {
	return s.importProject(ctx, changeID, userID, reason, projection, createdAt, false)
}

func (s *Repository) ImportProjectWithSemantic(
	ctx context.Context,
	changeID, userID, reason string,
	projection ProjectProjection,
	createdAt time.Time,
) (model.Proposal, error) {
	return s.importProject(ctx, changeID, userID, reason, projection, createdAt, true)
}

func (s *Repository) importProject(
	ctx context.Context,
	changeID, userID, reason string,
	projection ProjectProjection,
	createdAt time.Time,
	analyzeSemantic bool,
) (model.Proposal, error) {
	if strings.TrimSpace(projection.ProjectID) == "" {
		return model.Proposal{}, fmt.Errorf("project projection identity is required: %w", model.ErrInvalid)
	}
	if err := s.validateProjectionArtifacts(ctx, projection); err != nil {
		return model.Proposal{}, err
	}
	if _, err := s.Project(ctx, projection.ProjectID, 0); errors.Is(err, model.ErrNotFound) {
		created, err := s.CreateProject(ctx, CreateProjectCommand{
			ProjectID: projection.ProjectID, ChangeID: changeID + ":create", UserID: userID, Reason: reason,
			Draft: ProjectDraft{Intent: projection.Intent, Approval: projection.Approval}, CreatedAt: createdAt,
		})
		if err != nil {
			return model.Proposal{}, err
		}
		projection.BaseRevision = created.Revision
	} else if err != nil {
		return model.Proposal{}, err
	} else if projection.BaseRevision <= model.InitialRevision {
		return model.Proposal{}, fmt.Errorf("importing into an existing project requires its base revision: %w", model.ErrInvalid)
	}
	base, err := s.Project(ctx, projection.ProjectID, projection.BaseRevision)
	if err != nil {
		return model.Proposal{}, err
	}
	currentDocuments, err := projectionDocuments(projectionOf(base))
	if err != nil {
		return model.Proposal{}, err
	}
	desiredDocuments, err := projectionDocuments(projection)
	if err != nil {
		return model.Proposal{}, err
	}
	patches, err := model.StatePatches(currentDocuments, desiredDocuments)
	if err != nil {
		return model.Proposal{}, err
	}
	if len(patches) == 0 {
		return model.Proposal{}, fmt.Errorf("project projection contains no changes: %w", model.ErrInvalid)
	}
	proposal := model.Proposal{
		ID:           changeID,
		Target:       model.AuthorityTarget{Kind: model.AuthorityProject, ID: projection.ProjectID},
		BaseRevision: projection.BaseRevision,
		Author:       model.Author{Kind: model.AuthorUser, ID: userID},
		Reason:       reason, Patches: patches,
		ApprovalState: model.ApprovalPending, CreatedAt: createdAt,
	}
	if analyzeSemantic {
		return s.changes.PrepareWithSemantic(ctx, proposal)
	}
	return s.changes.Prepare(ctx, proposal)
}

func (s *Repository) validateProjectionArtifacts(ctx context.Context, projection ProjectProjection) error {
	for _, attachment := range projection.Attachments {
		if err := attachment.Validate(); err != nil {
			return err
		}
		artifact, err := s.store.GetArtifact(ctx, attachment.Artifact.ID)
		if err != nil {
			return fmt.Errorf("attachment %q requires artifact %q in this project library; projection import does not transfer media files: %w", attachment.ID, attachment.Artifact.ID, err)
		}
		if artifact.ProjectID != projection.ProjectID || artifact.Digest != attachment.Artifact.Digest {
			return fmt.Errorf("attachment %q does not match an artifact in this project library: %w", attachment.ID, model.ErrInvalid)
		}
	}
	return nil
}

func projectionDocuments(projection ProjectProjection) (model.DocumentSet, error) {
	documents := make(model.DocumentSet)
	add := func(ref model.DocumentRef, value any) error {
		if _, exists := documents[ref.Key()]; exists {
			return fmt.Errorf("duplicate projected document %q: %w", ref.Key(), model.ErrInvalid)
		}
		content, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode projected document %q: %w", ref.Key(), err)
		}
		documents[ref.Key()] = model.DocumentVersion{Document: ref, Content: content}
		return nil
	}
	if err := add(model.DocumentRef{Kind: model.DocumentIntent, ID: model.SingletonDocumentID}, projection.Intent); err != nil {
		return nil, err
	}
	if projection.Compass != nil {
		if err := add(model.DocumentRef{Kind: model.DocumentCompass, ID: model.SingletonDocumentID}, *projection.Compass); err != nil {
			return nil, err
		}
	}
	for _, node := range projection.Plan {
		if err := add(model.DocumentRef{Kind: model.DocumentPlan, ID: node.ID}, node); err != nil {
			return nil, err
		}
	}
	for _, entity := range projection.Entities {
		if err := add(model.DocumentRef{Kind: model.DocumentEntity, ID: entity.ID}, entity); err != nil {
			return nil, err
		}
	}
	for _, attachment := range projection.Attachments {
		if err := add(model.DocumentRef{Kind: model.DocumentAttachment, ID: attachment.ID}, attachment); err != nil {
			return nil, err
		}
	}
	for _, fact := range projection.Canon {
		if err := add(model.DocumentRef{Kind: model.DocumentCanon, ID: fact.ID}, fact); err != nil {
			return nil, err
		}
	}
	for _, chapter := range projection.Manuscript {
		if err := add(model.DocumentRef{Kind: model.DocumentManuscript, ID: chapter.ID}, chapter); err != nil {
			return nil, err
		}
	}
	for _, rule := range projection.Ownership {
		if err := add(model.DocumentRef{Kind: model.DocumentOwnership, ID: rule.Target.Key()}, rule); err != nil {
			return nil, err
		}
	}
	for _, directive := range projection.Directives {
		if err := add(model.DocumentRef{Kind: model.DocumentDirective, ID: directive.ID}, directive); err != nil {
			return nil, err
		}
	}
	if projection.Approval != "" {
		setting := model.ApprovalSetting{Policy: projection.Approval}
		if err := add(model.DocumentRef{Kind: model.DocumentApproval, ID: model.SingletonDocumentID}, setting); err != nil {
			return nil, err
		}
	}
	if len(projection.Overlay) > 0 {
		overlay := model.ProjectOverlay{Rules: projection.Overlay}
		if err := add(model.DocumentRef{Kind: model.DocumentOverlay, ID: model.SingletonDocumentID}, overlay); err != nil {
			return nil, err
		}
	}
	if projection.Assets != nil {
		if err := add(model.DocumentRef{Kind: model.DocumentAssets, ID: model.SingletonDocumentID}, *projection.Assets); err != nil {
			return nil, err
		}
	}
	return documents, nil
}
