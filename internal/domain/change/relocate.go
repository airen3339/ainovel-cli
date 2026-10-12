package change

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// VerifyBasis 核对证据基线在 at 上成立（D48）：文档最后变化 revision 相等、要求作用域
// 成员摘要相等、工件已发布且摘要一致。不成立返回 ErrBasisMismatch 并说明条目。
func (e *Engine) VerifyBasis(ctx context.Context, target model.AuthorityTarget, basis model.EvidenceBasis, at model.Revision) error {
	for _, entry := range basis.Documents {
		version, err := e.store.GetDocument(ctx, target, entry.Ref, at)
		if errors.Is(err, model.ErrNotFound) {
			return fmt.Errorf("basis document %s is absent at revision %d: %w", entry.Ref.Key(), at, ErrBasisMismatch)
		}
		if err != nil {
			return err
		}
		if version.Revision != entry.Revision {
			return fmt.Errorf("basis document %s claims revision %d, stored %d at revision %d: %w",
				entry.Ref.Key(), entry.Revision, version.Revision, at, ErrBasisMismatch)
		}
	}
	if len(basis.Scopes) > 0 {
		directives, revisionOf, err := e.directivesAt(ctx, target, at)
		if err != nil {
			return err
		}
		for _, scope := range basis.Scopes {
			if model.ScopeDigest(model.ScopeMembers(directives, scope.Target, revisionOf)) != scope.Digest {
				return fmt.Errorf("directives covering chapter %d changed by revision %d: %w",
					scope.Target.ChapterNumber, at, ErrBasisMismatch)
			}
		}
	}
	for _, ref := range basis.Artifacts {
		artifact, err := e.store.GetArtifact(ctx, ref.ID)
		if errors.Is(err, model.ErrNotFound) {
			return fmt.Errorf("basis artifact %s is not published: %w", ref.ID, ErrBasisMismatch)
		}
		if err != nil {
			return err
		}
		if artifact.Digest != ref.Digest {
			return fmt.Errorf("basis artifact %s digest mismatch: %w", ref.ID, ErrBasisMismatch)
		}
	}
	return nil
}

// BasisHolds 是 VerifyBasis 的布尔视图：基线不成立返回 false 而非错误，其它错误照常上抛。
// 证据有效性查询（审阅裁定、工件、用户裁决）共用这一处，不各自翻译 ErrBasisMismatch。
func (e *Engine) BasisHolds(ctx context.Context, target model.AuthorityTarget, basis model.EvidenceBasis, at model.Revision) (bool, error) {
	err := e.VerifyBasis(ctx, target, basis, at)
	if errors.Is(err, ErrBasisMismatch) {
		return false, nil
	}
	return err == nil, err
}

func (e *Engine) directivesAt(
	ctx context.Context,
	target model.AuthorityTarget,
	at model.Revision,
) ([]model.Directive, func(model.DocumentRef) model.Revision, error) {
	revisions := make(map[string]model.Revision)
	var directives []model.Directive
	if at > model.InitialRevision {
		documents, err := e.store.ListDocuments(ctx, target, model.DocumentDirective, at)
		if err != nil {
			return nil, nil, err
		}
		for _, document := range documents {
			var directive model.Directive
			if err := json.Unmarshal(document.Content, &directive); err != nil {
				return nil, nil, fmt.Errorf("decode directive %q: %w", document.Document.ID, err)
			}
			directives = append(directives, directive)
			revisions[document.Document.Key()] = document.Revision
		}
	}
	return directives, func(ref model.DocumentRef) model.Revision { return revisions[ref.Key()] }, nil
}

// Relocate 落实提案重定位规则（D51 / §5.5 第 4 条）：提案基线落后于当前 Revision 时，
// 仅当中间各 Revision 只改过用户专属文档、没碰本提案的文档，且所属任务的基线在当前仍
// 成立，才把基线搬到当前并重算结构影响；否则返回 model.ErrRevisionConflict 并说明原因。
// 不落库。没有任务的提案（主编提出的修改、导入的草案，D77）不是照着任务上下文写的，
// 中间各 Revision 没碰它的文档就能搬；带语义影响报告的，报告判断过的文档（结构影响的
// 全集）其间都不能变，搬过去后也不能波及报告之外的文档——否则报告过时，要重判。
func (e *Engine) Relocate(ctx context.Context, proposal model.Proposal) (model.Proposal, bool, error) {
	task, err := e.task(ctx, proposal)
	if err != nil {
		return model.Proposal{}, false, err
	}
	if task == nil {
		return e.relocate(ctx, proposal, model.EvidenceBasis{}, true)
	}
	basis, err := model.OperationBasis(*task)
	if err != nil {
		return model.Proposal{}, false, err
	}
	return e.relocate(ctx, proposal, basis, false)
}

// RelocatePending 重定位已保存的待裁决提案并落库；attempt 为正时受执行归属围栏保护（D42）。
func (e *Engine) RelocatePending(ctx context.Context, proposal model.Proposal, attempt int, now time.Time) (model.Proposal, error) {
	relocated, moved, err := e.Relocate(ctx, proposal)
	if err != nil || !moved {
		return relocated, err
	}
	if err := e.store.UpdatePendingProposal(ctx, relocated, attempt, now); err != nil {
		return model.Proposal{}, err
	}
	return relocated, nil
}

// judgedDocuments 是没有任务的提案的语义影响报告判断过的文档：提案基线上的结构影响
// 全集加语义范围，与分析时读的一致。没有报告时为空。
func (e *Engine) judgedDocuments(ctx context.Context, proposal model.Proposal, direct bool) (map[string]bool, error) {
	if !direct || len(proposal.Impact.Semantic) == 0 {
		return nil, nil
	}
	var impact StructuralImpact
	if err := json.Unmarshal(proposal.Impact.Structural, &impact); err != nil {
		return nil, fmt.Errorf("decode structural impact of %q: %w", proposal.ID, err)
	}
	base, err := e.loadState(ctx, proposal.Target, proposal.BaseRevision)
	if err != nil {
		return nil, err
	}
	scope, err := model.SemanticScope(proposal.Patches, base)
	if err != nil {
		return nil, err
	}
	judged := make(map[string]bool)
	for _, ref := range slices.Concat(impact.Direct, impact.Affected, scope) {
		judged[ref.Key()] = true
	}
	return judged, nil
}

func (e *Engine) relocate(ctx context.Context, proposal model.Proposal, basis model.EvidenceBasis, direct bool) (model.Proposal, bool, error) {
	current, err := e.currentRevision(ctx, proposal.Target)
	if err != nil {
		return model.Proposal{}, false, err
	}
	if current == proposal.BaseRevision {
		return proposal, false, nil
	}
	if current < proposal.BaseRevision {
		return model.Proposal{}, false, fmt.Errorf("base revision %d is ahead of current revision %d: %w",
			proposal.BaseRevision, current, model.ErrRevisionConflict)
	}
	own := make(map[string]struct{}, len(proposal.Patches))
	for _, patch := range proposal.Patches {
		own[patch.Document.Key()] = struct{}{}
	}
	judged, err := e.judgedDocuments(ctx, proposal, direct)
	if err != nil {
		return model.Proposal{}, false, err
	}
	for revision := proposal.BaseRevision + 1; revision <= current; revision++ {
		changeSet, err := e.store.GetChangeSetByRevision(ctx, proposal.Target, revision)
		if err != nil {
			return model.Proposal{}, false, err
		}
		for _, patch := range changeSet.Patches {
			spec, err := model.DocumentType(patch.Document.Kind)
			if err != nil {
				return model.Proposal{}, false, err
			}
			_, touched := own[patch.Document.Key()]
			unrelated := spec.UserOnly
			if direct {
				unrelated = !judged[patch.Document.Key()]
			}
			if touched || !unrelated {
				return model.Proposal{}, false, fmt.Errorf("%s changed at revision %d: %w",
					patch.Document.Key(), revision, model.ErrRevisionConflict)
			}
		}
	}
	if err := e.VerifyBasis(ctx, proposal.Target, basis, current); errors.Is(err, ErrBasisMismatch) {
		return model.Proposal{}, false, fmt.Errorf("%w: %w", model.ErrRevisionConflict, err)
	} else if err != nil {
		return model.Proposal{}, false, err
	}
	relocated := proposal
	relocated.BaseRevision = current
	checked, err := e.validateAndAnalyze(ctx, relocated)
	if err != nil {
		return model.Proposal{}, false, err
	}
	if judged != nil {
		scope, err := model.SemanticScope(relocated.Patches, checked.base)
		if err != nil {
			return model.Proposal{}, false, err
		}
		for _, ref := range unionRefs(checked.impact.Affected, scope) {
			if !judged[ref.Key()] {
				return model.Proposal{}, false, fmt.Errorf("%s came after the semantic impact report: %w", ref.Key(), model.ErrRevisionConflict)
			}
		}
	}
	relocated, err = withImpact(relocated, checked.impact)
	return relocated, err == nil, err
}
