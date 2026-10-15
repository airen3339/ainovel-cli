package bootstrap_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voocel/agentcore"
	"github.com/voocel/ainovel-cli/internal/app/chief"
	novelapp "github.com/voocel/ainovel-cli/internal/app/novel"
	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/app/task"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/llm/models"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/litellmtest"
)

// scriptedModels 让主编用脚本化的模型回话。
type scriptedModels struct{ provider litellm.Provider }

func (m scriptedModels) Binding(string) (models.Binding, bool) {
	client, err := litellm.New(m.provider)
	if err != nil {
		panic(err)
	}
	chat := agentcore.Model{Client: client, Request: litellm.Request{Model: "model"}}
	return models.Binding{Provider: "test", Model: "model", Digest: "model", Chat: chat}, true
}

func chiefCall(id, name, args string) litellmtest.Reply {
	return litellmtest.Respond(litellm.ToolUseBlock{ID: id, Name: name, Arguments: args})
}

func newChiefApp(t *testing.T, replies ...litellmtest.Reply) (*testApp, *litellmtest.Provider) {
	t.Helper()
	provider := litellmtest.New(replies...)
	return chiefApp(t, provider), provider
}

// chiefApp 是已有作品 chief-book、主编用 provider 回话的应用。
func chiefApp(t *testing.T, provider litellm.Provider) *testApp {
	t.Helper()
	api := newTestApp(openTestStore(t), bootstrap.Options{ChiefModels: scriptedModels{provider}})
	if _, err := api.Projects.CreateProject(context.Background(), projectdoc.CreateProjectCommand{
		ProjectID: "chief-book", ChangeID: "create", UserID: "user-1", Reason: "建立作品",
		Draft: testProjectDraft(), CreatedAt: testTime(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return api
}

// settle 等主编这一回合说完。
func settle(t *testing.T, session *chief.Session) chief.Thread {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if thread := session.Thread(); !thread.Busy {
			return thread
		}
		select {
		case <-session.Changes():
		case <-deadline:
			t.Fatal("chief turn did not end")
		}
	}
}

// 用户说一句话，主编按原话记下要求：作用域照用户说的章号，不按用户正在看的章；来源指回
// 那句话；模型把全角标点摘成半角也认，入账的是用户的写法；对话里留下回执，重开这本书时
// 对话还在。
func TestChiefNotesARequirementQuotingTheUser(t *testing.T) {
	api, provider := newChiefApp(t,
		chiefCall("note", "note_requirement", `{"text":"第二章结尾,留个钩子","scope":{"chapter":2}}`),
		litellmtest.Text("记下了，第 2 章结尾会留钩子。"),
	)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()
	if err := session.Send(ctx, "第二章结尾，留个钩子吧", chief.View{Chapter: 21}); err != nil {
		t.Fatal(err)
	}
	thread := settle(t, session)
	if thread.Err != "" || len(thread.Entries) != 3 {
		t.Fatalf("thread = %+v", thread)
	}
	action := thread.Entries[1]
	if action.Kind != chief.EntryAction || !action.Done || action.Failed || !action.Change || action.Text != "记下要求 · 第 2 章 ·「第二章结尾，留个钩子」" {
		t.Fatalf("receipt = %+v", action)
	}
	if thread.Entries[0].Text != "第二章结尾，留个钩子吧" || thread.Entries[2].Text != "记下了，第 2 章结尾会留钩子。" {
		t.Fatalf("entries = %+v", thread.Entries)
	}
	snap, err := api.Workbench.WorkbenchSnapshot(ctx, "chief-book")
	if err != nil || len(snap.Directives) != 1 {
		t.Fatalf("directives = %+v, %v", snap.Directives, err)
	}
	if directive := snap.Directives[0]; directive.Scope != model.DirectiveScopeChapters(2, 2) || directive.Text != "第二章结尾，留个钩子" || directive.Source != model.ConversationSource(1) {
		t.Fatalf("directive = %+v", directive)
	}
	// 书况在用户的话之前，告诉主编用户正在看哪一章。
	first := provider.Requests()[0]
	brief := first.Messages[len(first.Messages)-1].Blocks[0].(litellm.TextBlock).Text
	if !strings.Contains(brief, "【书况】") || !strings.Contains(brief, "用户正在看：第 21 章") {
		t.Fatalf("brief = %q", brief)
	}

	// 对话只载一次：会话建立之后再载不重复。
	if err := session.Load(ctx); err != nil || len(session.Thread().Entries) != 3 {
		t.Fatalf("load after talk = %+v, %v", session.Thread().Entries, err)
	}
	reopened := api.Chief.Open(ctx, "chief-book", "user-1")
	defer reopened.Close()
	if err := reopened.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if again := reopened.Thread(); len(again.Entries) != 3 || again.Entries[1].Text != action.Text {
		t.Fatalf("reloaded = %+v", again.Entries)
	}
}

// 以用户名义写入的文字必须逐字出自用户的话：改写过的原话被拒，作品不变。
func TestChiefCannotPutWordsInTheUsersMouth(t *testing.T) {
	api, _ := newChiefApp(t,
		chiefCall("note", "note_requirement", `{"text":"主角必须惨败"}`),
		litellmtest.Text("抱歉，我会照你的原话记。"),
	)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()
	if err := session.Send(ctx, "让主角输一次", chief.View{}); err != nil {
		t.Fatal(err)
	}
	thread := settle(t, session)
	if action := thread.Entries[1]; !action.Failed || !strings.Contains(action.Text, "不是用户说过的原话") {
		t.Fatalf("action = %+v", action)
	}
	if snap, err := api.Workbench.WorkbenchSnapshot(ctx, "chief-book"); err != nil || len(snap.Directives) != 0 {
		t.Fatalf("directives = %+v, %v", snap.Directives, err)
	}
}

// 第二回合接着第一回合的对话：模型看到的历史里有上一回合的话与工具结果。
func TestChiefRemembersTheConversation(t *testing.T) {
	api, provider := newChiefApp(t,
		chiefCall("outline", "book_outline", `{}`),
		litellmtest.Text("还没有大纲。"),
		litellmtest.Text("好的。"),
	)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()
	if err := session.Send(ctx, "现在写到哪了", chief.View{}); err != nil {
		t.Fatal(err)
	}
	settle(t, session)
	if err := session.Send(ctx, "知道了", chief.View{}); err != nil {
		t.Fatal(err)
	}
	thread := settle(t, session)
	if thread.Err != "" || len(thread.Entries) != 5 {
		t.Fatalf("thread = %+v", thread)
	}
	last := provider.Requests()[2].Messages
	var tools int
	for _, message := range last {
		for _, block := range message.Blocks {
			if result, ok := block.(litellm.ToolResultBlock); ok && result.ToolUseID == "outline" {
				tools++
				// 名册让主编知道书里已有哪些人物与物品：改名才不会另建一个（D87）。
				var outline struct {
					Intent   model.Intent `json:"intent"`
					Entities []struct {
						Name string `json:"name"`
					} `json:"entities"`
				}
				text := agentcore.Message{Blocks: result.Content}.Text()
				if err := json.Unmarshal([]byte(text), &outline); err != nil || outline.Intent.Premise == "" || len(outline.Entities) == 0 {
					t.Fatalf("outline result = %s, %v", text, err)
				}
			}
		}
	}
	if tools != 1 {
		t.Fatalf("history lost the first turn: %+v", last)
	}
}

// stallingExecutor 第一次执行停在模型调用里，直到推进被打断；之后照脚本执行。
type stallingExecutor struct {
	*scriptedQuickExecutor
	started chan struct{}
	stalled bool
}

func (e *stallingExecutor) Execute(ctx context.Context, operation model.Operation) (model.OperationOutcome, error) {
	if !e.stalled {
		e.stalled = true
		close(e.started)
		<-ctx.Done()
		return model.OperationOutcome{}, ctx.Err()
	}
	return e.scriptedQuickExecutor.Execute(ctx, operation)
}

// pausedMidChapter 开写两章的书，在第一次模型调用里按键暂停：推进停下、作品落盘为暂停。
func pausedMidChapter(t *testing.T, provider litellm.Provider) (*testApp, *chief.Session) {
	t.Helper()
	authorityStore := openTestStore(t)
	executor := &stallingExecutor{
		scriptedQuickExecutor: &scriptedQuickExecutor{now: time.Now().UTC(), authorityStore: authorityStore},
		started:               make(chan struct{}),
	}
	api := newTestApp(authorityStore, bootstrap.Options{
		Executors: task.ExecutorSet{LLM: executor}, ChiefModels: scriptedModels{provider},
	})
	ctx := context.Background()
	session := api.Chief.Open(ctx, "paused-book", "user-1")
	t.Cleanup(session.Close)
	if !session.Write(chief.WriteParams{Premise: "一个失忆的邮差替亡者送完最后一封信", Chapters: 2}) {
		t.Fatal("write did not start")
	}
	<-executor.started
	if err := session.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if production := session.Production(); production.Writing || production.Ended != 1 {
		t.Fatalf("pause must stop the drive: %+v", production)
	}
	return api, session
}

// driven 等第 n 次推进结束，返回它的落点。
func driven(t *testing.T, session *chief.Session, n int) chief.DriveEnd {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if production := session.Production(); production.Ended >= n && !production.Writing {
			return production.Last
		}
		select {
		case <-session.Changes():
		case <-deadline:
			t.Fatal("drive did not end")
		}
	}
}

func talk(t *testing.T, session *chief.Session, words string) chief.Thread {
	t.Helper()
	if err := session.Send(context.Background(), words, chief.View{}); err != nil {
		t.Fatal(err)
	}
	thread := settle(t, session)
	if thread.Err != "" {
		t.Fatalf("turn failed: %s", thread.Err)
	}
	return thread
}

func requireCompleted(t *testing.T, end chief.DriveEnd, chapters int) {
	t.Helper()
	if end.Err != nil || end.Result.RunState != model.RunCompleted || len(end.Result.Chapters) != chapters {
		t.Fatalf("drive end = %+v", end)
	}
}

// 按键暂停后跟主编说完一句话，回合结束就从停下的地方接着写。
func TestTalkingAfterAPauseResumesWriting(t *testing.T) {
	provider := litellmtest.New(litellmtest.Text("在写第一章。"))
	_, session := pausedMidChapter(t, provider)
	talk(t, session, "现在写到哪了")
	// 书况照实说回合结束后接着写：主编以为书停着，就会先问用户、不先暂停（D87）。
	if brief := fmt.Sprint(provider.Requests()[0].Messages); !strings.Contains(brief, "创作：用户按键暂停，你这一回合结束后接着写") {
		t.Fatalf("brief = %s", brief)
	}
	requireCompleted(t, driven(t, session, 2), 2)
}

// 写到一半改了作品前提：确认后照新前提接着写。前提只在创作意图里，运行目标不留旧抄本（D87）。
func TestWritingContinuesAfterThePremiseChanges(t *testing.T) {
	provider := litellmtest.New(
		chiefCall("premise", "propose_change", `{"reason":"送信的人换成邮差的姐姐","intent":{"premise":"邮差的姐姐替亡者送完最后一封信"}}`),
		litellmtest.Text("等你确认。"),
	)
	api, session := pausedMidChapter(t, provider)
	talk(t, session, "送信的其实是邮差的姐姐")
	ctx := context.Background()
	pending, ok, err := api.Decisions.PendingChange(ctx, "paused-book")
	if err != nil || !ok {
		t.Fatalf("pending = %v, %v", ok, err)
	}
	if held := driven(t, session, 2); held.Err != nil || held.Result.RunState != model.RunWaitingUser {
		t.Fatalf("a pending change must hold the drive: %+v", held)
	}
	if err := session.Confirm(ctx, pending.ID); err != nil {
		t.Fatal(err)
	}
	requireCompleted(t, driven(t, session, 3), 2)
}

// 用户让主编停下：之后的话不再带着续写，直到用户让它继续。
func TestChiefPauseHoldsUntilAskedToResume(t *testing.T) {
	provider := litellmtest.New(
		chiefCall("pause", "pause_creation", `{}`), litellmtest.Text("好，先停着。"),
		litellmtest.Text("好的。"),
		chiefCall("resume", "resume_creation", `{}`), litellmtest.Text("继续写了。"),
	)
	_, session := pausedMidChapter(t, provider)
	talk(t, session, "先停一下，我想想")
	talk(t, session, "第二章多点雨声")
	if production := session.Production(); production.Writing || production.Ended != 1 {
		t.Fatalf("a chief pause must hold across turns: %+v", production)
	}
	if brief := fmt.Sprint(provider.Requests()[2].Messages); !strings.Contains(brief, "创作：已暂停") {
		t.Fatalf("brief after a chief pause = %s", brief)
	}
	thread := talk(t, session, "继续吧")
	if receipt := thread.Entries[len(thread.Entries)-2]; !receipt.Change || receipt.Text != "继续创作" {
		t.Fatalf("receipt = %+v", receipt)
	}
	requireCompleted(t, driven(t, session, 2), 2)
}

// 主编改篇幅后按新篇幅接着写；下一句话的书况带上这期间的里程碑（D77）。
func TestChiefSetsTheLengthAndKeepsWriting(t *testing.T) {
	provider := litellmtest.New(
		chiefCall("length", "set_length", `{"chapters":3}`), litellmtest.Text("改成三章了。"),
		litellmtest.Text("写完了。"),
	)
	_, session := pausedMidChapter(t, provider)
	thread := talk(t, session, "写三章吧")
	if receipt := thread.Entries[1]; receipt.Failed || receipt.Text != "篇幅定为 3 章 · 继续创作" {
		t.Fatalf("receipt = %+v", receipt)
	}
	requireCompleted(t, driven(t, session, 2), 3)
	talk(t, session, "写得怎么样了")
	requests := provider.Requests()
	last := requests[len(requests)-1]
	brief := last.Messages[len(last.Messages)-1].Blocks[0].(litellm.TextBlock).Text
	if !strings.Contains(brief, "上一回合以来：") || !strings.Contains(brief, "第 3 章《") || !strings.Contains(brief, "全书 3 章完成") {
		t.Fatalf("brief = %q", brief)
	}
}

// gatedProvider 让第一次模型调用等到放行：测试借此在主编回话中途再说一句。
type gatedProvider struct {
	*litellmtest.Provider
	asked, release chan struct{}
	once           sync.Once
}

func (p *gatedProvider) Stream(ctx context.Context, request *litellm.Request) (litellm.Stream, error) {
	p.once.Do(func() {
		close(p.asked)
		<-p.release
	})
	return p.Provider.Stream(ctx, request)
}

// 主编回话时再说的话进同一个回合：两句都有回答，按说的先后排。
func TestWordsSentMidAnswerJoinTheTurn(t *testing.T) {
	gated := &gatedProvider{
		Provider: litellmtest.New(litellmtest.Text("先答第一句。"), litellmtest.Text("再答第二句。")),
		asked:    make(chan struct{}), release: make(chan struct{}),
	}
	api := chiefApp(t, gated)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()
	if err := session.Send(ctx, "第一句", chief.View{}); err != nil {
		t.Fatal(err)
	}
	<-gated.asked
	if err := session.Send(ctx, "第二句", chief.View{}); err != nil {
		t.Fatal(err)
	}
	if thread := session.Thread(); !thread.Busy || len(thread.Pending) != 1 || thread.Pending[0] != "第二句" {
		t.Fatalf("mid-answer thread = %+v", thread)
	}
	close(gated.release)
	thread := settle(t, session)
	var said []string
	for _, entry := range thread.Entries {
		said = append(said, entry.Text)
	}
	if want := []string{"第一句", "先答第一句。", "第二句", "再答第二句。"}; !slices.Equal(said, want) || len(thread.Pending) != 0 {
		t.Fatalf("thread = %q, want %q", said, want)
	}
}

// 打断只停下主编正在说的：打断后、回合收尾前送出的话不丢，开新回合答。
func TestInterruptKeepsWordsSentMeanwhile(t *testing.T) {
	gated := &gatedProvider{
		Provider: litellmtest.New(litellmtest.Text("答第一句。"), litellmtest.Text("答第二句。")),
		asked:    make(chan struct{}), release: make(chan struct{}),
	}
	api := chiefApp(t, gated)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()
	if err := session.Send(ctx, "第一句", chief.View{}); err != nil {
		t.Fatal(err)
	}
	<-gated.asked
	if !session.Interrupt() {
		t.Fatal("a turn is under way")
	}
	if err := session.Send(ctx, "第二句", chief.View{}); err != nil {
		t.Fatal(err)
	}
	close(gated.release)
	thread := settle(t, session)
	var said []string
	for _, entry := range thread.Entries {
		said = append(said, entry.Text)
	}
	if want := []string{"第一句", "第二句", "答第二句。"}; !slices.Equal(said, want) || thread.Err != "" {
		t.Fatalf("thread = %q (err %q), want %q", said, thread.Err, want)
	}
}

// 有稿件等用户决定：主编先看稿，再按用户的原话退回；退稿理由入账为该章要求，来源指回
// 那句话，创作随即按意见重写，写完又停下等用户。
// draftWaiting 是逐章确认、第 1 章稿件等用户决定的书：主编用 provider 回话。
func draftWaiting(t *testing.T, provider litellm.Provider) (*testApp, *chief.Session) {
	t.Helper()
	authorityStore := openTestStore(t)
	api := newTestApp(authorityStore, bootstrap.Options{
		Executors:   task.ExecutorSet{LLM: &scriptedQuickExecutor{now: time.Now().UTC(), authorityStore: authorityStore}},
		ChiefModels: scriptedModels{provider},
	})
	session := api.Chief.Open(context.Background(), "draft-book", "user-1")
	t.Cleanup(session.Close)
	session.Write(chief.WriteParams{Premise: "一个失忆的邮差替亡者送完最后一封信", Chapters: 1, Approval: model.ApprovalManual})
	plan := driven(t, session, 1)
	if _, err := api.Decisions.Approve(context.Background(), plan.Result.WaitingOperationID+"-proposal", "user-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	session.Write(chief.WriteParams{Chapters: novelapp.KeepChapters})
	if waiting := driven(t, session, 2); waiting.Err != nil || waiting.Result.RunState != model.RunWaitingUser {
		t.Fatalf("chapter draft = %+v", waiting)
	}
	return api, session
}

func TestChiefReadsTheDraftAndRejectsItInTheUsersWords(t *testing.T) {
	provider := litellmtest.New(
		chiefCall("draft", "book_draft", `{}`),
		chiefCall("reject", "reject_draft", `{"reason":"结尾太仓促了"}`),
		litellmtest.Text("退回了，按你的意见重写。"),
	)
	api, session := draftWaiting(t, provider)
	ctx := context.Background()

	thread := talk(t, session, "这稿结尾太仓促了，重写")
	if len(thread.Entries) != 4 || thread.Entries[1].Text != "看待定稿件" || thread.Entries[2].Text != "退回稿件 ·「结尾太仓促了」· 按这条意见重写" {
		t.Fatalf("thread = %+v", thread.Entries)
	}
	var draft string
	for _, message := range provider.Requests()[1].Messages {
		for _, block := range message.Blocks {
			if result, ok := block.(litellm.ToolResultBlock); ok && result.ToolUseID == "draft" {
				draft = agentcore.Message{Blocks: result.Content}.Text()
			}
		}
	}
	if !strings.Contains(draft, "第1章正文") {
		t.Fatalf("draft = %s", draft)
	}
	snap, err := api.Workbench.WorkbenchSnapshot(ctx, "draft-book")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Directives) != 1 || snap.Directives[0].Text != "结尾太仓促了" || snap.Directives[0].Source != model.ConversationSource(1) {
		t.Fatalf("directives = %+v", snap.Directives)
	}
	if rewritten := driven(t, session, 3); rewritten.Err != nil || rewritten.Result.RunState != model.RunWaitingUser {
		t.Fatalf("rewrite = %+v", rewritten)
	}
}

// 稿件等决定时记下落在这一章的要求：稿件过时了，回执写明，创作随即由后继按新要求重写，
// 不停在不能再通过的旧稿上（§5.5 第 4 条）。
func TestChiefRequirementOnAWaitingDraftRedoesIt(t *testing.T) {
	_, session := draftWaiting(t, litellmtest.New(
		chiefCall("note", "note_requirement", `{"text":"开头直接从拆信写起","scope":{"chapter":1}}`),
		litellmtest.Text("记下了，这一章按它重写。"),
	))
	thread := talk(t, session, "开头直接从拆信写起")
	if receipt := lastReceipt(t, thread); receipt.Text != "记下要求 · 第 1 章 ·「开头直接从拆信写起」 · 待定的稿件过时了，按现在的内容重写" {
		t.Fatalf("receipt = %+v", receipt)
	}
	redone := driven(t, session, 3)
	if redone.Err != nil || redone.Result.RunState != model.RunWaitingUser || !strings.HasSuffix(redone.Result.WaitingOperationID, ":redo2") {
		t.Fatalf("redone = %+v", redone)
	}
}

// 主编连不上模型服务时，对话里说清楚出了什么事，不贴原始错误链。
func TestChiefExplainsAnUnreachableModelService(t *testing.T) {
	offline := litellm.NewNetworkError("compat", "request failed", errors.New("dial tcp: lookup relay: no such host"))
	offline.Temporary = false // 不等退避：直接看重试用尽之后的样子
	api, _ := newChiefApp(t, litellmtest.Fail(offline))
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()
	if err := session.Send(ctx, "接着写", chief.View{}); err != nil {
		t.Fatal(err)
	}
	if thread := settle(t, session); thread.Err != model.FailureModelUnreachable.Explain("") {
		t.Fatalf("turn error = %q", thread.Err)
	}
}

// 用户说某条要求不要了：主编按编号退掉它；编号猜错时报错指回 book_requirements，照着改就成。
func TestChiefRetiresARequirementByItsNumber(t *testing.T) {
	var api *testApp
	retire := func() litellmtest.Reply {
		snap, err := api.Workbench.WorkbenchSnapshot(context.Background(), "chief-book")
		if err != nil || len(snap.Directives) != 1 {
			t.Errorf("directives = %+v, %v", snap.Directives, err)
			return litellmtest.Text("查不到。")
		}
		return chiefCall("retire", "retire_requirement", `{"id":"`+snap.Directives[0].ID+`"}`)
	}
	provider := &lazyProvider{replies: []func() litellmtest.Reply{
		fixed(chiefCall("note", "note_requirement", `{"text":"每章结尾留个钩子"}`)),
		fixed(litellmtest.Text("记下了。")),
		fixed(chiefCall("guess", "retire_requirement", `{"id":"1"}`)),
		retire,
		fixed(litellmtest.Text("那条不要了。")),
	}}
	api = chiefApp(t, provider)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()

	talk(t, session, "每章结尾留个钩子")
	thread := talk(t, session, "留钩子那条不要了")
	guess := slices.IndexFunc(thread.Entries, func(entry chief.Entry) bool { return entry.Tool == "retire_requirement" && entry.Failed })
	if guess < 0 || !strings.Contains(thread.Entries[guess].Text, "book_requirements") {
		t.Fatalf("a wrong number must point back to book_requirements: %+v", thread.Entries)
	}
	if receipt := lastReceipt(t, thread); receipt.Failed || receipt.Text != "退掉要求 ·「每章结尾留个钩子」" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if snap, err := api.Workbench.WorkbenchSnapshot(ctx, "chief-book"); err != nil || len(snap.Directives) != 0 {
		t.Fatalf("directives after retire = %+v, %v", snap.Directives, err)
	}
}
