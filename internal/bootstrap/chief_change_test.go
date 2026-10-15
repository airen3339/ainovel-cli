package bootstrap_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/app/chief"
	decisions "github.com/voocel/ainovel-cli/internal/app/decision"
	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/app/task"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/litellm"
	"github.com/voocel/litellm/litellmtest"
)

// lazyProvider 按调用次序现做回复：脚本要引用运行中才知道的东西（如修改编号）时用它。
type lazyProvider struct {
	mu      sync.Mutex
	replies []func() litellmtest.Reply
}

func (p *lazyProvider) Name() string { return "test" }

func (p *lazyProvider) Chat(ctx context.Context, request *litellm.Request) (*litellm.Response, error) {
	stream, err := p.Stream(ctx, request)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	return litellm.Collect(stream)
}

func (p *lazyProvider) Stream(ctx context.Context, request *litellm.Request) (litellm.Stream, error) {
	p.mu.Lock()
	reply := p.replies[0]
	p.replies = p.replies[1:]
	p.mu.Unlock()
	return litellmtest.New(reply()).Stream(ctx, request)
}

func fixed(reply litellmtest.Reply) func() litellmtest.Reply {
	return func() litellmtest.Reply { return reply }
}

func chapterTitle(t *testing.T, api *testApp, projectID, planID string) string {
	t.Helper()
	project, err := api.Projects.Project(context.Background(), projectID, model.InitialRevision)
	if err != nil {
		t.Fatal(err)
	}
	return model.IndexPlan(project.Plan)[planID].Title
}

func lastReceipt(t *testing.T, thread chief.Thread) chief.Entry {
	t.Helper()
	for i := len(thread.Entries) - 1; i >= 0; i-- {
		if entry := thread.Entries[i]; entry.Kind == chief.EntryAction && entry.Change {
			return entry
		}
	}
	t.Fatalf("no receipt in %+v", thread.Entries)
	return chief.Entry{}
}

// 改标题按审批直接生效：回执逐处说清改了什么，提案由主编署名、来源指回用户的话；之后
// 主编查修改记录、撤销它，标题恢复原样。
func TestChiefRetitlesAChapterAndUndoesIt(t *testing.T) {
	var api *testApp
	history := func() litellmtest.Reply {
		records, err := api.Workbench.History(context.Background(), "chief-book", 1)
		if err != nil || len(records) != 1 || !records[0].Undoable {
			t.Errorf("history = %+v, %v", records, err)
			return litellmtest.Text("查不到。")
		}
		return chiefCall("revert", "revert_change", `{"id":"`+records[0].ID+`"}`)
	}
	provider := &lazyProvider{replies: []func() litellmtest.Reply{
		fixed(chiefCall("title", "propose_change", `{"reason":"用户想要更长的标题","chapters":[{"chapter":1,"title":"抵达云雾缭绕的山门"}]}`)),
		fixed(litellmtest.Text("改好了。")),
		fixed(chiefCall("history", "book_history", `{}`)),
		history,
		fixed(litellmtest.Text("撤销了。")),
	}}
	api = chiefApp(t, provider)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()

	thread := talk(t, session, "第一章的标题换一个长一点的")
	if receipt := lastReceipt(t, thread); receipt.Failed || receipt.Text != "第 1 章《第一章》改名《抵达云雾缭绕的山门》 · 已生效，可撤销" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if title := chapterTitle(t, api, "chief-book", "chapter-plan-1"); title != "抵达云雾缭绕的山门" {
		t.Fatalf("title = %q", title)
	}
	records, err := api.Workbench.History(ctx, "chief-book", 10)
	if err != nil || len(records) != 1 || records[0].By != "主编" || records[0].Reason != "用户想要更长的标题" {
		t.Fatalf("history = %+v, %v", records, err)
	}
	changeSet, err := api.store.GetChangeSet(ctx, records[0].ID)
	if err != nil || changeSet.Author != model.ChiefAuthor || changeSet.Source != model.ConversationSource(1) {
		t.Fatalf("change set = %+v, %v", changeSet, err)
	}

	thread = talk(t, session, "算了，还是改回去吧")
	if receipt := lastReceipt(t, thread); receipt.Failed || receipt.Text != "撤销：第 1 章《抵达云雾缭绕的山门》改名《第一章》 · 已生效，可撤销" {
		t.Fatalf("revert receipt = %+v", receipt)
	}
	if title := chapterTitle(t, api, "chief-book", "chapter-plan-1"); title != "第一章" {
		t.Fatalf("title after undo = %q", title)
	}
	// 撤销本身也是一次修改：最新的那次可以再撤销，被撤销的那次其后改过同一处，不能单独撤销。
	if records, err := api.Workbench.History(ctx, "chief-book", 10); err != nil || len(records) != 2 || !records[0].Undoable || records[1].Undoable {
		t.Fatalf("history after undo = %+v, %v", records, err)
	}
}

// 审批要求用户确认时，修改等用户按键：书况告诉主编有修改在等；再提一份替换旧的；主编
// 照用户的话撤回，作品不变；用户确认的那份生效。
func TestChiefChangeWaitsForTheUserUnderManualApproval(t *testing.T) {
	var api *testApp
	withdraw := func() litellmtest.Reply {
		proposal, ok, err := api.Decisions.PendingChange(context.Background(), "manual-book")
		if err != nil || !ok {
			t.Errorf("pending = %+v, %v, %v", proposal, ok, err)
			return litellmtest.Text("没有。")
		}
		return chiefCall("withdraw", "revert_change", `{"id":"`+proposal.ID+`"}`)
	}
	provider := &lazyProvider{replies: []func() litellmtest.Reply{
		fixed(chiefCall("title", "propose_change", `{"reason":"换标题","chapters":[{"chapter":1,"title":"山门"}]}`)),
		fixed(litellmtest.Text("等你确认。")),
		fixed(chiefCall("title", "propose_change", `{"reason":"换个更好的标题","chapters":[{"chapter":1,"title":"云中山门"}]}`)),
		fixed(litellmtest.Text("换成这个了，等你确认。")),
		withdraw,
		fixed(litellmtest.Text("撤回了。")),
		fixed(chiefCall("title", "propose_change", `{"reason":"换标题","chapters":[{"chapter":1,"title":"山门"}]}`)),
		fixed(litellmtest.Text("等你确认。")),
	}}
	api = newTestApp(openTestStore(t), bootstrap.Options{ChiefModels: scriptedModels{provider}})
	ctx := context.Background()
	draft := testProjectDraft()
	draft.Approval = model.ApprovalManual
	if _, err := api.Projects.CreateProject(ctx, projectdoc.CreateProjectCommand{
		ProjectID: "manual-book", ChangeID: "create", UserID: "user-1", Reason: "建立作品", Draft: draft, CreatedAt: testTime(),
	}); err != nil {
		t.Fatal(err)
	}
	session := api.Chief.Open(ctx, "manual-book", "user-1")
	defer session.Close()

	thread := talk(t, session, "第一章标题改成山门")
	if receipt := lastReceipt(t, thread); receipt.Text != "第 1 章《第一章》改名《山门》 · 等你确认" {
		t.Fatalf("receipt = %+v", receipt)
	}
	snap, err := api.Workbench.WorkbenchSnapshot(ctx, "manual-book")
	if err != nil || snap.Change == nil || snap.Change.Stale || len(snap.Change.Options) != 0 || chapterTitle(t, api, "manual-book", "chapter-plan-1") != "第一章" {
		t.Fatalf("pending change = %+v, %v", snap.Change, err)
	}
	first := snap.Change.Proposal.ID

	thread = talk(t, session, "换个更好的")
	if receipt := lastReceipt(t, thread); receipt.Text != "替换上一份待确认的修改 · 第 1 章《第一章》改名《云中山门》 · 等你确认" {
		t.Fatalf("superseding receipt = %+v", receipt)
	}
	if replaced, err := api.store.GetProposal(ctx, first); err != nil || replaced.ApprovalState != model.ApprovalRejected {
		t.Fatalf("replaced = %+v, %v", replaced, err)
	}
	thread = talk(t, session, "不改了")
	if receipt := lastReceipt(t, thread); receipt.Text != "撤回待确认的修改 · 作品不变" {
		t.Fatalf("withdraw receipt = %+v", receipt)
	}
	if snap, err := api.Workbench.WorkbenchSnapshot(ctx, "manual-book"); err != nil || snap.Change != nil {
		t.Fatalf("after withdraw = %+v, %v", snap.Change, err)
	}

	talk(t, session, "还是改成山门")
	pending, ok, err := api.Decisions.PendingChange(ctx, "manual-book")
	if err != nil || !ok {
		t.Fatalf("pending = %v, %v", ok, err)
	}
	if _, err := api.Decisions.Approve(ctx, pending.ID, "user-1", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if title := chapterTitle(t, api, "manual-book", "chapter-plan-1"); title != "山门" {
		t.Fatalf("title = %q", title)
	}
}

// 作品方向以用户的名义写入：改写过的话被拒；摘了原话的也要用户确认（D25）。
func TestChiefIntentChangeQuotesTheUserAndWaits(t *testing.T) {
	api, provider := newChiefApp(t,
		chiefCall("bad", "propose_change", `{"reason":"结局改悲剧","intent":{"ending_direction":"主角失去了一切"}}`),
		chiefCall("good", "propose_change", `{"reason":"结局改悲剧","intent":{"ending_direction":"主角最终失去一切"}}`),
		litellmtest.Text("等你确认。"),
		litellmtest.Text("好的。"),
	)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()
	thread := talk(t, session, "结局改成主角最终失去一切")
	if rejected := thread.Entries[1]; !rejected.Failed || !strings.Contains(rejected.Text, "不是用户说过的原话") {
		t.Fatalf("unquoted = %+v", rejected)
	}
	if receipt := lastReceipt(t, thread); receipt.Text != "作品方向：结局方向改为「主角最终失去一切」 · 等你确认" {
		t.Fatalf("receipt = %+v", receipt)
	}
	// 下一句话的书况告诉主编有修改在等用户。
	talk(t, session, "好")
	requests := provider.Requests()
	last := requests[len(requests)-1]
	brief := last.Messages[len(last.Messages)-1].Blocks[0].(litellm.TextBlock).Text
	if !strings.Contains(brief, "待用户确认的修改：结局改悲剧。用户按 y 确认") {
		t.Fatalf("brief = %q", brief)
	}
}

// 作品前提例外（D87）：改设定时主编照改后的样子改写前提，不必是用户的原话；作品意图的修改
// 总要用户确认，确认即署名。
func TestChiefRewritesThePremiseAndWaits(t *testing.T) {
	api, _ := newChiefApp(t,
		chiefCall("premise", "propose_change", `{"reason":"信物改成旧邮票","intent":{"premise":"邮差陈渡循着一枚旧邮票找姐姐"}}`),
		litellmtest.Text("等你确认。"),
	)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()
	thread := talk(t, session, "信里那把铜钥匙其实是一枚旧邮票")
	if receipt := lastReceipt(t, thread); receipt.Failed || !strings.HasSuffix(receipt.Text, "等你确认") {
		t.Fatalf("receipt = %+v", receipt)
	}
	snap, err := api.Workbench.WorkbenchSnapshot(ctx, "chief-book")
	if err != nil || snap.Change == nil {
		t.Fatalf("change = %+v, %v", snap.Change, err)
	}
}

// 改设定牵连已写的章：语义影响分析给出冲突，修改等用户三选一（S4）；用户选只改设定、
// 后文按新设定写，修改生效。
func TestChiefCanonChangeConflictingWithWrittenChaptersOffersChoices(t *testing.T) {
	provider := litellmtest.New(
		chiefCall("canon", "propose_change", `{"reason":"主角的底线变了","facts":[{"subject":"主角","predicate":"rule.bottom_line","value":"可以为目的伤害无辜"}]}`),
		litellmtest.Text("和第一章有出入，你选怎么处理。"),
	)
	authorityStore := openTestStore(t)
	api := newTestApp(authorityStore, bootstrap.Options{
		Executors: task.ExecutorSet{LLM: preferenceTestExecutor{}}, ChiefModels: scriptedModels{provider},
	})
	ctx := context.Background()
	at := testTime()
	if _, err := api.Projects.CreateProject(ctx, projectdoc.CreateProjectCommand{
		ProjectID: "chief-book", ChangeID: "create", UserID: "user-1", Reason: "建立作品", Draft: testProjectDraft(), CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	projection, err := api.Projects.ExportProject(ctx, "chief-book", 0)
	if err != nil {
		t.Fatal(err)
	}
	projection.Manuscript = append(projection.Manuscript, model.ManuscriptChapter{
		ID: "chapter-1", PlanNodeID: "chapter-plan-1", Number: 1, Author: model.AuthorUser,
		Blocks:    []model.ManuscriptBlock{{ID: "block-1", Text: "他在山门前放下了刀。"}},
		DependsOn: []model.DocumentRef{{Kind: model.DocumentEntity, ID: "hero"}},
	})
	written, err := api.Projects.ImportProject(ctx, "write-chapter-1", "user-1", "写入第一章", projection, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.Decisions.Approve(ctx, written.ID, "user-1", at.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()

	thread := talk(t, session, "主角的底线改成可以为目的伤害无辜")
	if receipt := lastReceipt(t, thread); receipt.Text != "设定 · 「主角」可以为目的伤害无辜 · 与已写的章节有出入，等你在确认卡上输入序号选怎么处理" {
		t.Fatalf("receipt = %+v", receipt)
	}
	snap, err := api.Workbench.WorkbenchSnapshot(ctx, "chief-book")
	if err != nil || snap.Change == nil || snap.Change.Stale || len(snap.Change.Findings) != 1 || len(snap.Change.Options) != 3 {
		t.Fatalf("change = %+v, %v", snap.Change, err)
	}
	options := snap.Change.Options
	if options[0].Strategy != model.ResolutionRewriteAffected || len(options[0].Chapters) != 1 || options[0].Chapters[0] != 1 ||
		options[1].Strategy != model.ResolutionReinterpretFuture || options[2].Strategy != model.ResolutionAbandon {
		t.Fatalf("options = %+v", options)
	}
	if _, err := api.Decisions.ResolveProposal(ctx, decisions.ResolveProposalCommand{
		ProposalID: snap.Change.Proposal.ID, UserID: "user-1", Strategy: string(model.ResolutionReinterpretFuture), CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	project, err := api.Projects.Project(ctx, "chief-book", model.InitialRevision)
	if err != nil || string(project.Canon[0].Value) != `"可以为目的伤害无辜"` {
		t.Fatalf("canon = %+v, %v", project.Canon, err)
	}
}

// 还没写的章也能锁（S5 的关键剧情节点）：锁的是这一章的大纲，锁定要用户确认（D25）。
func TestChiefLocksAPlannedChapter(t *testing.T) {
	api, _ := newChiefApp(t,
		chiefCall("lock", "propose_change", `{"reason":"用户要锁住第一章","locks":[{"chapter":1,"control":"locked"}]}`),
		litellmtest.Text("等你确认。"),
	)
	ctx := context.Background()
	session := api.Chief.Open(ctx, "chief-book", "user-1")
	defer session.Close()
	thread := talk(t, session, "第一章锁住，别让 AI 动")
	if receipt := lastReceipt(t, thread); receipt.Failed || receipt.Text != "锁定第 1 章《第一章》的大纲 · 等你确认" {
		t.Fatalf("receipt = %+v", receipt)
	}
	pending, ok, err := api.Decisions.PendingChange(ctx, "chief-book")
	if err != nil || !ok {
		t.Fatalf("pending = %v, %v", ok, err)
	}
	if err := session.Confirm(ctx, pending.ID); err != nil {
		t.Fatal(err)
	}
	project, err := api.Projects.Project(ctx, "chief-book", model.InitialRevision)
	if err != nil {
		t.Fatal(err)
	}
	plan := model.DocumentRef{Kind: model.DocumentPlan, ID: "chapter-plan-1"}
	if !slices.ContainsFunc(project.Ownership, func(rule model.OwnershipRule) bool { return rule.Target == plan && rule.Control == model.ControlLocked }) {
		t.Fatalf("ownership = %+v", project.Ownership)
	}
}
