package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/app/chief"
	"github.com/voocel/ainovel-cli/internal/app/task"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/litellmtest"
)

// analyzingExecutor 照脚本写书，语义影响分析按 analyze 给结论。
type analyzingExecutor struct {
	*scriptedQuickExecutor
	analyze func(model.Proposal) (model.SemanticImpactReport, error)
}

func (e analyzingExecutor) Analyze(_ context.Context, proposal model.Proposal, _ model.StructuralImpact) (json.RawMessage, error) {
	report, err := e.analyze(proposal)
	if err != nil {
		return nil, err
	}
	return json.Marshal(report)
}

// conflicting 一律报第一章与改动有出入（S4）。
func conflicting(model.Proposal) (model.SemanticImpactReport, error) {
	chapter := "chapter-chapter-plan-1"
	return model.SemanticImpactReport{
		Status:   model.SemanticImpactConflict,
		Findings: []model.SemanticImpactFinding{{Document: &model.DocumentRef{Kind: model.DocumentManuscript, ID: chapter}, Explanation: "第一章写的是失忆的邮差"}},
		Options: []model.ResolutionOption{
			{Strategy: model.ResolutionRewriteAffected, ChapterIDs: []string{chapter}, Explanation: "按新设定重写第一章"},
			{Strategy: model.ResolutionReinterpretFuture, Explanation: "后文解释"},
			{Strategy: model.ResolutionAbandon, Explanation: "不改了"},
		},
	}, nil
}

// finishedBook 写完两章的书：主编用 provider 回话，analyze 为空时不做语义影响分析。
func finishedBook(t *testing.T, provider litellm.Provider, analyze func(model.Proposal) (model.SemanticImpactReport, error)) (*testApp, *chief.Session) {
	t.Helper()
	authorityStore := openTestStore(t)
	scripted := &scriptedQuickExecutor{now: time.Now().UTC(), authorityStore: authorityStore}
	executors := task.ExecutorSet{LLM: scripted}
	if analyze != nil {
		executors.LLM = analyzingExecutor{scripted, analyze}
	}
	api := newTestApp(authorityStore, bootstrap.Options{Executors: executors, ChiefModels: scriptedModels{provider}})
	session := api.Chief.Open(context.Background(), "finished-book", "user-1")
	t.Cleanup(session.Close)
	session.Write(chief.WriteParams{Premise: "一个失忆的邮差替亡者送完最后一封信", Chapters: 2})
	requireCompleted(t, driven(t, session, 1), 2)
	return api, session
}

func chapterText(t *testing.T, api *testApp, projectID string, number int) string {
	t.Helper()
	project, err := api.Projects.Project(context.Background(), projectID, model.InitialRevision)
	if err != nil {
		t.Fatal(err)
	}
	for _, chapter := range project.Manuscript {
		if chapter.Number == number {
			return chapter.Blocks[0].Text
		}
	}
	t.Fatalf("chapter %d is not written", number)
	return ""
}

// 用户说已写的章不满意（D78）：主编把原话记成返工要求；作用域里没有写好的章时被拒。完本
// 的书开新一轮，按要求重写那一章、重审后再完成。
func TestChiefReworksAChapterOfAFinishedBook(t *testing.T) {
	api, session := finishedBook(t, litellmtest.New(
		chiefCall("early", "note_requirement", `{"text":"太拖了，紧凑点重写","scope":{"chapter":9},"rework":true}`),
		chiefCall("rework", "note_requirement", `{"text":"太拖了，紧凑点重写","scope":{"chapter":1},"rework":true}`),
		litellmtest.Text("记下了，第 1 章会重写。"),
	), nil)
	thread := talk(t, session, "第一章太拖了，紧凑点重写")
	if early := thread.Entries[1]; !early.Failed || !strings.Contains(early.Text, "还没有入稿的章") {
		t.Fatalf("rework without written chapters = %+v", early)
	}
	if receipt := lastReceipt(t, thread); receipt.Text != "记下要求 · 第 1 章 ·「太拖了，紧凑点重写」 · 已写的 1 章按它重写" {
		t.Fatalf("receipt = %+v", receipt)
	}
	end := driven(t, session, 2)
	requireCompleted(t, end, 2)
	if !strings.HasSuffix(end.Result.RunID, ":2") {
		t.Fatalf("rework of a finished book must be a new round: %s", end.Result.RunID)
	}
	if text := chapterText(t, api, "finished-book", 1); !strings.Contains(text, "按返工要求重写") {
		t.Fatalf("chapter 1 = %q", text)
	}
	if text := chapterText(t, api, "finished-book", 2); strings.Contains(text, "重写") {
		t.Fatalf("chapter 2 is outside the rework: %q", text)
	}
	snap, err := api.Workbench.WorkbenchSnapshot(context.Background(), "finished-book")
	if err != nil || len(snap.Directives) != 1 || !snap.Directives[0].Rework {
		t.Fatalf("directives = %+v, %v", snap.Directives, err)
	}
}

// 完本的书上选“重写受影响的章”（S4）：受影响的章记为返工要求（D86），与点名返工一样开新一轮，
// 只重写那一章、重审后再完成。
func TestRewritingAffectedChaptersOfAFinishedBookStartsANewRound(t *testing.T) {
	api, session := finishedBook(t, litellmtest.New(
		chiefCall("canon", "propose_change", `{"reason":"邮差其实记得一切","facts":[{"subject":"邮差","predicate":"state.memory","value":"记得一切"}]}`),
		litellmtest.Text("和第一章有出入，你选怎么处理。"),
	), conflicting)
	talk(t, session, "邮差其实记得一切")
	ctx := context.Background()
	snap, err := api.Workbench.WorkbenchSnapshot(ctx, "finished-book")
	if err != nil || snap.Change == nil || len(snap.Change.Options) != 3 {
		t.Fatalf("change = %+v, %v", snap.Change, err)
	}
	if err := session.Resolve(ctx, snap.Change.Proposal.ID, model.ResolutionRewriteAffected); err != nil {
		t.Fatal(err)
	}
	end := driven(t, session, 2)
	requireCompleted(t, end, 2)
	if !strings.HasSuffix(end.Result.RunID, ":2") {
		t.Fatalf("rewriting a finished book must be a new round: %s", end.Result.RunID)
	}
	if text := chapterText(t, api, "finished-book", 1); !strings.Contains(text, "按返工要求重写") {
		t.Fatalf("chapter 1 = %q", text)
	}
	if text := chapterText(t, api, "finished-book", 2); strings.Contains(text, "重写") {
		t.Fatalf("chapter 2 is outside the affected chapters: %q", text)
	}
}

// 改设定时大纲里点到它的也要一并改（D87）：分析指出故事弧梗概还按旧设定写，修改被退回，
// 报错用故事里的叫法；主编在同一次修改里改了梗概，修改才生效。分析器自己出错时照实说与
// 修改本身无关，不让主编当成修改哪里不对。
func TestChangeContradictingTheOutlineIsSentBack(t *testing.T) {
	arc := model.DocumentRef{Kind: model.DocumentPlan, ID: "arc-1"}
	analyzed := 0
	api, session := finishedBook(t, litellmtest.New(
		chiefCall("malformed", "propose_change", `{"reason":"邮差其实记得一切","facts":[{"subject":"邮差","predicate":"state.memory","value":"记得一切"}]}`),
		chiefCall("fact", "propose_change", `{"reason":"邮差其实记得一切","facts":[{"subject":"邮差","predicate":"state.memory","value":"记得一切"}]}`),
		chiefCall("with-arc", "propose_change", `{"reason":"邮差其实记得一切","facts":[{"subject":"邮差","predicate":"state.memory","value":"记得一切"}],"arcs":[{"arc":1,"summary":"邮差带着全部记忆寻找收信人"}]}`),
		litellmtest.Text("改好了，故事弧梗概一起改了。"),
	), func(proposal model.Proposal) (model.SemanticImpactReport, error) {
		if analyzed++; analyzed == 1 {
			// 分析器重给一次仍不合契约：模型一侧的失败，不是修改不对。
			return model.SemanticImpactReport{}, &model.Failure{Code: model.FailureModelMalformed, Err: errors.New("大纲核对漏了第 1 个故事弧")}
		}
		if slices.ContainsFunc(proposal.Patches, func(patch model.Patch) bool { return patch.Document == arc }) {
			return model.SemanticImpactReport{Status: model.SemanticImpactConsistent}, nil
		}
		return model.SemanticImpactReport{
			Status:   model.SemanticImpactConflict,
			Findings: []model.SemanticImpactFinding{{Document: &arc, Explanation: "梗概还写着邮差追索身份"}},
		}, nil
	})
	thread := talk(t, session, "邮差其实记得一切")
	if failed := thread.Entries[1]; !failed.Failed || !strings.Contains(failed.Text, "模型给出的结果不合要求，重给一次仍不合，与修改本身无关") {
		t.Fatalf("analyzer failure = %+v", failed)
	}
	if sent := thread.Entries[2]; !sent.Failed || !strings.Contains(sent.Text, "第 1 个故事弧《启程》: 梗概还写着邮差追索身份") {
		t.Fatalf("change contradicting the outline = %+v", sent)
	}
	if receipt := lastReceipt(t, thread); !strings.Contains(receipt.Text, "已生效") {
		t.Fatalf("receipt = %+v", receipt)
	}
	project, err := api.Projects.Project(context.Background(), "finished-book", model.InitialRevision)
	if err != nil {
		t.Fatal(err)
	}
	if summary := model.IndexPlan(project.Plan)["arc-1"].Summary; summary != "邮差带着全部记忆寻找收信人" {
		t.Fatalf("arc summary = %q", summary)
	}
}
