package workbench

import (
	"slices"

	"github.com/voocel/ainovel-cli/internal/app/novel"
	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/domain/narrative"
)

// ChapterBrief 是一章的依据（页面设计 §3 右栏）：它推进的长线、覆盖它的要求与审阅对每项
// 的结论，以及这一章记下的设定。规划摘要在大纲里，审阅发现在 Findings 里。
type ChapterBrief struct {
	Threads      []BriefThread      `json:"threads,omitempty"`
	Requirements []BriefRequirement `json:"requirements,omitempty"`
	Facts        []BriefFact        `json:"facts,omitempty"`
}

// Thread 是一条长线（D91）的进度：最近推进它的已写章、此后停了几章、下一次推进它的未写章
// 与作答章，与模型看到的同一份统计。
type Thread = narrative.ThreadView

// BriefThread 是这一章推进的一条长线（D91）；Answer 表示答案落在这一章。
type BriefThread struct {
	Name   string `json:"name"`
	Answer bool   `json:"answer,omitempty"`
}

// BriefRequirement 是覆盖这一章的一项要求；Status 是包含这一章的当前裁定对它的结论
// （model.CheckSatisfied / CheckViolated / CheckPending），还没有有效裁定时为空；那一窗判
// pending、要求已在作用域内别处兑现的，记为 satisfied。Rework 表示
// 这一章的正文写在这条返工要求之前，要按它重写（D78）。
type BriefRequirement struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	Scope     string `json:"scope,omitempty"` // 用户要求的作用域（故事语言）；全书为空
	Forbidden bool   `json:"forbidden,omitempty"`
	Status    string `json:"status,omitempty"`
	Rework    bool   `json:"rework,omitempty"`
}

// BriefFact 是这一章记下的一条设定；Pending 表示正文改动后待核验（D41）。
type BriefFact struct {
	Fact
	Pending bool `json:"pending,omitempty"`
}

// Fact 是给读者看的一条设定：种类、主体与内容，不带谓词键（D66：故事语言）。
type Fact struct {
	Kind    model.CanonFactKind `json:"kind"`
	Subject string              `json:"subject"`
	Text    string              `json:"text"`
}

func factOf(story *narrative.Story, fact model.CanonFact) Fact {
	subject, text := story.FactNote(fact)
	return Fact{Kind: fact.Kind, Subject: subject, Text: text}
}

// chapterBriefs 按章号组装各已规划章节的依据：要求与审阅闸门同一份清单、同一覆盖规则，
// 结论取该章的当前裁定（含用户接受的发现，D43），已兑现的要求（satisfied）不再停在 pending。
func chapterBriefs(project projectdoc.Snapshot, final int, current map[string]*novel.StoredVerdict, satisfied map[string]bool) map[int]ChapterBrief {
	plans := model.ChapterPlansInOrder(project.Plan)
	written := make(map[string]model.ManuscriptChapter, len(project.Manuscript)) // plan node id → 正文章节
	numbers := make(map[string]int, len(project.Manuscript))                     // 正文章节 id → 章号
	for _, chapter := range project.Manuscript {
		written[chapter.PlanNodeID], numbers[chapter.ID] = chapter, chapter.Number
	}
	briefs := make(map[int]ChapterBrief, len(plans))
	if project.Compass != nil {
		for index, plan := range plans {
			for _, thread := range project.Compass.Threads {
				if answer := thread.Answer == plan.ID; answer || slices.Contains(plan.Threads, thread.ID) {
					brief := briefs[index+1]
					brief.Threads = append(brief.Threads, BriefThread{Name: thread.Name, Answer: answer})
					briefs[index+1] = brief
				}
			}
		}
	}
	reworks := novel.ReworksDue(project)
	effective := make(map[string]model.ReviewVerdict)
	for number, requirements := range novel.RequirementsByChapter(project, final) {
		var verdict model.ReviewVerdict
		if stored := current[written[plans[number-1].ID].ID]; stored != nil {
			if _, ok := effective[stored.Key]; !ok {
				effective[stored.Key] = stored.Effective()
			}
			verdict = effective[stored.Key]
		}
		brief := briefs[number]
		due := reworks[written[plans[number-1].ID].ID].Directives
		for _, requirement := range requirements {
			status := verdict.CheckStatus(requirement.ID)
			if status == model.CheckPending && satisfied[requirement.ID] {
				status = model.CheckSatisfied
			}
			brief.Requirements = append(brief.Requirements, BriefRequirement{
				ID: requirement.ID, Text: requirement.Text, Scope: requirement.Scope,
				Forbidden: requirement.Forbidden, Status: status,
				Rework: slices.ContainsFunc(due, func(directive model.Directive) bool {
					return novel.DirectiveRequirementID(directive.ID) == requirement.ID
				}),
			})
		}
		briefs[number] = brief
	}
	var pending []string
	for _, gap := range novel.CanonGaps(project) {
		pending = append(pending, gap.Pending...)
	}
	story := storyOf(project)
	for _, fact := range project.Canon {
		number := numbers[fact.SourceChapterID]
		if number == 0 {
			continue
		}
		brief := briefs[number]
		brief.Facts = append(brief.Facts, BriefFact{Fact: factOf(story, fact), Pending: slices.Contains(pending, fact.ID)})
		briefs[number] = brief
	}
	return briefs
}
