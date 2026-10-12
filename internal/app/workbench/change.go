package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// PendingChange 是主编提出、等用户确认的修改（D77）。Options 非空时改动与已写章节有
// 冲突，用户在三种处置里选一种（S4），Findings 是影响分析给的冲突说明；Stale 表示其后
// 作品又变了，不能照它确认，要主编按最新内容重新提。
type PendingChange struct {
	Proposal model.Proposal `json:"proposal"`
	View     ProposalView   `json:"view"`
	Findings []string       `json:"findings,omitempty"`
	Options  []ChangeOption `json:"options,omitempty"`
	Stale    bool           `json:"stale,omitempty"`
}

// ChangeOption 是一种处置：重写哪几章，或只改设定、后文按新设定写，或不改了。
type ChangeOption struct {
	Strategy    model.ResolutionStrategy `json:"strategy"`
	Chapters    []int                    `json:"chapters,omitempty"`
	Explanation string                   `json:"explanation"`
}

func (s *Query) pendingChange(ctx context.Context, project projectdoc.Snapshot) (*PendingChange, error) {
	proposal, ok, err := s.decisions.PendingChange(ctx, project.ID)
	if err != nil || !ok {
		return nil, err
	}
	change := &PendingChange{Proposal: proposal}
	if _, _, err := s.decisions.Relocate(ctx, proposal); errors.Is(err, model.ErrRevisionConflict) {
		change.Stale = true
	} else if err != nil {
		return nil, err
	}
	if change.View, err = proposalView(project, proposal.Patches); err != nil {
		return nil, err
	}
	if len(proposal.Impact.Semantic) == 0 {
		return change, nil
	}
	var report model.SemanticImpactReport
	if err := json.Unmarshal(proposal.Impact.Semantic, &report); err != nil {
		return nil, fmt.Errorf("decode semantic impact: %w", err)
	}
	if report.Status == model.SemanticImpactConsistent {
		return change, nil
	}
	numbers := make(map[string]int, len(project.Manuscript))
	for _, chapter := range project.Manuscript {
		numbers[chapter.ID] = chapter.Number
	}
	for _, finding := range report.Findings {
		change.Findings = append(change.Findings, finding.Explanation)
	}
	for _, option := range report.Options {
		choice := ChangeOption{Strategy: option.Strategy, Explanation: option.Explanation}
		for _, id := range option.ChapterIDs {
			choice.Chapters = append(choice.Chapters, numbers[id])
		}
		change.Options = append(change.Options, choice)
	}
	slices.SortFunc(change.Options, func(a, b ChangeOption) int {
		return slices.Index(resolutionOrder, a.Strategy) - slices.Index(resolutionOrder, b.Strategy)
	})
	return change, nil
}

// resolutionOrder 是处置的呈现顺序，也是用户按键选的序号。
var resolutionOrder = []model.ResolutionStrategy{model.ResolutionRewriteAffected, model.ResolutionReinterpretFuture, model.ResolutionAbandon}

// ChangeRecord 是一次生效的变更：谁、何时、为什么、逐处改了什么，此刻能否撤销。
type ChangeRecord struct {
	ID       string    `json:"id"`
	At       time.Time `json:"at"`
	By       string    `json:"by"`
	Reason   string    `json:"reason,omitempty"`
	Changes  []string  `json:"changes"`
	Undoable bool      `json:"undoable"`
}

// History 是作品最近生效的 limit 次变更，新的在前；每次都对照它生效前的作品描述。
func (s *Query) History(ctx context.Context, projectID string, limit int) ([]ChangeRecord, error) {
	history, err := s.decisions.History(ctx, projectID, limit)
	if err != nil {
		return nil, err
	}
	records := make([]ChangeRecord, 0, len(history))
	for _, change := range history {
		before, err := s.projects.Project(ctx, projectID, change.NewRevision-1)
		if err != nil {
			return nil, err
		}
		view, err := proposalView(before, change.Patches)
		if err != nil {
			return nil, err
		}
		at := change.CreatedAt
		if change.DecidedAt != nil {
			at = *change.DecidedAt
		}
		records = append(records, ChangeRecord{
			ID: change.ID, At: at, By: authorLabel(change.Author), Reason: change.Reason, Changes: view.Changes, Undoable: change.Undoable,
		})
	}
	return records, nil
}

func authorLabel(author model.Author) string {
	switch {
	case author.Kind == model.AuthorUser:
		return "用户"
	case author == model.ChiefAuthor:
		return "主编"
	case author.Kind == model.AuthorAI:
		return "创作团队"
	case author.Kind == model.AuthorExtension:
		return "扩展"
	}
	return "系统"
}
