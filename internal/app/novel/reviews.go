package novel

import (
	"context"
	"encoding/json"
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
// 或在预算用尽时确认继续，都重新起算（D63）。
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
	for _, event := range events {
		if event.Kind == model.RunEventCreated || event.Kind == model.RunEventStrategyUpdated {
			repairs = make(map[string]int)
			continue
		}
		if event.Kind != model.RunEventOperationCreated {
			continue
		}
		created, err := event.OperationCreated()
		if err != nil {
			return Evidence{}, err
		}
		op, err := s.store.GetOperation(ctx, created.OperationID)
		if err != nil {
			return Evidence{}, err
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
	return evidence, nil
}
