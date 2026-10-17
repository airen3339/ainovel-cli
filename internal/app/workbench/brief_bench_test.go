package workbench

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/voocel/ainovel-cli/internal/app/novel"
	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// 长篇的依据随工作台每次刷新重算：500 章、每章 10 条设定、20 条全书要求、每 5 章一份
// 裁定。要求覆盖按章计算一次，不随要求数 × 章数 × 大纲节点数增长。
func BenchmarkChapterBriefsLongBook(b *testing.B) {
	const volumes, arcs, chapters = 5, 10, 10
	project := projectdoc.Snapshot{Index: projectdoc.DocumentIndex{}, Intent: model.Intent{Required: []string{"保持悬疑"}, Forbidden: []string{"主角死亡"}}}
	for i := range 20 {
		project.Directives = append(project.Directives, model.Directive{ID: fmt.Sprint("d", i), Scope: model.DirectiveScopeProject, Text: "要求", Status: model.DirectiveActive})
	}
	current := map[string]*novel.StoredVerdict{}
	var window []string
	for n := 1; n <= volumes*arcs*chapters; n++ {
		volume, arc := fmt.Sprint("v", (n-1)/(arcs*chapters)), fmt.Sprint("a", (n-1)/chapters)
		if (n-1)%(arcs*chapters) == 0 {
			project.Plan = append(project.Plan, model.PlanNode{ID: volume, Kind: model.PlanVolume, Order: n})
		}
		if (n-1)%chapters == 0 {
			project.Plan = append(project.Plan, model.PlanNode{ID: arc, Kind: model.PlanArc, ParentID: volume, Order: n})
		}
		plan, chapter := fmt.Sprint("p", n), fmt.Sprint("c", n)
		project.Plan = append(project.Plan, model.PlanNode{ID: plan, Kind: model.PlanChapter, Turn: "转折", ParentID: arc, Order: n})
		project.Manuscript = append(project.Manuscript, model.ManuscriptChapter{ID: chapter, PlanNodeID: plan, Number: n})
		for f := range 10 {
			project.Canon = append(project.Canon, model.CanonFact{ID: fmt.Sprint("f", n, "-", f), Kind: model.CanonState, Predicate: "state.x", Value: json.RawMessage(`"值"`), SourceChapterID: chapter})
		}
		if window = append(window, chapter); len(window) == 5 {
			stored := &novel.StoredVerdict{Key: chapter, Verdict: model.ReviewVerdict{ChapterIDs: window}}
			for _, id := range window {
				current[id] = stored
			}
			window = nil
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		chapterBriefs(project, len(project.Manuscript), current, nil)
	}
}
