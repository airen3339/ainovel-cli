package narrative

import (
	"fmt"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// ImpactInput 是语义影响分析（S4）看到的改动，故事语言（D66）：被改的那几处改前改后的样子，
// 可能受牵连的已写章节与其余设定。
type ImpactInput struct {
	Reason   string        `json:"reason"`
	Before   ImpactSide    `json:"before"`
	After    ImpactSide    `json:"after"`
	Chapters []ChapterText `json:"written_chapters,omitempty"`
	Related  ImpactSide    `json:"related,omitzero"`
}

// ImpactSide 是一组大纲、实体与设定。
type ImpactSide struct {
	Outline  []VolumeView `json:"outline,omitempty"`
	Entities []EntityView `json:"entities,omitempty"`
	Facts    []FactView   `json:"facts,omitempty"`
}

// ImpactReport 是分析器的结论：出入与处置涉及的章按章号说。
type ImpactReport struct {
	Status   model.SemanticImpactStatus `json:"status"`
	Findings []ImpactFinding            `json:"findings"`
	Options  []ImpactOption             `json:"options"`
}

type ImpactFinding struct {
	Chapter     int    `json:"chapter,omitempty"`
	Explanation string `json:"explanation"`
}

type ImpactOption struct {
	Strategy    model.ResolutionStrategy `json:"strategy"`
	Chapters    []int                    `json:"chapters,omitempty"`
	Explanation string                   `json:"explanation"`
}

// Impact 渲染改动的语义影响输入：s 是改前的故事；direct 是改动直接碰到的文档，affected 是
// 可能受牵连的。
func (s *Story) Impact(reason string, patches []model.Patch, direct, affected []model.DocumentRef) (ImpactInput, error) {
	after, err := s.Apply(patches)
	if err != nil {
		return ImpactInput{}, err
	}
	input := ImpactInput{Reason: reason, Before: s.side(direct), After: after.side(direct)}
	var chapters []string
	var related []model.DocumentRef
	for _, ref := range affected {
		if ref.Kind == model.DocumentManuscript {
			chapters = append(chapters, ref.ID)
		} else {
			related = append(related, ref)
		}
	}
	input.Chapters, input.Related = s.Texts(chapters), s.side(related)
	return input, nil
}

func (s *Story) side(refs []model.DocumentRef) ImpactSide {
	ids := make(map[model.DocumentKind][]string)
	for _, ref := range refs {
		ids[ref.Kind] = append(ids[ref.Kind], ref.ID)
	}
	side := ImpactSide{Entities: s.Entities(ids[model.DocumentEntity]), Facts: s.Facts(ids[model.DocumentCanon])}
	if plans := ids[model.DocumentPlan]; len(plans) > 0 {
		side.Outline = s.Outline(plans)
	}
	return side
}

// Report 把故事语言的结论换回文档引用：章号对应已写的正文。
func (s *Story) Report(report ImpactReport) (model.SemanticImpactReport, error) {
	converted := model.SemanticImpactReport{
		Status:   report.Status,
		Findings: make([]model.SemanticImpactFinding, 0, len(report.Findings)),
		Options:  make([]model.ResolutionOption, 0, len(report.Options)),
	}
	for _, finding := range report.Findings {
		item := model.SemanticImpactFinding{Explanation: finding.Explanation}
		if finding.Chapter > 0 {
			chapter, err := s.manuscript(finding.Chapter)
			if err != nil {
				return model.SemanticImpactReport{}, fmt.Errorf("finding: %w", err)
			}
			item.Document = &model.DocumentRef{Kind: model.DocumentManuscript, ID: chapter.ID}
		}
		converted.Findings = append(converted.Findings, item)
	}
	for _, option := range report.Options {
		item := model.ResolutionOption{Strategy: option.Strategy, Explanation: option.Explanation}
		for _, number := range option.Chapters {
			chapter, err := s.manuscript(number)
			if err != nil {
				return model.SemanticImpactReport{}, fmt.Errorf("option %s: %w", option.Strategy, err)
			}
			item.ChapterIDs = append(item.ChapterIDs, chapter.ID)
		}
		converted.Options = append(converted.Options, item)
	}
	return converted, nil
}
