package novel

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
)

type AddAdjudicationCommand struct {
	ProjectID string
	ChangeID  string
	UserID    string
	// Findings 是一次接受的发现标识：<裁定 Operation id>/<发现序号>（工作台与 headless 均按此
	// 引用）。一次决定一次提交，每条发现各记一条裁决，ID 为 <ChangeID>:<序号>。
	Findings  []string
	Reason    string
	CreatedAt time.Time
}

type WithdrawAdjudicationCommand struct {
	ProjectID      string
	ChangeID       string
	UserID         string
	AdjudicationID string
	Reason         string
	CreatedAt      time.Time
}

type AdjudicationResult struct {
	ProjectID     string               `json:"project_id"`
	Revision      model.Revision       `json:"revision"`
	Adjudications []model.Adjudication `json:"adjudications"`
}

// AddAdjudication 记录用户对阻塞发现的接受（D43）：基线从被裁决的裁定复制，用户不构造；
// 裁定必须仍然有效。同一 ChangeID 重复提交幂等。
func (s *Reviews) AddAdjudication(ctx context.Context, command AddAdjudicationCommand) (AdjudicationResult, error) {
	if err := projectdoc.ValidateDirectiveCommand(command.ProjectID, command.ChangeID, command.UserID, command.Reason, command.CreatedAt); err != nil {
		return AdjudicationResult{}, err
	}
	if len(command.Findings) == 0 {
		return AdjudicationResult{}, fmt.Errorf("adjudication requires at least one finding: %w", model.ErrInvalid)
	}
	project, err := s.projects.Project(ctx, command.ProjectID, 0)
	if err != nil {
		return AdjudicationResult{}, err
	}
	verdicts, _, err := s.rawVerdicts(ctx, project)
	if err != nil {
		return AdjudicationResult{}, err
	}
	records := make([]model.Adjudication, len(command.Findings))
	for i, finding := range command.Findings {
		operationID, index, err := model.ParseFindingID(finding)
		if err != nil {
			return AdjudicationResult{}, err
		}
		position := slices.IndexFunc(verdicts, func(verdict StoredVerdict) bool { return verdict.Key == operationID })
		if position < 0 {
			return AdjudicationResult{}, fmt.Errorf("finding %q belongs to no valid review verdict: %w", finding, model.ErrInvalid)
		}
		verdict := verdicts[position].Verdict
		if index >= len(verdict.Findings) || verdict.Findings[index].Severity != model.FindingBlocking {
			return AdjudicationResult{}, fmt.Errorf("finding %q is not a blocking finding: %w", finding, model.ErrInvalid)
		}
		records[i] = model.Adjudication{
			ID: fmt.Sprintf("%s:%d", command.ChangeID, i+1), Finding: finding, Basis: verdict.Basis,
			Reason: strings.TrimSpace(command.Reason), CreatedAt: command.CreatedAt,
		}
	}
	return s.putAdjudications(ctx, project, command.ChangeID, command.UserID, records)
}

// WithdrawAdjudication 以新记录撤回一条接受：历史不变，有效性改变。
func (s *Reviews) WithdrawAdjudication(ctx context.Context, command WithdrawAdjudicationCommand) (AdjudicationResult, error) {
	if err := projectdoc.ValidateDirectiveCommand(command.ProjectID, command.ChangeID, command.UserID, command.Reason, command.CreatedAt); err != nil {
		return AdjudicationResult{}, err
	}
	project, err := s.projects.Project(ctx, command.ProjectID, 0)
	if err != nil {
		return AdjudicationResult{}, err
	}
	var target *model.Adjudication
	for index := range project.Adjudications {
		record := &project.Adjudications[index]
		switch {
		case record.ID == command.AdjudicationID:
			target = record
		case record.Withdraws == command.AdjudicationID:
			return AdjudicationResult{}, fmt.Errorf("adjudication %q is already withdrawn: %w", command.AdjudicationID, model.ErrStateConflict)
		}
	}
	if target == nil {
		return AdjudicationResult{}, fmt.Errorf("adjudication %q: %w", command.AdjudicationID, model.ErrNotFound)
	}
	if target.Finding == "" {
		return AdjudicationResult{}, fmt.Errorf("adjudication %q is a withdrawal record: %w", command.AdjudicationID, model.ErrInvalid)
	}
	return s.putAdjudications(ctx, project, command.ChangeID, command.UserID, []model.Adjudication{{
		ID: command.ChangeID, Withdraws: target.ID, Reason: strings.TrimSpace(command.Reason), CreatedAt: command.CreatedAt,
	}})
}

// putAdjudications 把一次决定的裁决记录作为一次提交追加：记录只追加，同一决定的记录已入账
// 即是重试，不产生新 Revision。
func (s *Reviews) putAdjudications(ctx context.Context, project projectdoc.Snapshot, changeID, userID string, records []model.Adjudication) (AdjudicationResult, error) {
	var stored []model.Adjudication
	for _, existing := range project.Adjudications {
		if slices.ContainsFunc(records, func(record model.Adjudication) bool { return record.ID == existing.ID }) {
			stored = append(stored, existing)
		}
	}
	if len(stored) > 0 {
		return AdjudicationResult{ProjectID: project.ID, Revision: project.Revision, Adjudications: stored}, nil
	}
	patches := make([]model.Patch, len(records))
	for i, record := range records {
		if err := record.Validate(); err != nil {
			return AdjudicationResult{}, err
		}
		content, err := json.Marshal(record)
		if err != nil {
			return AdjudicationResult{}, fmt.Errorf("encode adjudication: %w", err)
		}
		patches[i] = model.Patch{Document: model.DocumentRef{Kind: model.DocumentAdjudication, ID: record.ID}, Operation: model.PatchPut, Content: content}
	}
	committed, err := s.changes.CommitUser(ctx, model.Proposal{
		ID: changeID, Target: model.AuthorityTarget{Kind: model.AuthorityProject, ID: project.ID}, BaseRevision: project.Revision,
		Author: model.Author{Kind: model.AuthorUser, ID: userID}, Reason: records[0].Reason,
		Patches: patches, ApprovalState: model.ApprovalPending, CreatedAt: records[0].CreatedAt,
	})
	if err != nil {
		return AdjudicationResult{}, err
	}
	return AdjudicationResult{ProjectID: project.ID, Revision: committed.NewRevision, Adjudications: records}, nil
}

// ValidAdjudications 取基线仍成立的裁决记录（撤回记录无基线，恒有效）；
// 接受了哪些发现由 model.AcceptedFindings 归并。
func (s *Reviews) ValidAdjudications(ctx context.Context, project projectdoc.Snapshot) ([]model.Adjudication, error) {
	var valid []model.Adjudication
	for _, record := range project.Adjudications {
		basisValid, err := s.basisValid(ctx, project, record.Basis)
		if err != nil {
			return nil, err
		}
		if basisValid {
			valid = append(valid, record)
		}
	}
	return valid, nil
}
