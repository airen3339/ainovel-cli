package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// SemanticScope 是一次修改可能与之矛盾的内容：语义影响分析要读的，也是判断分析是否过时
// 的范围。
//   - 已写的章（D77）：设定改动牵连写到该主体的章及它的来源章、生效章；人物地点改动牵连
//     写到它的章；大纲改动只在梗概或归属变了（或删除）时牵连该节点下已写的章——标题不在
//     正文里。
//   - 点到被改主体的规划（D87）：没写的章纲与卷、故事弧的梗概，规划时定下的事实（没有来源
//     章；关于被改主体的，或值里点到它的），作品前提。与修改矛盾的要在同一次修改里一并改。
//     已写章的章题、章纲与它的事实描述的是它的正文，随正文走，不在其中。
func SemanticScope(patches []Patch, base DocumentSet) ([]DocumentRef, error) {
	var chapters []ManuscriptChapter
	var facts []CanonFact
	var intent Intent
	plans := make(PlanIndex)
	entities := make(map[string]Entity)
	for _, document := range base {
		var err error
		switch document.Document.Kind {
		case DocumentManuscript:
			var chapter ManuscriptChapter
			err = json.Unmarshal(document.Content, &chapter)
			chapters = append(chapters, chapter)
		case DocumentPlan:
			err = decodeInto(plans, document)
		case DocumentEntity:
			err = decodeInto(entities, document)
		case DocumentCanon:
			var fact CanonFact
			err = json.Unmarshal(document.Content, &fact)
			facts = append(facts, fact)
		case DocumentIntent:
			err = json.Unmarshal(document.Content, &intent)
		}
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", document.Document.Key(), err)
		}
	}
	// subjects 是被改的主体，named 是要在规划里找的它们：改前的样子与改后的（改名时两个都找）。
	subjects := make(map[string]bool)
	var named []Entity
	scope := make(map[string]DocumentRef)
	add := func(ref DocumentRef) { scope[ref.Key()] = ref }
	include := func(match func(ManuscriptChapter) bool) {
		for _, chapter := range chapters {
			if match(chapter) {
				add(DocumentRef{Kind: DocumentManuscript, ID: chapter.ID})
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
				subjects[fact.SubjectID] = true
				include(mentioning(fact.SubjectID))
				include(func(chapter ManuscriptChapter) bool {
					return chapter.ID == fact.SourceChapterID || chapter.ID == fact.EffectiveChapterID
				})
			}
		case DocumentEntity:
			subjects[patch.Document.ID] = true
			include(mentioning(patch.Document.ID))
			if patch.Operation == PatchPut {
				var entity Entity
				if err := json.Unmarshal(patch.Content, &entity); err != nil {
					return nil, fmt.Errorf("decode entity %q: %w", patch.Document.ID, err)
				}
				named = append(named, entity)
			}
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
	for id := range subjects {
		if entity, ok := entities[id]; ok {
			named = append(named, entity)
		}
	}
	mentioned := func(text string) bool {
		return slices.ContainsFunc(named, func(entity Entity) bool { return entity.MentionedIn(text) })
	}
	written := make(map[string]bool, len(chapters))
	for _, chapter := range chapters {
		written[chapter.PlanNodeID] = true
	}
	for _, node := range plans {
		if !written[node.ID] && mentioned(node.Title+"\n"+node.Summary) {
			add(DocumentRef{Kind: DocumentPlan, ID: node.ID})
		}
	}
	for _, fact := range facts {
		if fact.SourceChapterID == "" && (subjects[fact.SubjectID] || mentioned(string(fact.Value))) {
			add(DocumentRef{Kind: DocumentCanon, ID: fact.ID})
		}
	}
	if mentioned(intent.Premise) {
		add(DocumentRef{Kind: DocumentIntent, ID: SingletonDocumentID})
	}
	refs := make([]DocumentRef, 0, len(scope))
	for _, ref := range scope {
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b DocumentRef) int { return strings.Compare(a.Key(), b.Key()) })
	return refs, nil
}
