package bootstrap_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/app/chief"
	"github.com/voocel/ainovel-cli/internal/app/task"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain/change"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/litellmtest"
)

// conflictingExecutor 照脚本写书，语义影响分析一律报第一章与改动有出入（S4）。
type conflictingExecutor struct{ *scriptedQuickExecutor }

func (conflictingExecutor) Analyze(context.Context, model.Proposal, change.StructuralImpact) (json.RawMessage, error) {
	chapter := "chapter-chapter-plan-1"
	return json.Marshal(model.SemanticImpactReport{
		Status:   model.SemanticImpactConflict,
		Findings: []model.SemanticImpactFinding{{Document: &model.DocumentRef{Kind: model.DocumentManuscript, ID: chapter}, Explanation: "第一章写的是失忆的邮差"}},
		Options: []model.ResolutionOption{
			{Strategy: model.ResolutionRewriteAffected, ChapterIDs: []string{chapter}, Explanation: "按新设定重写第一章"},
			{Strategy: model.ResolutionReinterpretFuture, Explanation: "后文解释"},
			{Strategy: model.ResolutionAbandon, Explanation: "不改了"},
		},
	})
}

// finishedBook 写完两章的书：主编用 provider 回话。
func finishedBook(t *testing.T, provider litellm.Provider, analyze bool) (*testApp, *chief.Session) {
	t.Helper()
	authorityStore := openTestStore(t)
	scripted := &scriptedQuickExecutor{now: time.Now().UTC(), authorityStore: authorityStore}
	executors := task.ExecutorSet{LLM: scripted}
	if analyze {
		executors.LLM = conflictingExecutor{scripted}
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
	), false)
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

// 完本的书上选“重写受影响的章”（S4）：开新一轮，重写任务归进它并接着推进。
func TestRewritingAffectedChaptersOfAFinishedBookStartsANewRound(t *testing.T) {
	api, session := finishedBook(t, litellmtest.New(
		chiefCall("canon", "propose_change", `{"reason":"邮差其实记得一切","facts":[{"subject":"邮差","predicate":"state.memory","value":"记得一切"}]}`),
		litellmtest.Text("和第一章有出入，你选怎么处理。"),
	), true)
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
	if end.Err != nil || end.Result.RunState != model.RunWaitingUser || !strings.HasSuffix(end.Result.RunID, ":2") ||
		!strings.Contains(end.Result.RunReason, "已按改过的设定重写") {
		t.Fatalf("drive end = %+v", end)
	}
}
