package workbench

import (
	"slices"
	"testing"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// 人物按涉及的已写章数排序，同样多时最近出场的在前；地点等不是人物，没出过场的不列。
func TestCastRanksCharactersByChaptersTheyAppearIn(t *testing.T) {
	ref := func(id string) model.DocumentRef { return model.DocumentRef{Kind: model.DocumentEntity, ID: id} }
	project := projectdoc.Snapshot{
		Entities: []model.Entity{
			{ID: "hero", Kind: model.EntityCharacter, Name: "沈砚", Aliases: []string{"漏根"}},
			{ID: "sister", Kind: model.EntityCharacter, Name: "沈穗"},
			{ID: "mentor", Kind: model.EntityCharacter, Name: "老周头"},
			{ID: "town", Kind: model.EntityLocation, Name: "青石镇"},
			{ID: "absent", Kind: model.EntityCharacter, Name: "未登场"},
		},
		Manuscript: []model.ManuscriptChapter{
			{ID: "c1", Number: 1, DependsOn: []model.DocumentRef{ref("hero"), ref("sister"), ref("town")}},
			{ID: "c2", Number: 2, DependsOn: []model.DocumentRef{ref("hero"), ref("mentor")}},
			{ID: "c3", Number: 3, DependsOn: []model.DocumentRef{ref("hero")}},
		},
	}
	got := castOf(project)
	want := []CastMember{
		{Name: "沈砚", Aliases: []string{"漏根"}, Chapters: 3, Latest: 3},
		{Name: "老周头", Chapters: 1, Latest: 2},
		{Name: "沈穗", Chapters: 1, Latest: 1},
	}
	if !slices.EqualFunc(got, want, func(a, b CastMember) bool {
		return a.Name == b.Name && a.Chapters == b.Chapters && a.Latest == b.Latest && slices.Equal(a.Aliases, b.Aliases)
	}) {
		t.Fatalf("cast = %+v\nwant %+v", got, want)
	}
}
