package decision

import (
	"context"
	"fmt"
	"strings"
	"time"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/change"
	"github.com/voocel/ainovel-cli/internal/domain/model"
)

func (s *Review) Proposal(ctx context.Context, id string) (model.Proposal, error) {
	return s.store.GetProposal(ctx, id)
}

func (s *Review) ProposalForOperation(ctx context.Context, operationID string) (model.Proposal, error) {
	return s.store.GetProposalByOperation(ctx, operationID)
}

type ResolveProposalCommand struct {
	ProposalID string
	UserID     string
	Strategy   string
	Reason     string
	CreatedAt  time.Time
}

type ResolveProposalResult struct {
	Strategy  string           `json:"strategy"`
	Proposal  *model.Proposal  `json:"proposal,omitempty"`
	ChangeSet *model.ChangeSet `json:"change_set,omitempty"`
	// Reworks 是选“重写受影响的章”时逐章记下的返工要求（D86）。
	Reworks []model.Directive `json:"reworks,omitempty"`
}

// ResolveProposal 按用户选的处置裁决带语义冲突的提案（S4）：不改了即否决；只改设定即批准；
// 重写受影响的章是批准之后把那几章记为返工要求，由推导逐章返工（D86）。同一处置重试幂等。
func (s *Review) ResolveProposal(ctx context.Context, command ResolveProposalCommand) (ResolveProposalResult, error) {
	if command.ProposalID == "" || command.UserID == "" || command.CreatedAt.IsZero() {
		return ResolveProposalResult{}, fmt.Errorf("proposal resolution requires proposal, user and time: %w", model.ErrInvalid)
	}
	proposal, err := s.store.GetProposal(ctx, command.ProposalID)
	if err != nil {
		return ResolveProposalResult{}, err
	}
	strategy := model.ResolutionStrategy(command.Strategy)
	report, option, err := semanticResolutionOption(proposal, strategy)
	if err != nil {
		return ResolveProposalResult{}, err
	}
	if report.Status == model.SemanticImpactConsistent {
		return ResolveProposalResult{}, fmt.Errorf("consistent proposal does not require a resolution strategy; approve it directly: %w", model.ErrInvalid)
	}
	result := ResolveProposalResult{Strategy: command.Strategy}
	if strategy == model.ResolutionAbandon {
		rejected, err := s.Reject(ctx, RejectCommand{ProposalID: proposal.ID, UserID: command.UserID, Reason: command.Reason, At: command.CreatedAt})
		if err != nil {
			return ResolveProposalResult{}, err
		}
		result.Proposal = &rejected
		return result, nil
	}
	committed, err := s.Approve(ctx, proposal.ID, command.UserID, command.CreatedAt)
	if err != nil {
		return ResolveProposalResult{}, err
	}
	result.ChangeSet = &committed
	if strategy == model.ResolutionReinterpretFuture {
		return result, nil
	}
	result.Reworks, err = s.projects.ReworkChapters(ctx, projectdoc.ReworkChaptersCommand{
		ProjectID: proposal.Target.ID, ChangeID: proposal.ID + ":rework", UserID: command.UserID,
		ChapterIDs: option.ChapterIDs, Text: option.Explanation, Reason: "按修改重写受影响的章", CreatedAt: command.CreatedAt,
	})
	if err != nil {
		return result, fmt.Errorf("proposal was committed but the affected chapters could not be noted for rework; retry the same resolution: %w", err)
	}
	return result, nil
}

func semanticResolutionOption(
	proposal model.Proposal,
	strategy model.ResolutionStrategy,
) (model.SemanticImpactReport, model.ResolutionOption, error) {
	if strategy != model.ResolutionRewriteAffected && strategy != model.ResolutionReinterpretFuture && strategy != model.ResolutionAbandon {
		return model.SemanticImpactReport{}, model.ResolutionOption{}, fmt.Errorf("unknown resolution strategy %q: %w", strategy, model.ErrInvalid)
	}
	if proposal.Impact.Semantic == nil {
		return model.SemanticImpactReport{}, model.ResolutionOption{}, fmt.Errorf("proposal %q has no semantic impact report: %w", proposal.ID, model.ErrInvalid)
	}
	report := *proposal.Impact.Semantic
	// 一致的报告没有候选策略，先返回让调用方给出“直接批准即可”，不要误报成策略不存在。
	if report.Status == model.SemanticImpactConsistent {
		return report, model.ResolutionOption{}, nil
	}
	for _, option := range report.Options {
		if option.Strategy == strategy {
			return report, option, nil
		}
	}
	return model.SemanticImpactReport{}, model.ResolutionOption{}, fmt.Errorf("semantic impact does not offer strategy %q: %w", strategy, model.ErrInvalid)
}

func (s *Review) Approve(ctx context.Context, proposalID, userID string, at time.Time) (model.ChangeSet, error) {
	proposal, err := s.store.GetProposal(ctx, proposalID)
	if err != nil {
		return model.ChangeSet{}, err
	}
	switch proposal.ApprovalState {
	case model.ApprovalApproved:
		return s.store.GetChangeSet(ctx, proposal.ID)
	case model.ApprovalRejected:
		return model.ChangeSet{}, fmt.Errorf("proposal %q was rejected: %w", proposal.ID, model.ErrStateConflict)
	default:
		return s.commitPending(ctx, proposal, userID, at)
	}
}

// commitPending 由用户批准并提交待裁决提案：任务提案只在任务仍等待审批时可批准（存储在
// 提交事务内再断言一次），先按 D51 重定位到当前 Revision，不能重定位的原样返回
// ErrRevisionConflict，由用户按工作台指引重写。
func (s *Review) commitPending(ctx context.Context, proposal model.Proposal, userID string, at time.Time) (model.ChangeSet, error) {
	if proposal.OperationID != "" {
		operation, err := s.store.GetOperation(ctx, proposal.OperationID)
		if err != nil {
			return model.ChangeSet{}, err
		}
		if operation.State != model.OperationAwaitingApproval {
			return model.ChangeSet{}, fmt.Errorf("operation %q is %s and no longer awaits approval: %w", operation.ID, operation.State, model.ErrStateConflict)
		}
	}
	proposal, err := s.changes.RelocatePending(ctx, proposal, 0, at)
	if err != nil {
		return model.ChangeSet{}, err
	}
	approved, err := change.Decide(proposal, model.ApprovalApproved, model.Author{Kind: model.AuthorUser, ID: userID}, at)
	if err != nil {
		return model.ChangeSet{}, err
	}
	return s.changes.Commit(ctx, approved)
}

// Relocate 按所属任务的基线重定位待裁决提案（D51），不落库：工作台据此判断候选是否
// 仍是当前稿件。
func (s *Review) Relocate(ctx context.Context, proposal model.Proposal) (model.Proposal, bool, error) {
	return s.changes.Relocate(ctx, proposal)
}

// RejectCommand 否决一份提案；Reason 入账为要求（S10），Source 是主编代录这句理由时用户
// 说它的那条对话消息（D74）。
type RejectCommand struct {
	ProposalID string
	UserID     string
	Reason     string
	Source     string
	At         time.Time
}

// Reject 否决提案并把理由入账为要求。等待审批的任务随之失败；已被取代（stale）或取消的
// 任务只否决旧稿、不再转移状态。
func (s *Review) Reject(ctx context.Context, command RejectCommand) (model.Proposal, error) {
	userID, at := command.UserID, command.At
	proposal, err := s.store.GetProposal(ctx, command.ProposalID)
	if err != nil {
		return model.Proposal{}, err
	}
	var operation model.Operation
	if proposal.OperationID != "" {
		if operation, err = s.store.GetOperation(ctx, proposal.OperationID); err != nil {
			return model.Proposal{}, err
		}
		switch operation.State {
		case model.OperationAwaitingApproval, model.OperationFailed, model.OperationStale, model.OperationCancelled:
		default:
			return model.Proposal{}, fmt.Errorf("operation %q is %s: %w", operation.ID, operation.State, model.ErrStateConflict)
		}
	}
	rejected := proposal
	switch proposal.ApprovalState {
	case model.ApprovalApproved:
		return model.Proposal{}, fmt.Errorf("proposal %q was approved: %w", proposal.ID, model.ErrStateConflict)
	case model.ApprovalPending:
		rejected, err = change.Decide(proposal, model.ApprovalRejected, model.Author{Kind: model.AuthorUser, ID: userID}, at)
		if err == nil {
			rejected.DecisionReason = strings.TrimSpace(command.Reason)
			rejected, err = s.changes.Reject(ctx, rejected)
		}
		if err != nil {
			return model.Proposal{}, err
		}
	}
	if proposal.OperationID == "" {
		return rejected, nil
	}
	if err := s.rejectionDirective(ctx, rejected, operation, userID, command.Source, at); err != nil {
		return model.Proposal{}, err
	}
	return rejected, nil
}

// rejectionDirective 把否决理由入账为 Directive（§4.9 / S10），作用域见 rejectionScope；
// 后继按当前快照装配到它，审阅逐条核验。幂等：同一提案重复否决沿用同一 ChangeID 与
// 已记录的理由。
func (s *Review) rejectionDirective(
	ctx context.Context,
	rejected model.Proposal,
	operation model.Operation,
	userID, source string,
	at time.Time,
) error {
	if rejected.DecisionReason == "" || rejected.Target.Kind != model.AuthorityProject {
		return nil
	}
	scope, err := s.rejectionScope(ctx, operation, rejected.Target.ID)
	if err != nil {
		return err
	}
	_, err = s.projects.AddDirective(ctx, projectdoc.AddDirectiveCommand{
		ProjectID: rejected.Target.ID, ChangeID: rejected.ID + ":directive", UserID: userID,
		DirectiveID: rejected.ID + ":directive", Scope: scope, Text: rejected.DecisionReason, Source: source,
		Reason: "否决候选 " + rejected.ID + " 的理由", CreatedAt: at,
	})
	return err
}

// rejectionScope 是否决理由的作用域（D71）：章节稿件落到该章；规划方案说的是还没写的
// 部分，从下一章起——已写章节不因规划重写，理由若覆盖它们，每个已审窗口的要求作用域
// 都会变、全书重审。其余任务（事实核验）落到整书。
func (s *Review) rejectionScope(ctx context.Context, operation model.Operation, projectID string) (string, error) {
	input, err := model.DecodeTaskInput(operation.Kind, operation.Input)
	if err != nil {
		return "", err
	}
	switch input := input.(type) {
	case *model.WriteChapterInput:
		return model.DirectiveScopePlanNode(input.ChapterPlanID), nil
	case *model.RewriteChapterInput:
		return model.DirectiveScopePlanNode(input.ChapterPlanID), nil
	case *model.ReworkChapterInput:
		return model.DirectiveScopePlanNode(input.ChapterPlanID), nil
	case *model.DevelopPlanInput, *model.RevisePlanInput:
		project, err := s.projects.Project(ctx, projectID, model.InitialRevision)
		if err != nil {
			return "", err
		}
		return model.DirectiveScopeFromChapter(len(project.Manuscript) + 1), nil
	}
	return model.DirectiveScopeProject, nil
}

func (s *Review) PrepareRevert(
	ctx context.Context,
	proposalID, projectID, userID, reason string,
	to model.Revision,
	createdAt time.Time,
) (model.Proposal, error) {
	return s.changes.PrepareRevert(
		ctx, proposalID,
		model.AuthorityTarget{Kind: model.AuthorityProject, ID: projectID}, to,
		model.Author{Kind: model.AuthorUser, ID: userID}, reason, createdAt,
	)
}
