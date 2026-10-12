package workbench

import (
	"cmp"
	"slices"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
)

// CastMember 是一位出过场的人物：涉及他的已写章数与最近一章。一章涉及谁，由宿主按这一章
// 的 Canon Delta 记在章节依赖里（D35/D40），不从正文字面匹配。
type CastMember struct {
	Name     string   `json:"name"`
	Aliases  []string `json:"aliases,omitempty"`
	Chapters int      `json:"chapters"`
	Latest   int      `json:"latest"`
}

// castOf 列出出过场的人物：涉及章数多的在前，同样多时最近出场的在前。
func castOf(project projectdoc.Snapshot) []CastMember {
	members := make(map[string]*CastMember)
	for _, entity := range project.Entities {
		if entity.Kind == model.EntityCharacter {
			members[entity.ID] = &CastMember{Name: entity.Name, Aliases: entity.Aliases}
		}
	}
	for _, chapter := range project.Manuscript {
		for _, ref := range chapter.DependsOn {
			if member := members[ref.ID]; ref.Kind == model.DocumentEntity && member != nil {
				member.Chapters++
				member.Latest = max(member.Latest, chapter.Number)
			}
		}
	}
	var cast []CastMember
	for _, member := range members {
		if member.Chapters > 0 {
			cast = append(cast, *member)
		}
	}
	slices.SortFunc(cast, func(a, b CastMember) int {
		return cmp.Or(b.Chapters-a.Chapters, b.Latest-a.Latest, cmp.Compare(a.Name, b.Name))
	})
	return cast
}
