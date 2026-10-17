package change

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/voocel/ainovel-cli/internal/domain/model"
)

func chapterPlanPatch(t *testing.T, title string) model.Patch {
	t.Helper()
	return model.Patch{
		Document: model.DocumentRef{Kind: model.DocumentPlan, ID: "chapter-plan-1"}, Operation: model.PatchPut,
		Content: documentJSON(t, model.PlanNode{ID: "chapter-plan-1", Kind: model.PlanChapter, Turn: "转折", ParentID: "arc-1", Title: title, Summary: "抵达山门"}),
	}
}

func mentorPatch(t *testing.T) model.Patch {
	t.Helper()
	return model.Patch{
		Document: model.DocumentRef{Kind: model.DocumentEntity, ID: "mentor"}, Operation: model.PatchPut,
		Content: documentJSON(t, model.Entity{ID: "mentor", Kind: model.EntityCharacter, Name: "师父"}),
	}
}

// 撤销一次变更（D77）：它改过的文档恢复原样、新建的删去，基线是当前 Revision；只追加的
// 记录没有可撤销的内容；其后又被改过的文档不能单独撤销。
func TestUndoRestoresWhatTheChangeTouched(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	target := model.AuthorityTarget{Kind: model.AuthorityProject, ID: "book-1"}
	seedProject(t, ctx, s, target, false)
	engine := New(s)

	commitUserChange(t, ctx, s, target, "rename", chapterPlanPatch(t, "山门之前"))
	commitUserChange(t, ctx, s, target, "adjudication", model.Patch{
		Document: model.DocumentRef{Kind: model.DocumentAdjudication, ID: "adjudication-1"}, Operation: model.PatchPut,
		Content: documentJSON(t, model.Adjudication{ID: "adjudication-1", Reason: "接受现状", CreatedAt: testTime()}),
	})
	current := commitUserChange(t, ctx, s, target, "mentor", mentorPatch(t))

	patches, base, err := engine.Undo(ctx, "rename")
	if err != nil || base != current || len(patches) != 1 || patches[0].Operation != model.PatchPut {
		t.Fatalf("undo rename = %+v, %d, %v", patches, base, err)
	}
	var node model.PlanNode
	if err := json.Unmarshal(patches[0].Content, &node); err != nil || node.Title != "第一章" {
		t.Fatalf("restored node = %+v, %v", node, err)
	}
	if patches, _, err := engine.Undo(ctx, "mentor"); err != nil || len(patches) != 1 || patches[0].Operation != model.PatchDelete {
		t.Fatalf("undo creation = %+v, %v", patches, err)
	}
	if _, _, err := engine.Undo(ctx, "adjudication"); !errors.Is(err, model.ErrInvalid) {
		t.Fatalf("undo append-only = %v, want ErrInvalid", err)
	}
	commitUserChange(t, ctx, s, target, "rename-again", chapterPlanPatch(t, "又改了"))
	if _, _, err := engine.Undo(ctx, "rename"); !errors.Is(err, model.ErrRevisionConflict) {
		t.Fatalf("undo overwritten = %v, want ErrRevisionConflict", err)
	}
}

// 没有任务的提案（D77）：其间没碰它的文档就能搬到当前 Revision；带语义影响报告的，报告
// 判断过的文档其间变了，报告就过时、不能搬。
func TestRelocateTasklessProposal(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	target := model.AuthorityTarget{Kind: model.AuthorityProject, ID: "book-1"}
	seedProject(t, ctx, s, target, false)
	analyzer := semanticAnalyzerFunc(func(context.Context, model.Proposal, model.StructuralImpact) (json.RawMessage, error) {
		return json.Marshal(model.SemanticImpactReport{Status: model.SemanticImpactConsistent, Findings: []model.SemanticImpactFinding{}, Options: []model.ResolutionOption{}})
	})
	engine := NewWithSemanticAnalyzer(s, analyzer)
	chief := model.ChiefAuthor

	retitle := pendingChange("retitle", target, 1, model.AuthorAI, chapterPlanPatch(t, "山门之前"))
	retitle.Author = chief
	retitle, err := engine.Prepare(ctx, retitle)
	if err != nil {
		t.Fatal(err)
	}
	origin := pendingChange("origin", target, 1, model.AuthorAI, model.Patch{
		Document: model.DocumentRef{Kind: model.DocumentCanon, ID: "hero-origin"}, Operation: model.PatchPut,
		Content: documentJSON(t, model.CanonFact{
			ID: "hero-origin", Kind: model.CanonState, SubjectID: "hero", Predicate: "state.origin",
			PreviousValue: json.RawMessage(`"农家子"`), Value: json.RawMessage(`"孤儿"`),
		}),
	})
	origin.Author = chief
	origin, err = engine.PrepareWithSemantic(ctx, origin)
	if err != nil {
		t.Fatal(err)
	}

	commitUserChange(t, ctx, s, target, "mentor", mentorPatch(t))
	for _, proposal := range []model.Proposal{retitle, origin} {
		if relocated, moved, err := engine.Relocate(ctx, proposal); err != nil || !moved || relocated.BaseRevision != 2 {
			t.Fatalf("relocate %s over an unrelated change = %d, %v, %v", proposal.ID, relocated.BaseRevision, moved, err)
		}
	}

	// 第一章正文变了：改标题照样能搬（标题不在正文里），改出身的报告读过这章，过时了。
	commitUserChange(t, ctx, s, target, "rewrite-chapter-1", model.Patch{
		Document: model.DocumentRef{Kind: model.DocumentManuscript, ID: "chapter-1"}, Operation: model.PatchPut,
		Content: documentJSON(t, model.ManuscriptChapter{
			ID: "chapter-1", PlanNodeID: "chapter-plan-1", Number: 1, Author: model.AuthorUser,
			Blocks:    []model.ManuscriptBlock{{ID: "p-1", Text: "他又一次看见山门。"}},
			DependsOn: []model.DocumentRef{{Kind: model.DocumentCanon, ID: "hero-origin"}},
		}),
	})
	if _, moved, err := engine.Relocate(ctx, retitle); err != nil || !moved {
		t.Fatalf("relocate retitle = %v, %v", moved, err)
	}
	if _, _, err := engine.Relocate(ctx, origin); !errors.Is(err, model.ErrRevisionConflict) {
		t.Fatalf("relocate judged = %v, want ErrRevisionConflict", err)
	}

	commitUserChange(t, ctx, s, target, "retitle-by-user", chapterPlanPatch(t, "用户的标题"))
	if _, _, err := engine.Relocate(ctx, retitle); !errors.Is(err, model.ErrRevisionConflict) {
		t.Fatalf("relocate own document = %v, want ErrRevisionConflict", err)
	}
}
