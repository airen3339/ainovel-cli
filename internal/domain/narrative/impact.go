package narrative

import (
	"fmt"
	"slices"
	"strings"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// 一次修改的语义影响分两个问题问（D87），各自只给它要看的材料：
//   - 已写章节（S4）：改动与已经写下的正文是否冲突。冲突等用户选处置。
//   - 没写成正文的规划：章纲、卷与故事弧梗概，规划时定下的事实，作品前提，是否还按改前的
//     设定写。是就退回提出者一并改。
// 两样材料一起给，分析器会把“改后的规划与旧正文对不上”记到规划头上：规划明明改对了，
// 修改却被退回，提出者怎么改都过不去。

// ImpactInput 是已写章节的语义影响分析看到的改动，故事语言（D66）：被改的那几处改前改后的
// 样子（没写的规划除外），可能受牵连的已写章节与其余设定。
type ImpactInput struct {
	Reason   string        `json:"reason"`
	Before   ImpactSide    `json:"before"`
	After    ImpactSide    `json:"after"`
	Chapters []ChapterText `json:"written_chapters,omitempty"`
	Related  ImpactSide    `json:"related,omitzero"`
}

// PlanningInput 是规划核对看到的：设定与人物的改前改后，以及改动碰到或点到被改主体的没写
// 成正文的规划（改后的样子）。没有已写正文。
//
// 核对是逐处下结论，不是“找出有问题的”：开放地找，模型报出一处就停，主编补一处退一次；
// 落点靠章、故事弧、卷三个号码自报，也说不清“恰好一处”。所以由宿主列出要核对的每一处，
// 模型对每一处都给结论。
type PlanningInput struct {
	Reason  string       `json:"reason"`
	Before  ImpactSide   `json:"before"`
	After   ImpactSide   `json:"after"`
	Outline []VolumeView `json:"outline,omitempty"`
	Facts   []FactView   `json:"planned_facts,omitempty"`
	Premise string       `json:"premise,omitempty"`
	checks  []planningCheck
}

// planningCheck 是要下结论的一处规划：称呼（第 N 章、第 N 个故事弧、第 N 卷，「主体」谓词，
// 作品前提）与它的文档。
type planningCheck struct {
	label string
	ref   model.DocumentRef
}

// premiseLabel 是作品前提在规划核对里的称呼。
const premiseLabel = "作品前提"

// PlanningVerdict 是对一处规划的结论：explanation 说它写到这次改动的什么、对不对得上；
// stale 表示它还按改前的设定写。
type PlanningVerdict struct {
	Explanation string `json:"explanation"`
	Stale       bool   `json:"stale"`
}

// ImpactSide 是一组大纲、实体与设定。
type ImpactSide struct {
	Outline  []VolumeView `json:"outline,omitempty"`
	Entities []EntityView `json:"entities,omitempty"`
	Facts    []FactView   `json:"facts,omitempty"`
}

// ImpactReport 是已写章节分析的结论：出入与处置涉及的章按章号说，只落在已写的章上。
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

// Impact 渲染两个问题各自的输入：s 是改前的故事；direct 是改动直接碰到的文档，affected 是
// 可能受牵连的。没写成正文的规划只进规划核对；已写章的章纲随正文走，留在已写章节的分析里；
// 改动写下的事实与人物是设定本身，两个问题都拿它对照。没有要核对的规划时 planning 为空。
func (s *Story) Impact(reason string, patches []model.Patch, direct, affected []model.DocumentRef) (ImpactInput, PlanningInput, error) {
	after, err := s.Apply(patches)
	if err != nil {
		return ImpactInput{}, PlanningInput{}, err
	}
	// setting 是改动里不属于没写规划的部分（设定、人物、已写章的章纲），written 是其中的设定
	// 与人物：规划核对只拿它对照。
	changed := make(map[model.DocumentRef]bool, len(direct))
	var chapters []string
	var setting, written, planning, related []model.DocumentRef
	for _, ref := range direct {
		changed[ref] = true
		switch {
		case ref.Kind == model.DocumentPlan && s.writtenPlan(ref.ID):
			setting = append(setting, ref)
		case ref.Kind == model.DocumentPlan || ref.Kind == model.DocumentIntent:
			planning = append(planning, ref)
		default:
			setting, written = append(setting, ref), append(written, ref)
		}
	}
	for _, ref := range affected {
		switch {
		case changed[ref]:
		case ref.Kind == model.DocumentManuscript:
			chapters = append(chapters, ref.ID)
		case s.planning(ref):
			planning = append(planning, ref)
		default:
			related = append(related, ref)
		}
	}
	impact := ImpactInput{Reason: reason, Before: s.side(setting), After: after.side(setting), Chapters: s.Texts(chapters), Related: after.side(related)}
	var check PlanningInput
	if checks := after.planningChecks(planning); len(checks) > 0 {
		check = PlanningInput{Reason: reason, Before: s.side(written), After: after.side(written), checks: checks}
		var plans, facts []string
		for _, c := range checks {
			switch c.ref.Kind {
			case model.DocumentPlan:
				plans = append(plans, c.ref.ID)
			case model.DocumentCanon:
				facts = append(facts, c.ref.ID)
			case model.DocumentIntent:
				check.Premise = after.content.Intent.Premise
			}
		}
		if len(plans) > 0 {
			check.Outline = after.Outline(plans)
		}
		check.Facts = after.Facts(facts)
	}
	return impact, check, nil
}

// planning 判断文档是不是没写成正文的规划（D87）：没写的章纲与卷、故事弧梗概，规划时定下的
// 事实（没有来源章），作品前提。
func (s *Story) planning(ref model.DocumentRef) bool {
	switch ref.Kind {
	case model.DocumentPlan:
		return !s.writtenPlan(ref.ID)
	case model.DocumentCanon:
		fact, ok := s.facts[ref.ID]
		return ok && fact.SourceChapterID == ""
	case model.DocumentIntent:
		return true
	}
	return false
}

// planningChecks 按改后的故事给要核对的规划逐处起称呼：卷、故事弧、章，规划期事实，作品前提；
// 这次删掉的不再核对。
func (s *Story) planningChecks(refs []model.DocumentRef) []planningCheck {
	include := make(map[model.DocumentRef]bool, len(refs))
	for _, ref := range refs {
		include[ref] = true
	}
	var checks []planningCheck
	for _, kind := range []model.PlanNodeKind{model.PlanVolume, model.PlanArc, model.PlanChapter} {
		for _, node := range s.ordered[kind] {
			if ref := (model.DocumentRef{Kind: model.DocumentPlan, ID: node.ID}); include[ref] {
				checks = append(checks, planningCheck{label: s.ordinal(node), ref: ref})
			}
		}
	}
	var facts []planningCheck
	for _, fact := range s.content.Canon {
		if ref := (model.DocumentRef{Kind: model.DocumentCanon, ID: fact.ID}); include[ref] {
			facts = append(facts, planningCheck{label: s.factLabel(fact), ref: ref})
		}
	}
	slices.SortFunc(facts, func(a, b planningCheck) int { return strings.Compare(a.label, b.label) })
	checks = append(checks, facts...)
	if ref := (model.DocumentRef{Kind: model.DocumentIntent, ID: model.SingletonDocumentID}); include[ref] {
		checks = append(checks, planningCheck{label: premiseLabel, ref: ref})
	}
	return checks
}

// Checks 是要逐处下结论的规划的称呼；为空时不用核对。
func (in PlanningInput) Checks() []string {
	labels := make([]string, len(in.checks))
	for i, check := range in.checks {
		labels[i] = check.label
	}
	return labels
}

// Stale 把逐处的结论换回文档：每一处都要有结论，不多不少；还按改前设定写的成为出入。
// 报错点名漏了哪几处，分析器照着补。
func (in PlanningInput) Stale(verdicts map[string]PlanningVerdict) ([]model.SemanticImpactFinding, error) {
	var stale []model.SemanticImpactFinding
	var missing []string
	for _, check := range in.checks {
		verdict, ok := verdicts[check.label]
		switch {
		case !ok:
			missing = append(missing, check.label)
		case verdict.Stale:
			ref := check.ref
			stale = append(stale, model.SemanticImpactFinding{Document: &ref, Explanation: verdict.Explanation})
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("规划核对漏了%s: %w", strings.Join(missing, "、"), model.ErrInvalid)
	}
	if len(verdicts) != len(in.checks) {
		return nil, fmt.Errorf("规划核对只对%s下结论，不对别处: %w", strings.Join(in.Checks(), "、"), model.ErrInvalid)
	}
	return stale, nil
}

// writtenPlan 判断计划节点是不是已写的章：它的章纲随正文走。
func (s *Story) writtenPlan(id string) bool {
	_, ok := s.written[id]
	return ok
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

// Report 把已写章节分析的结论换回文档引用：章号对应已写的正文。
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
