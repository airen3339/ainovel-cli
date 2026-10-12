package novel

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/change"
	"github.com/voocel/ainovel-cli/internal/domain/creation"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/store"
)

// Reviews owns novel review interpretation and user adjudications.
type Reviews struct {
	store    *store.Store
	changes  *change.Engine
	projects *projectdoc.Repository
}

func NewReviews(s *store.Store, changes *change.Engine, projects *projectdoc.Repository) *Reviews {
	return &Reviews{store: s, changes: changes, projects: projects}
}

type Evidence struct {
	Verdicts []StoredVerdict
	// Prior 是基线已失效的上一轮裁定（D68）：章节正文未变时沿用其结论。
	Prior []StoredVerdict
	// Repairs 是各章自运行最近一次策略事件以来已派发的自动修订次数，预算按章计（D63）。
	Repairs map[string]int
	// Requested 是用户发起、归属本次运行、还没做完的任务（§6.3 缺口来源），每条是后继链
	// 的首个任务：语义影响处置里选的“重写受影响章”（S4，D77）。
	Requested []model.Operation
	// Change 表示主编提出的修改正等用户确认（D77）：照着要变的内容接着写是白写，先停下。
	Change bool
}

// Goal is the state-loading adapter; Policy remains a pure novel decision function.
type Goal struct{ reader *Reviews }

func NewGoal(reader *Reviews) *Goal                        { return &Goal{reader: reader} }
func (g *Goal) ValidateGoal(payload json.RawMessage) error { return (Policy{}).ValidateGoal(payload) }
func (g *Goal) Next(ctx context.Context, run model.CreationRun) (creation.Decision, error) {
	p, err := g.reader.projects.Project(ctx, run.ProjectID, 0)
	if err != nil {
		return creation.Decision{}, err
	}
	decision := creation.Decision{Revision: p.Revision}
	verdicts, prior, err := g.reader.listVerdicts(ctx, p)
	if err != nil {
		decision.Step.Fail = "审阅结果缺失或损坏，需要人工检查"
		return decision, err
	}
	evidence, err := g.reader.runEvidence(ctx, p, run)
	if err != nil {
		return decision, err
	}
	evidence.Verdicts, evidence.Prior = verdicts, prior
	decision.Step, err = (Policy{}).Next(p, run, evidence)
	return decision, err
}

// RepairHold 是 run 推进卡住的那一章（Policy.RepairHold）：工作台据此让用户接受现状或让它再改。
func (s *Reviews) RepairHold(ctx context.Context, project projectdoc.Snapshot, run model.CreationRun) (string, error) {
	verdicts, prior, err := s.listVerdicts(ctx, project)
	if err != nil {
		return "", err
	}
	evidence, err := s.runEvidence(ctx, project, run)
	if err != nil {
		return "", err
	}
	evidence.Verdicts, evidence.Prior = verdicts, prior
	return (Policy{}).RepairHold(project, run, evidence)
}

// runEvidence 读本次运行的任务账与作品上等用户确认的修改。修订预算按章统计自最近一次策略
// 事件以来已派发的自动修订任务数：预算管的是无人过问的连续修订，运行创建、用户调整策略、
// 或在预算用尽时确认继续，都重新起算（D63）。用户发起的任务按后继链归并，链上最新的
// 还没成功（也没被取消）就还要做。
func (s *Reviews) runEvidence(ctx context.Context, project projectdoc.Snapshot, run model.CreationRun) (Evidence, error) {
	pending, err := s.store.PendingProposals(ctx, model.AuthorityTarget{Kind: model.AuthorityProject, ID: project.ID})
	if err != nil {
		return Evidence{}, err
	}
	evidence := Evidence{Change: slices.ContainsFunc(pending, func(proposal model.Proposal) bool { return proposal.Author == model.ChiefAuthor })}
	events, err := s.store.ListCreationRunEvents(ctx, run.ID)
	if err != nil {
		return Evidence{}, err
	}
	repairs := make(map[string]int)
	var resolutions []string
	first, latest := make(map[string]model.Operation), make(map[string]model.Operation)
	for _, event := range events {
		if event.Kind == model.RunEventCreated || event.Kind == model.RunEventStrategyUpdated {
			repairs = make(map[string]int)
			continue
		}
		if event.Kind != model.RunEventOperationCreated {
			continue
		}
		var payload struct {
			OperationID string `json:"operation_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return Evidence{}, fmt.Errorf("run %q event %d payload is corrupt: %w", run.ID, event.Sequence, err)
		}
		op, err := s.store.GetOperation(ctx, payload.OperationID)
		if err != nil {
			return Evidence{}, err
		}
		if op.Kind == model.OperationRewriteAffected {
			input, err := model.TaskInputAs[model.RewriteAffectedInput](op)
			if err != nil {
				return Evidence{}, err
			}
			if _, seen := first[input.ResolutionProposalID]; !seen {
				first[input.ResolutionProposalID] = op
				resolutions = append(resolutions, input.ResolutionProposalID)
			}
			latest[input.ResolutionProposalID] = op
			continue
		}
		spec, err := model.KindSpec(op.Kind)
		if err != nil {
			return Evidence{}, err
		}
		if !spec.Repair {
			continue
		}
		input, err := model.DecodeTaskInput(op.Kind, op.Input)
		if err != nil {
			return Evidence{}, err
		}
		for _, id := range model.RepairChapters(input) {
			repairs[id]++
		}
	}
	evidence.Repairs = repairs
	for _, resolution := range resolutions {
		if state := latest[resolution].State; state != model.OperationSucceeded && state != model.OperationCancelled {
			evidence.Requested = append(evidence.Requested, first[resolution])
		}
	}
	return evidence, nil
}
