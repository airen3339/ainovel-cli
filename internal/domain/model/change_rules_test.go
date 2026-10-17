package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// 事实核验任务只提交 canon 补丁、来源固定为任务章节、待核验事实全部处理且至少留一条事实。
func TestReviseCanonSubmissionCoversRequestedFacts(t *testing.T) {
	input := ReviseCanonInput{ChapterID: "chapter-1", FactIDs: []string{"fact-1"}, Reason: "正文被改过"}
	canon := func(id, source string) Patch {
		return Patch{Document: DocumentRef{Kind: DocumentCanon, ID: id}, Operation: PatchPut,
			Content: json.RawMessage(fmt.Sprintf(`{"id":%q,"kind":"event","subject_id":"hero","predicate":"event.done","old_value":true,"new_value":true,"source_chapter_id":%q}`, id, source))}
	}
	deleted := Patch{Document: DocumentRef{Kind: DocumentCanon, ID: "fact-1"}, Operation: PatchDelete}
	manuscript := Patch{Document: DocumentRef{Kind: DocumentManuscript, ID: "chapter-1"}, Operation: PatchPut, Content: json.RawMessage(`{}`)}
	for _, tc := range []struct {
		name    string
		patches []Patch
		wantErr string
	}{
		{"manuscript patches are rejected", []Patch{manuscript, canon("fact-1", "chapter-1")}, "only change canon facts"},
		{"pending fact must be handled", []Patch{canon("fact-2", "chapter-1")}, "pending verification"},
		{"chapter keeps at least one fact", []Patch{deleted}, "at least one canon fact"},
		{"source must be the task chapter", []Patch{canon("fact-1", "chapter-2")}, "sourced from chapter"},
		{"confirmation passes", []Patch{canon("fact-1", "chapter-1")}, ""},
		{"replacement passes", []Patch{deleted, canon("fact-2", "chapter-1")}, ""},
	} {
		projected := story{canon: make(map[string]CanonFact)}
		for _, patch := range tc.patches {
			if patch.Document.Kind == DocumentCanon && patch.Operation == PatchPut {
				if err := decodeInto(projected.canon, DocumentVersion{Document: patch.Document, Content: patch.Content}); err != nil {
					t.Fatal(err)
				}
			}
		}
		err := validateCanonRevision(input, projected, tc.patches)
		if tc.wantErr == "" && err != nil {
			t.Fatalf("%s: unexpected err %v", tc.name, err)
		}
		if tc.wantErr != "" && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

// 章节任务只写它的目标章节（写章：目标计划节点的一章；修订与返工：任务章节），不得删除
// 正文，正文满足任务携带要求的字数约束（S13）。大纲归规划任务：只有返工能改本章的章题与
// 章纲（D87），修订照章纲改正文。
func TestChapterTasksWriteExactlyTheirTargets(t *testing.T) {
	chapter := func(id, plan, text string) Patch {
		content, _ := json.Marshal(ManuscriptChapter{ID: id, PlanNodeID: plan, Number: 1, Author: AuthorAI,
			Blocks: []ManuscriptBlock{{ID: "b", Text: text}}})
		return Patch{Document: DocumentRef{Kind: DocumentManuscript, ID: id}, Operation: PatchPut, Content: content}
	}
	task := func(kind OperationKind, input TaskInput) Operation {
		raw, _ := json.Marshal(input)
		return Operation{Kind: kind, Input: raw, Snapshot: ExecutionSnapshot{BaseRevision: 3}}
	}
	length := []Directive{{ID: "length", Scope: DirectiveScopeProject, Text: "每章十字左右", Status: DirectiveActive,
		Constraints: &DirectiveConstraints{TargetWords: 10}}}
	write := task(OperationWriteChapter, &WriteChapterInput{ChapterPlanID: "plan-1", Directives: length})
	rewrite := task(OperationRewriteChapter, &RewriteChapterInput{ChapterID: "chapter-1", ChapterPlanID: "plan-1", Findings: []string{"仓促"}})
	rework := task(OperationReworkChapter, &ReworkChapterInput{ChapterID: "chapter-1", ChapterPlanID: "plan-1", Directives: []Directive{{
		ID: "rework", Scope: DirectiveScopePlanNode("plan-1"), Text: "钥匙改成邮票，重写", Status: DirectiveActive, Rework: true}}})
	ten := strings.Repeat("好", 10)
	base := story{plans: map[string]PlanNode{
		"arc-1":  {ID: "arc-1", Kind: PlanArc, Title: "雨城", Summary: "陈渡循钥匙找姐姐"},
		"arc-2":  {ID: "arc-2", Kind: PlanArc, Order: 1, Title: "邮局", Summary: "撞破秘密"},
		"plan-1": {ID: "plan-1", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 1, Title: "火漆印下的铜钥匙", Summary: "信里只有一把铜钥匙"},
		"plan-2": {ID: "plan-2", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 2, Title: "死信房", Summary: "按钥匙编号找柜格"},
	}}
	outline := func(id string, edit func(*PlanNode)) Patch {
		node := base.plans[id]
		edit(&node)
		content, _ := json.Marshal(node)
		return Patch{Document: DocumentRef{Kind: DocumentPlan, ID: id}, Operation: PatchPut, Content: content}
	}
	retitled := outline("plan-1", func(node *PlanNode) {
		node.Title, node.Summary, node.Turn = "火漆印下的旧邮票", "信里只有一枚旧邮票", "邮票背面写着姐姐的名字"
	})
	for _, c := range []struct {
		name    string
		task    Operation
		patches []Patch
		wantErr string
	}{
		{"write implements its plan", write, []Patch{chapter("chapter-1", "plan-1", ten)}, ""},
		{"write another plan", write, []Patch{chapter("chapter-1", "plan-2", ten)}, "does not implement the requested plan"},
		{"write nothing", write, nil, "exactly one new chapter"},
		{"write two chapters", write, []Patch{chapter("chapter-1", "plan-1", ten), chapter("chapter-2", "plan-1", ten)}, "exactly one new chapter"},
		{"write too short", write, []Patch{chapter("chapter-1", "plan-1", "太短")}, "needs 9-11"},
		{"rewrite its chapter", rewrite, []Patch{chapter("chapter-1", "plan-1", "新")}, ""},
		{"rewrite another chapter", rewrite, []Patch{chapter("chapter-2", "plan-1", "新")}, "exactly chapter chapter-1"},
		{"rewrite cannot delete", rewrite, []Patch{chapter("chapter-1", "plan-1", "新"),
			{Document: DocumentRef{Kind: DocumentManuscript, ID: "chapter-2"}, Operation: PatchDelete}}, "cannot delete chapter"},
		{"rework retitles its chapter", rework, []Patch{chapter("chapter-1", "plan-1", "新"), retitled}, ""},
		{"rework cannot edit another outline", rework, []Patch{chapter("chapter-1", "plan-1", "新"), retitled,
			outline("plan-2", func(node *PlanNode) { node.Summary = "按邮票编号找柜格" })}, "only planning tasks edit the plan"},
		{"rework cannot edit its arc", rework, []Patch{chapter("chapter-1", "plan-1", "新"),
			outline("arc-1", func(node *PlanNode) { node.Summary = "陈渡循邮票找姐姐" })}, "only planning tasks edit the plan"},
		{"rework cannot move its chapter", rework, []Patch{chapter("chapter-1", "plan-1", "新"),
			outline("plan-1", func(node *PlanNode) { node.ParentID = "arc-2" })}, "only planning tasks edit the plan"},
		{"rework cannot change its threads", rework, []Patch{chapter("chapter-1", "plan-1", "新"),
			outline("plan-1", func(node *PlanNode) { node.Threads = []string{"t1"} })}, "only planning tasks edit the plan"},
		{"rewrite cannot edit its outline", rewrite, []Patch{chapter("chapter-1", "plan-1", "新"), retitled}, "only planning tasks edit the plan"},
		{"write cannot edit its outline", write, []Patch{chapter("chapter-1", "plan-1", ten), retitled}, "only planning tasks edit the plan"},
	} {
		t.Run(c.name, func(t *testing.T) {
			projected := story{chapters: make(map[string]ManuscriptChapter), plans: make(map[string]PlanNode)}
			for id, node := range base.plans {
				projected.plans[id] = node
			}
			for _, patch := range c.patches {
				version := DocumentVersion{Document: patch.Document, Content: patch.Content}
				var err error
				switch {
				case patch.Operation != PatchPut:
				case patch.Document.Kind == DocumentPlan:
					err = decodeInto(projected.plans, version)
				default:
					err = decodeInto(projected.chapters, version)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			err := validateTaskSubmission(c.task, base, projected, c.patches)
			if c.wantErr == "" && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if c.wantErr != "" && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}

// 规划要立起长线（D91）；已写章的大纲描述它的正文，规划不改；在已写章作答的长线，答案就是那
// 一章。未写的章与未写的答案随规划调整。
func TestPlanningKeepsWrittenChaptersAndTheirAnswers(t *testing.T) {
	raw, _ := json.Marshal(&RevisePlanInput{ExistingChapters: 2})
	planning := Operation{Kind: OperationRevisePlan, Input: raw, Snapshot: ExecutionSnapshot{BaseRevision: 3}}
	base := story{
		plans: map[string]PlanNode{
			"arc-1":  {ID: "arc-1", Kind: PlanArc, Title: "雨城", Summary: "陈渡循钥匙找姐姐"},
			"plan-1": {ID: "plan-1", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 1, Title: "铜钥匙", Summary: "信里只有一把钥匙", Threads: []string{"t1"}},
			"plan-2": {ID: "plan-2", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 2, Title: "死信房", Summary: "按编号找柜格", Threads: []string{"t2"}},
		},
		chapters: map[string]ManuscriptChapter{"chapter-1": {ID: "chapter-1", PlanNodeID: "plan-1", Number: 1}},
		compass: &Compass{ScaleMax: 50, Ending: "找到姐姐", Threads: []StoryThread{
			{ID: "t1", Name: "钥匙", Question: "钥匙开哪扇门", Answer: "plan-1"},
			{ID: "t2", Name: "姐姐", Question: "姐姐为何失踪", Answer: "plan-2"},
		}},
	}
	appended := PlanNode{ID: "plan-3", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 3, Title: "柜格", Summary: "柜格里是旧信", Threads: []string{"t2"}}
	answers := func(t1, t2 string) Compass {
		compass := *base.compass
		compass.Threads = []StoryThread{base.compass.Threads[0], base.compass.Threads[1]}
		compass.Threads[0].Answer, compass.Threads[1].Answer = t1, t2
		return compass
	}
	edited := func(id string, edit func(*PlanNode)) PlanNode {
		node := base.plans[id]
		edit(&node)
		return node
	}
	for _, c := range []struct {
		name    string
		compass Compass
		plans   []PlanNode
		wantErr string
	}{
		{"unwritten answer moves", answers("plan-1", "plan-3"), nil, ""},
		{"unwritten chapter is revised", answers("plan-1", "plan-2"),
			[]PlanNode{edited("plan-2", func(node *PlanNode) { node.Summary = "柜格是空的" })}, ""},
		{"written answer stays", answers("plan-3", "plan-2"), nil, "stays its answer"},
		{"written chapter is not revised", answers("plan-1", "plan-2"),
			[]PlanNode{edited("plan-1", func(node *PlanNode) { node.Summary = "信里只有一枚邮票" })}, "planning may not edit chapter"},
		{"written chapter keeps its threads", answers("plan-1", "plan-2"),
			[]PlanNode{edited("plan-1", func(node *PlanNode) { node.Threads = nil })}, "planning may not edit chapter"},
		{"written chapter keeps its turn", answers("plan-1", "plan-2"),
			[]PlanNode{edited("plan-1", func(node *PlanNode) { node.Turn = "钥匙是假的" })}, "planning may not edit chapter"},
		{"blueprint needs threads", Compass{ScaleMax: 50, Ending: "找到姐姐"}, nil, "requires compass threads"},
	} {
		t.Run(c.name, func(t *testing.T) {
			projected := story{plans: make(map[string]PlanNode), chapters: base.chapters, compass: &c.compass}
			for id, node := range base.plans {
				projected.plans[id] = node
			}
			content, _ := json.Marshal(c.compass)
			patches := []Patch{{Document: DocumentRef{Kind: DocumentCompass, ID: SingletonDocumentID}, Operation: PatchPut, Content: content}}
			for _, node := range append(c.plans, appended) {
				projected.plans[node.ID] = node
				content, _ := json.Marshal(node)
				patches = append(patches, Patch{Document: DocumentRef{Kind: DocumentPlan, ID: node.ID}, Operation: PatchPut, Content: content})
			}
			err := validateTaskSubmission(planning, base, projected, patches)
			if c.wantErr == "" && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if c.wantErr != "" && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
		})
	}
}

// 长线的引用是蓝图结构的一部分：章推进的长线必须在罗盘里，答案必须是章节计划。
func TestThreadReferencesFollowTheCompass(t *testing.T) {
	plans := map[string]PlanNode{
		"arc-1":  {ID: "arc-1", Kind: PlanArc, Title: "雨城", Summary: "找姐姐"},
		"plan-1": {ID: "plan-1", Kind: PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 1, Title: "铜钥匙", Summary: "钥匙", Threads: []string{"t1"}},
	}
	thread := func(answer string) *Compass {
		return &Compass{Ending: "找到姐姐", Threads: []StoryThread{{ID: "t1", Name: "钥匙", Question: "钥匙开哪扇门", Answer: answer}}}
	}
	for name, c := range map[string]struct {
		compass *Compass
		wantErr string
	}{
		"answered in a chapter": {thread("plan-1"), ""},
		"open":                  {thread(""), ""},
		"no compass":            {nil, "there is no compass"},
		"missing thread":        {&Compass{Ending: "找到姐姐"}, "missing thread"},
		"answered in an arc":    {thread("arc-1"), "missing chapter plan"},
		"answered nowhere":      {thread("plan-9"), "missing chapter plan"},
	} {
		err := story{plans: plans, compass: c.compass}.validateThreadRefs()
		if c.wantErr == "" && err != nil {
			t.Fatalf("%s: rejected: %v", name, err)
		}
		if c.wantErr != "" && (!errors.Is(err, ErrStructuralConflict) || !strings.Contains(err.Error(), c.wantErr)) {
			t.Fatalf("%s: err = %v, want %q", name, err, c.wantErr)
		}
	}
}

// 回滚、撤销与导入共用的状态差：Canon 的 old_value 按续写规则重写——恢复一条已删除的
// 事实是新建，不带它曾经的 old_value；只差 old_value 不算变化。只追加的记录不参与比较。
func TestStatePatchesFollowCanonContinuity(t *testing.T) {
	canon := func(value, previous string) DocumentVersion {
		fact := CanonFact{ID: "fact-1", Kind: CanonState, SubjectID: "hero", Predicate: "位置", Value: json.RawMessage(value)}
		if previous != "" {
			fact.PreviousValue = json.RawMessage(previous)
		}
		content, err := json.Marshal(fact)
		if err != nil {
			t.Fatal(err)
		}
		return DocumentVersion{Document: DocumentRef{Kind: DocumentCanon, ID: fact.ID}, Content: content}
	}
	decode := func(patch Patch) CanonFact {
		var fact CanonFact
		if err := json.Unmarshal(patch.Content, &fact); err != nil {
			t.Fatal(err)
		}
		return fact
	}
	key := DocumentRef{Kind: DocumentCanon, ID: "fact-1"}.Key()
	adjudication := DocumentVersion{Document: DocumentRef{Kind: DocumentAdjudication, ID: "a-1"}, Content: json.RawMessage(`{}`)}

	restored, err := StatePatches(DocumentSet{adjudication.Document.Key(): adjudication}, DocumentSet{key: canon(`"京城"`, `"边关"`)})
	if err != nil || len(restored) != 1 || restored[0].Operation != PatchPut || decode(restored[0]).PreviousValue != nil {
		t.Fatalf("restoring a deleted fact = %+v, %v", restored, err)
	}
	if same, err := StatePatches(DocumentSet{key: canon(`"京城"`, "")}, DocumentSet{key: canon(`"京城"`, `"边关"`)}); err != nil || len(same) != 0 {
		t.Fatalf("an old_value-only difference = %+v, %v", same, err)
	}
	updated, err := StatePatches(DocumentSet{key: canon(`"京城"`, `"边关"`)}, DocumentSet{key: canon(`"边关"`, "")})
	if err != nil || len(updated) != 1 || string(decode(updated[0]).PreviousValue) != `"京城"` {
		t.Fatalf("an update = %+v, %v", updated, err)
	}
	if deleted, err := StatePatches(DocumentSet{key: canon(`"京城"`, "")}, DocumentSet{}); err != nil || len(deleted) != 1 || deleted[0].Operation != PatchDelete {
		t.Fatalf("a delete = %+v, %v", deleted, err)
	}
}
