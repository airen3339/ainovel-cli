package decision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/change"
	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// 不属于任务的 AI 修改（主编，D77）：放行规则与任务提案相同，只是没有任务快照；能放行
// 的直接生效，不能的等用户按键确认。

// Submission 是提出一份修改的结果：ChangeSet 非空即已生效，否则等用户确认；Conflicted
// 表示与已写章节可能有出入，等用户选处置（S4）。
type Submission struct {
	Proposal   model.Proposal
	ChangeSet  *model.ChangeSet
	Conflicted bool
}

// Submit 提出一份修改：改动波及已写章节的内容时先做语义影响分析，有冲突就等用户选处置
// （S4）；否则按审批策略放行，策略或权限要求用户时等用户确认。
func (s *Review) Submit(ctx context.Context, proposal model.Proposal) (Submission, error) {
	scope, err := s.changes.SemanticScope(ctx, proposal)
	if err != nil {
		return Submission{}, err
	}
	semantic := len(scope) > 0
	prepare := s.changes.Prepare
	if semantic {
		prepare = s.changes.PrepareWithSemantic
	}
	prepared, err := prepare(ctx, proposal)
	if err != nil {
		return Submission{}, err
	}
	if semantic && prepared.Impact.Semantic.Status != model.SemanticImpactConsistent {
		return Submission{Proposal: prepared, Conflicted: true}, nil
	}
	admission, err := s.changes.Admit(ctx, prepared)
	if err != nil || admission.Hold != "" {
		return Submission{Proposal: prepared}, err
	}
	approved, err := admission.Approve(prepared, prepared.CreatedAt)
	if err != nil {
		return Submission{}, err
	}
	committed, err := s.changes.Commit(ctx, approved)
	if err != nil {
		return Submission{}, err
	}
	return Submission{Proposal: committed.Proposal, ChangeSet: &committed}, nil
}

// Undo 填好撤销一次已生效变更的提案：它改过的文档恢复原样，基线是当前 Revision；之后
// 同样经 Submit 放行或等用户确认。
func (s *Review) Undo(ctx context.Context, changeSetID string, proposal model.Proposal) (model.Proposal, error) {
	patches, base, err := s.changes.Undo(ctx, changeSetID)
	if err != nil {
		return model.Proposal{}, err
	}
	proposal.BaseRevision, proposal.Patches = base, patches
	return proposal, nil
}

// Withdraw 撤回主编提出、还在等用户确认的修改：用户说不要了，主编照办，作品不变。
func (s *Review) Withdraw(ctx context.Context, proposalID string, at time.Time) error {
	proposal, err := s.store.GetProposal(ctx, proposalID)
	if err != nil {
		return err
	}
	if proposal.Author != model.ChiefAuthor || proposal.OperationID != "" {
		return fmt.Errorf("proposal %q is not the chief's: %w", proposalID, model.ErrInvalid)
	}
	withdrawn, err := change.Decide(proposal, model.ApprovalRejected, model.Author{Kind: model.AuthorSystem, ID: "chief-withdrawal"}, at)
	if err != nil {
		return err
	}
	_, err = s.changes.Reject(ctx, withdrawn)
	return err
}

// PendingChange 是主编提出、还在等用户确认的那份修改；没有时 ok=false。
func (s *Review) PendingChange(ctx context.Context, projectID string) (model.Proposal, bool, error) {
	pending, err := s.store.PendingProposals(ctx, model.AuthorityTarget{Kind: model.AuthorityProject, ID: projectID})
	if err != nil {
		return model.Proposal{}, false, err
	}
	for _, proposal := range pending {
		if proposal.Author == model.ChiefAuthor {
			return proposal, true, nil
		}
	}
	return model.Proposal{}, false, nil
}

// PendingDraft 是用户自己提交、等他批准的那份修改（导入作品文件、回滚）：不属于任何任务，
// 也不是主编提的；没有时 ok=false。
func (s *Review) PendingDraft(ctx context.Context, projectID string) (model.Proposal, bool, error) {
	pending, err := s.store.PendingProposals(ctx, model.AuthorityTarget{Kind: model.AuthorityProject, ID: projectID})
	if err != nil {
		return model.Proposal{}, false, err
	}
	for _, proposal := range pending {
		if proposal.OperationID == "" && proposal.Author.Kind == model.AuthorUser {
			return proposal, true, nil
		}
	}
	return model.Proposal{}, false, nil
}

// Change 是一次生效的变更与它此刻能否撤销：其后有变更碰过同样的文档（ErrRevisionConflict）、
// 或它只追加了记录（要求、裁决，ErrInvalid）时不能撤销。
type Change struct {
	model.ChangeSet
	Undoable bool
}

// History 是作品最近生效的 limit 次变更，新的在前。建书那次不在其中：它是作品的起点，
// 不是对作品的修改。
func (s *Review) History(ctx context.Context, projectID string, limit int) ([]Change, error) {
	target := model.AuthorityTarget{Kind: model.AuthorityProject, ID: projectID}
	current, err := s.store.CurrentRevision(ctx, target)
	if err != nil {
		return nil, err
	}
	var history []Change
	for revision := current; revision > model.InitialRevision+1 && len(history) < limit; revision-- {
		changeSet, err := s.store.GetChangeSetByRevision(ctx, target, revision)
		if err != nil {
			return nil, err
		}
		_, _, err = s.changes.Undo(ctx, changeSet.ID)
		if err != nil && !errors.Is(err, model.ErrRevisionConflict) && !errors.Is(err, model.ErrInvalid) {
			return nil, err
		}
		history = append(history, Change{ChangeSet: changeSet, Undoable: err == nil})
	}
	return history, nil
}
