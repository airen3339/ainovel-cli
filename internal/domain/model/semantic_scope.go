package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// SemanticScope 是一次修改可能与之矛盾的已写章节（D77）：语义影响分析要读的正文，也是
// 判断分析是否过时的范围。设定改动牵连写到该主体的章及它的来源章、生效章；人物地点
// 改动牵连写到它的章；大纲改动只在梗概或归属变了（或删除）时牵连该节点下已写的章——
// 标题不在正文里。
func SemanticScope(patches []Patch, base DocumentSet) ([]DocumentRef, error) {
	var chapters []ManuscriptChapter
	plans := make(PlanIndex)
	for _, document := range base {
		switch document.Document.Kind {
		case DocumentManuscript:
			var chapter ManuscriptChapter
			if err := json.Unmarshal(document.Content, &chapter); err != nil {
				return nil, fmt.Errorf("decode chapter %q: %w", document.Document.ID, err)
			}
			chapters = append(chapters, chapter)
		case DocumentPlan:
			var node PlanNode
			if err := json.Unmarshal(document.Content, &node); err != nil {
				return nil, fmt.Errorf("decode plan node %q: %w", document.Document.ID, err)
			}
			plans[node.ID] = node
		}
	}
	scope := make(map[string]DocumentRef)
	include := func(match func(ManuscriptChapter) bool) {
		for _, chapter := range chapters {
			if match(chapter) {
				ref := DocumentRef{Kind: DocumentManuscript, ID: chapter.ID}
				scope[ref.Key()] = ref
			}
		}
	}
	mentioning := func(entityID string) func(ManuscriptChapter) bool {
		entity := DocumentRef{Kind: DocumentEntity, ID: entityID}
		return func(chapter ManuscriptChapter) bool { return slices.Contains(chapter.DependsOn, entity) }
	}
	for _, patch := range patches {
		switch patch.Document.Kind {
		case DocumentCanon:
			versions := []json.RawMessage{patch.Content}
			if document, ok := base[patch.Document.Key()]; ok {
				versions = append(versions, document.Content)
			}
			for _, content := range versions {
				if content == nil {
					continue
				}
				var fact CanonFact
				if err := json.Unmarshal(content, &fact); err != nil {
					return nil, fmt.Errorf("decode fact %q: %w", patch.Document.ID, err)
				}
				include(mentioning(fact.SubjectID))
				include(func(chapter ManuscriptChapter) bool {
					return chapter.ID == fact.SourceChapterID || chapter.ID == fact.EffectiveChapterID
				})
			}
		case DocumentEntity:
			include(mentioning(patch.Document.ID))
		case DocumentPlan:
			before, existed := plans[patch.Document.ID]
			if !existed {
				continue
			}
			if patch.Operation == PatchPut {
				var node PlanNode
				if err := json.Unmarshal(patch.Content, &node); err != nil {
					return nil, fmt.Errorf("decode plan node %q: %w", patch.Document.ID, err)
				}
				if node.Summary == before.Summary && node.ParentID == before.ParentID {
					continue
				}
			}
			include(func(chapter ManuscriptChapter) bool {
				return slices.Contains(plans.Ancestry(chapter.PlanNodeID), patch.Document.ID)
			})
		}
	}
	refs := make([]DocumentRef, 0, len(scope))
	for _, ref := range scope {
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b DocumentRef) int { return strings.Compare(a.Key(), b.Key()) })
	return refs, nil
}
