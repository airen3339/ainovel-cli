package model

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// 语义范围（D77）：设定牵连写到主体的章与它的来源章，人物牵连写到它的章；大纲只有梗概
// 或归属变了才牵连节点下已写的章，改名与新增不牵连。设定与人物还牵连点到主体（本名或
// 别名）的没写成正文的规划（D87）：没写的章纲与卷弧梗概，关于主体或值里点到它的规划期
// 事实，作品前提；已写章的章纲与事实随正文走，不在其中。
func TestSemanticScope(t *testing.T) {
	put := func(kind DocumentKind, id string, value any) Patch {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return Patch{Document: DocumentRef{Kind: kind, ID: id}, Operation: PatchPut, Content: content}
	}
	hero := DocumentRef{Kind: DocumentEntity, ID: "hero"}
	documents := []Patch{
		put(DocumentPlan, "volume-1", PlanNode{ID: "volume-1", Kind: PlanVolume, Title: "卷一", Summary: "入道"}),
		put(DocumentPlan, "arc-1", PlanNode{ID: "arc-1", Kind: PlanArc, ParentID: "volume-1", Title: "山门", Summary: "入门"}),
		put(DocumentPlan, "arc-2", PlanNode{ID: "arc-2", Kind: PlanArc, ParentID: "volume-1", Order: 1, Title: "下山", Summary: "阿渡离开宗门"}),
		put(DocumentPlan, "plan-1", PlanNode{ID: "plan-1", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Title: "第一章", Summary: "陈渡抵达"}),
		put(DocumentPlan, "plan-2", PlanNode{ID: "plan-2", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Title: "第二章", Summary: "拜师"}),
		put(DocumentPlan, "plan-4", PlanNode{ID: "plan-4", Kind: PlanChapter, Turn: "转折", ParentID: "arc-2", Order: 4, Title: "陈渡下山", Summary: "告别"}),
		put(DocumentEntity, "hero", Entity{ID: "hero", Kind: EntityCharacter, Name: "陈渡", Aliases: []string{"阿渡"}}),
		put(DocumentManuscript, "chapter-1", ManuscriptChapter{ID: "chapter-1", PlanNodeID: "plan-1", Number: 1, DependsOn: []DocumentRef{hero}}),
		put(DocumentManuscript, "chapter-2", ManuscriptChapter{ID: "chapter-2", PlanNodeID: "plan-2", Number: 2}),
		put(DocumentCanon, "vow", CanonFact{ID: "vow", Kind: CanonEvent, SubjectID: "mentor", Predicate: "event.vow", Value: json.RawMessage(`"立誓"`), SourceChapterID: "chapter-2"}),
		put(DocumentCanon, "fate", CanonFact{ID: "fate", Kind: CanonState, SubjectID: "mentor", Predicate: "state.fate", Value: json.RawMessage(`"阿渡送别师父"`)}),
		put(DocumentIntent, SingletonDocumentID, Intent{Premise: "陈渡入道"}),
	}
	base := make(DocumentSet)
	for _, patch := range documents {
		base[patch.Document.Key()] = DocumentVersion{Document: patch.Document, Content: patch.Content}
	}
	scope := func(patches ...Patch) []string {
		refs, err := SemanticScope(patches, base)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, ref := range refs {
			ids = append(ids, ref.ID)
		}
		return ids
	}
	for _, c := range []struct {
		name    string
		patches []Patch
		want    []string
	}{
		{"canon about a subject", []Patch{put(DocumentCanon, "origin", CanonFact{ID: "origin", Kind: CanonState, SubjectID: "hero", Predicate: "state.origin", Value: json.RawMessage(`"孤儿"`)})}, []string{"fate", SingletonDocumentID, "chapter-1", "arc-2", "plan-4"}},
		{"canon from its source chapter", []Patch{{Document: DocumentRef{Kind: DocumentCanon, ID: "vow"}, Operation: PatchDelete}}, []string{"fate", "chapter-2"}},
		{"entity renamed", []Patch{put(DocumentEntity, "hero", Entity{ID: "hero", Kind: EntityCharacter, Name: "阿青"})}, []string{"fate", SingletonDocumentID, "chapter-1", "arc-2", "plan-4"}},
		{"entity by its new name", []Patch{put(DocumentEntity, "elder", Entity{ID: "elder", Kind: EntityCharacter, Name: "宗门"})}, []string{"arc-2"}},
		{"retitle", []Patch{put(DocumentPlan, "plan-1", PlanNode{ID: "plan-1", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Title: "山门之前", Summary: "陈渡抵达"})}, nil},
		{"new chapter plan", []Patch{put(DocumentPlan, "plan-3", PlanNode{ID: "plan-3", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Title: "第三章", Summary: "下山"})}, nil},
		{"chapter summary", []Patch{put(DocumentPlan, "plan-2", PlanNode{ID: "plan-2", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Title: "第二章", Summary: "拒绝拜师"})}, []string{"chapter-2"}},
		{"arc summary", []Patch{put(DocumentPlan, "arc-1", PlanNode{ID: "arc-1", Kind: PlanArc, ParentID: "volume-1", Title: "山门", Summary: "落选"})}, []string{"chapter-1", "chapter-2"}},
	} {
		if got := scope(c.patches...); !slices.Equal(got, c.want) {
			t.Errorf("%s: scope = %v, want %v", c.name, got, c.want)
		}
	}
}

// 出入落在没写成正文的规划上（大纲、规划期事实、作品前提）的修改不完整（D87）：报告进不了
// 提案，退回提出者一并改；落在已写章节的照常等用户选处置。
func TestPlanningFindingsSendTheChangeBack(t *testing.T) {
	base := DocumentSet{(DocumentRef{Kind: DocumentManuscript, ID: "chapter-1"}).Key(): {}}
	options := `"options":[{"strategy":"rewrite_affected","chapter_ids":["chapter-1"],"explanation":"重写"},` +
		`{"strategy":"reinterpret_future","explanation":"往后解释"},{"strategy":"abandon","explanation":"放弃"}]`
	_, err := DecodeSemanticImpact(json.RawMessage(`{"status":"conflict","findings":[`+
		`{"document":{"kind":"manuscript","id":"chapter-1"},"explanation":"第 1 章用钥匙开门"},`+
		`{"document":{"kind":"plan","id":"plan-3"},"explanation":"第 3 章纲还写着铜钥匙"},`+
		`{"document":{"kind":"canon","id":"fate"},"explanation":"规划里还写着铜钥匙"}],`+options+`}`), base)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `"plan:plan-3": 第 3 章纲还写着铜钥匙; "canon:fate": 规划里还写着铜钥匙`) {
		t.Fatalf("planning finding = %v", err)
	}
	report, err := DecodeSemanticImpact(json.RawMessage(`{"status":"conflict","findings":[`+
		`{"document":{"kind":"manuscript","id":"chapter-1"},"explanation":"第 1 章用钥匙开门"}],`+options+`}`), base)
	if err != nil || len(report.Options) != 3 {
		t.Fatalf("chapter finding = %+v, %v", report, err)
	}
}
