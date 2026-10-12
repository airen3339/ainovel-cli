package novel

import (
	"context"
	"encoding/json"
	"fmt"

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
	repairs, err := g.reader.repairCounts(ctx, run.ID)
	if err != nil {
		return decision, err
	}
	decision.Step, err = (Policy{}).Next(p, run, Evidence{Verdicts: verdicts, Prior: prior, Repairs: repairs})
	return decision, err
}

// RepairHold 是 run 推进卡住的那一章（Policy.RepairHold）：工作台据此让用户接受现状或让它再改。
func (s *Reviews) RepairHold(ctx context.Context, project projectdoc.Snapshot, run model.CreationRun) (string, error) {
	verdicts, prior, err := s.listVerdicts(ctx, project)
	if err != nil {
		return "", err
	}
	repairs, err := s.repairCounts(ctx, run.ID)
	if err != nil {
		return "", err
	}
	return (Policy{}).RepairHold(project, run, Evidence{Verdicts: verdicts, Prior: prior, Repairs: repairs})
}

// repairCounts 统计各章自最近一次策略事件以来已派发的自动修订任务数。预算管的是无人过问的
// 连续修订：运行创建、用户调整策略、或在预算用尽时确认继续，都重新起算（D63）。
func (s *Reviews) repairCounts(ctx context.Context, runID string) (map[string]int, error) {
	events, err := s.store.ListCreationRunEvents(ctx, runID)
	if err != nil {
		return nil, err
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
		var payload struct {
			OperationID string `json:"operation_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("run %q event %d payload is corrupt: %w", runID, event.Sequence, err)
		}
		op, err := s.store.GetOperation(ctx, payload.OperationID)
		if err != nil {
			return nil, err
		}
		spec, err := model.KindSpec(op.Kind)
		if err != nil {
			return nil, err
		}
		if !spec.Repair {
			continue
		}
		input, err := model.DecodeTaskInput(op.Kind, op.Input)
		if err != nil {
			return nil, err
		}
		for _, id := range model.RepairChapters(input) {
			repairs[id]++
		}
	}
	return repairs, nil
}
