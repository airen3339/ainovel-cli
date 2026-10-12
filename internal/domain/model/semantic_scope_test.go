package model

import (
	"encoding/json"
	"slices"
	"testing"
)

// 语义范围（D77）：设定牵连写到主体的章与它的来源章，人物牵连写到它的章；大纲只有梗概
// 或归属变了才牵连节点下已写的章，改名与新增不牵连。
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
		put(DocumentPlan, "plan-1", PlanNode{ID: "plan-1", Kind: PlanChapter, ParentID: "arc-1", Title: "第一章", Summary: "抵达"}),
		put(DocumentPlan, "plan-2", PlanNode{ID: "plan-2", Kind: PlanChapter, ParentID: "arc-1", Title: "第二章", Summary: "拜师"}),
		put(DocumentManuscript, "chapter-1", ManuscriptChapter{ID: "chapter-1", PlanNodeID: "plan-1", Number: 1, DependsOn: []DocumentRef{hero}}),
		put(DocumentManuscript, "chapter-2", ManuscriptChapter{ID: "chapter-2", PlanNodeID: "plan-2", Number: 2}),
		put(DocumentCanon, "vow", CanonFact{ID: "vow", Kind: CanonEvent, SubjectID: "mentor", Predicate: "event.vow", Value: json.RawMessage(`"立誓"`), SourceChapterID: "chapter-2"}),
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
		{"canon about a subject", []Patch{put(DocumentCanon, "origin", CanonFact{ID: "origin", Kind: CanonState, SubjectID: "hero", Predicate: "state.origin", Value: json.RawMessage(`"孤儿"`)})}, []string{"chapter-1"}},
		{"canon from its source chapter", []Patch{{Document: DocumentRef{Kind: DocumentCanon, ID: "vow"}, Operation: PatchDelete}}, []string{"chapter-2"}},
		{"entity", []Patch{put(DocumentEntity, "hero", Entity{ID: "hero", Kind: EntityCharacter, Name: "阿青"})}, []string{"chapter-1"}},
		{"retitle", []Patch{put(DocumentPlan, "plan-1", PlanNode{ID: "plan-1", Kind: PlanChapter, ParentID: "arc-1", Title: "山门之前", Summary: "抵达"})}, nil},
		{"new chapter plan", []Patch{put(DocumentPlan, "plan-3", PlanNode{ID: "plan-3", Kind: PlanChapter, ParentID: "arc-1", Title: "第三章", Summary: "下山"})}, nil},
		{"chapter summary", []Patch{put(DocumentPlan, "plan-2", PlanNode{ID: "plan-2", Kind: PlanChapter, ParentID: "arc-1", Title: "第二章", Summary: "拒绝拜师"})}, []string{"chapter-2"}},
		{"arc summary", []Patch{put(DocumentPlan, "arc-1", PlanNode{ID: "arc-1", Kind: PlanArc, ParentID: "volume-1", Title: "山门", Summary: "落选"})}, []string{"chapter-1", "chapter-2"}},
	} {
		if got := scope(c.patches...); !slices.Equal(got, c.want) {
			t.Errorf("%s: scope = %v, want %v", c.name, got, c.want)
		}
	}
}
