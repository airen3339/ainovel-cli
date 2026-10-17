package novel

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
)

func directiveTestProject() projectdoc.Snapshot {
	return projectdoc.Snapshot{
		Plan: []model.PlanNode{
			{ID: "volume-1", Kind: model.PlanVolume, Order: 1, Title: "卷一"},
			{ID: "arc-1", Kind: model.PlanArc, ParentID: "volume-1", Order: 1, Title: "弧一"},
			{ID: "chapter-plan-1", Kind: model.PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 1, Title: "第一章"},
			{ID: "chapter-plan-2", Kind: model.PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 2, Title: "第二章"},
		},
		Manuscript: []model.ManuscriptChapter{
			{ID: "chapter-1", PlanNodeID: "chapter-plan-1", Number: 1},
			{ID: "chapter-2", PlanNodeID: "chapter-plan-2", Number: 2},
		},
		Directives: []model.Directive{
			{ID: "d-all", Scope: model.DirectiveScopeProject, Text: "每章结尾留钩子", Status: model.DirectiveActive},
			{ID: "d-arc", Scope: "plan_node:arc-1", Text: "弧一压住感情线", Status: model.DirectiveActive},
			{ID: "d-ch2", Scope: "plan_node:chapter-plan-2", Text: "第二章加一场雨", Status: model.DirectiveActive},
			{ID: "d-later", Scope: "from_chapter:3", Text: "第三章起换视角", Status: model.DirectiveActive},
			{ID: "d-old", Scope: model.DirectiveScopeProject, Text: "已退役", Status: model.DirectiveRetired},
		},
	}
}

func directiveIDs(directives []model.Directive) []string {
	ids := make([]string, 0, len(directives))
	for _, directive := range directives {
		ids = append(ids, directive.ID)
	}
	return ids
}

// 章节任务按目标章装配要求（§4.9）：写第 1 章命中全书与所在弧的，重写第 2 章还命中本章的。
func TestChapterWorkCarriesCoveringDirectives(t *testing.T) {
	project := directiveTestProject()
	run := model.CreationRun{ID: "run-1", ProjectID: "book-1"}
	write := writeWork(project, run, chapterSlot{number: 1, plan: project.Plan[2]})
	if got := directiveIDs(write.Input.(model.WriteChapterInput).Directives); !slices.Equal(got, []string{"d-all", "d-arc"}) {
		t.Fatalf("write directives = %v", got)
	}
	rewrite := rewriteWork(project, run, chapterSlot{number: 2, plan: project.Plan[3], chapterID: "chapter-2"}, []string{"节奏太慢"}, 7)
	input, ok := rewrite.Input.(model.RewriteChapterInput)
	if !ok || rewrite.Kind != model.OperationRewriteChapter || rewrite.ID != "run-1:rewrite:chapter-2:r7" {
		t.Fatalf("rewrite work = %#v", rewrite)
	}
	if input.ChapterID != "chapter-2" || input.ChapterPlanID != "chapter-plan-2" || !slices.Equal(input.Findings, []string{"节奏太慢"}) {
		t.Fatalf("rewrite input = %+v", input)
	}
	if got := directiveIDs(input.Directives); !slices.Equal(got, []string{"d-all", "d-arc", "d-ch2"}) {
		t.Fatalf("rewrite directives = %v", got)
	}
	// 章节任务基线只钉本章的要求作用域（D51）：相干要求变化才使任务失效。
	if len(input.Basis.Documents) != 0 || len(input.Basis.Scopes) != 1 || input.Basis.Scopes[0].Target.ChapterNumber != 2 {
		t.Fatalf("rewrite basis = %+v", input.Basis)
	}
	if rewrite.Reasons.Waiting == "" || rewrite.Reasons.Failure == "" || rewrite.Reasons.Stuck == "" {
		t.Fatalf("rewrite reasons = %#v", rewrite.Reasons)
	}
}

// 审阅以要求核验项投递（D62）：命中窗口的 active 要求；作用域全在目标之外的跳过。
func TestReviewWorkCarriesWindowRequirements(t *testing.T) {
	project := directiveTestProject()
	project.Revision = 7
	project.Index = projectdoc.DocumentIndex{}
	for _, ref := range []model.DocumentRef{
		{Kind: model.DocumentIntent, ID: model.SingletonDocumentID},
		{Kind: model.DocumentManuscript, ID: "chapter-1"},
		{Kind: model.DocumentPlan, ID: "chapter-plan-1"},
	} {
		project.Index[ref.Key()] = projectdoc.DocumentEntry{Ref: ref, Revision: 3}
	}
	run := model.CreationRun{ID: "run-1", ProjectID: "book-1"}
	ledger := newReviewLedger(project, 2, nil)
	work, err := reviewWork(project, run, ledger, gateStep{review: []string{"chapter-1"}})
	if err != nil {
		t.Fatalf("review work: %v", err)
	}
	input, ok := work.Input.(model.ReviewRangeInput)
	if !ok || work.ID != reviewOperationID("run-1", 7, input) || !strings.HasPrefix(work.ID, "run-1:review:chapter-1:r7:") {
		t.Fatalf("review work = %#v", work)
	}
	if got := input.Requirements; len(got) != 2 || got[0].ID != "directive:d-all" || got[1].ID != "directive:d-arc" || got[0].Settle {
		t.Fatalf("review requirements = %+v", got)
	}
}

func TestWriteInputWithoutDirectivesOmitsTheField(t *testing.T) {
	project := directiveTestProject()
	project.Directives = nil
	work := writeWork(project, model.CreationRun{ID: "run-1"}, chapterSlot{number: 1, plan: project.Plan[2]})
	encoded, err := json.Marshal(work.Input)
	if err != nil {
		t.Fatalf("encode write input: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("decode write input: %v", err)
	}
	if _, present := raw["directives"]; present {
		t.Fatalf("write input without directives must omit the field: %s", encoded)
	}
}
