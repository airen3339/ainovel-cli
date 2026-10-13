package bootstrap_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/app/chief"
	novelapp "github.com/voocel/ainovel-cli/internal/app/novel"
	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/app/workbench"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	appconfig "github.com/voocel/ainovel-cli/internal/infra/config"
	"github.com/voocel/ainovel-cli/internal/infra/store"
)

// 真实模型验收（§0.4，D37）：AINOVEL_LIVE 是数据库路径，设了才跑，用本机配置的模型。断言只管
// 结构，文学上的判断看日志与诊断：
//
//	AINOVEL_LIVE=/tmp/live.db go test ./internal/bootstrap -run TestLive -v -timeout 8h
//	go run ./cmd/ainovel-cli --headless --db /tmp/live.db diag --project <日志里的作品>
const liveUserID = "live-user"

// internalID 是文档编号的样子（c4、f43、e16、b1）：给用户看的文字里不该出现（D66）。
var internalID = regexp.MustCompile(`(^|[^A-Za-z0-9])[a-z]{1,2}[0-9]+([^A-Za-z0-9]|$)`)

const livePremise = "雨城邮差陈渡在一封无人认领的信里发现一把铜钥匙，循着它去找失踪多年的姐姐，却撞破了邮局局长的秘密。"

// liveUser 是验收里的用户：经主编说话，按工作台的按键处理决定卡与确认卡。
type liveUser struct {
	t       *testing.T
	ctx     context.Context
	api     *bootstrap.App
	session *chief.Session
	project string
	start   time.Time
	seen    int // 已记进日志的对话条目
	ended   int // 已记进日志的推进
	cue     *liveCue
}

// liveCue 是写作途中插的一句话：现场走到 phase 时说。
type liveCue struct{ phase, words string }

func openLive(t *testing.T) *liveUser {
	path := os.Getenv("AINOVEL_LIVE")
	if path == "" {
		t.Skip("AINOVEL_LIVE 未设：真实模型验收不跑")
	}
	dir, err := appconfig.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	config, err := appconfig.ResolveConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	authorityStore, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { authorityStore.Close() })
	api := bootstrap.New(authorityStore, bootstrap.Options{Version: "live", ConfigDir: t.TempDir()})
	if err := api.Models.Apply(config); err != nil {
		t.Fatal(err)
	}
	project := "live-" + time.Now().Format("20060102-150405")
	session := api.Chief.Open(ctx, project, liveUserID)
	t.Cleanup(func() {
		session.Close()
		api.Chief.Wait(30 * time.Second)
	})
	t.Logf("作品 %s · 数据库 %s · 模型 %s", project, path, config.Model)
	return &liveUser{t: t, ctx: ctx, api: api, session: session, project: project, start: time.Now()}
}

func (u *liveUser) log(format string, args ...any) {
	u.t.Helper()
	u.t.Logf("[%s] %s", time.Since(u.start).Round(time.Second), fmt.Sprintf(format, args...))
}

func (u *liveUser) snapshot() workbench.WorkbenchSnapshot {
	u.t.Helper()
	snap, err := u.api.Workbench.WorkbenchSnapshot(u.ctx, u.project)
	if err != nil {
		u.t.Fatal(err)
	}
	return snap
}

// say 跟主编说一句话，等它回完，返回这一回合的条目。
func (u *liveUser) say(words string, chapter int) []chief.Entry {
	u.t.Helper()
	u.log("用户：%s", words)
	if err := u.session.Send(u.ctx, words, chief.View{Chapter: chapter}); err != nil {
		u.t.Fatal(err)
	}
	deadline := time.After(20 * time.Minute)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for u.session.Busy() {
		select {
		case <-u.session.Changes():
		case <-tick.C:
		case <-deadline:
			u.t.Fatal("主编 20 分钟没有回完")
		}
	}
	thread := u.session.Thread()
	turn := thread.Entries[u.seen:]
	for _, entry := range turn {
		switch {
		case entry.Kind == chief.EntryChief:
			u.log("主编：%s", entry.Text)
		case entry.Kind == chief.EntryAction && entry.Failed:
			u.log("  ✗ %s", entry.Text)
		case entry.Kind == chief.EntryAction:
			u.log("  · %s", entry.Text)
		}
	}
	u.seen = len(thread.Entries)
	if thread.Err != "" {
		u.t.Errorf("主编这一回合出错：%s", thread.Err)
	}
	return turn
}

// used 报告这一回合里主编是否成功用了某个工具。
func used(turn []chief.Entry, tool string) bool {
	return slices.ContainsFunc(turn, func(entry chief.Entry) bool { return entry.Tool == tool && entry.Done && !entry.Failed })
}

// idle 等推进与主编都停下；途中按 cue 插话。
func (u *liveUser) idle() {
	u.t.Helper()
	deadline := time.Now().Add(90 * time.Minute)
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	phase := ""
	for {
		production := u.session.Production()
		if !production.Writing && !u.session.Busy() {
			if production.Ended > u.ended {
				u.ended = production.Ended
				end := production.Last
				u.log("推进停下：%s %s %v", end.Result.RunState, end.Result.RunReason, end.Err)
			}
			return
		}
		if time.Now().After(deadline) {
			u.t.Fatal("推进 90 分钟没有停下")
		}
		select {
		case <-u.session.Changes():
		case <-tick.C:
			if current := u.snapshot().CurrentPhase; current != phase {
				phase = current
				u.log("现场：%s", phase)
			}
			if u.cue != nil && strings.Contains(phase, u.cue.phase) {
				cue := u.cue
				u.cue = nil
				u.say(cue.words, 0)
			}
		}
	}
}

// advance 像用户守着工作台一样推进，直到 stop：稿件按 y 通过，主编的修改按 y 确认，修订
// 预算用尽的章接受现状，停着就回车继续。
func (u *liveUser) advance(stop func(workbench.WorkbenchSnapshot) bool) workbench.WorkbenchSnapshot {
	u.t.Helper()
	stuck := ""
	for range 100 {
		u.idle()
		snap := u.snapshot()
		if stop(snap) {
			return snap
		}
		if snap.Run != nil {
			at := fmt.Sprintf("%s %s %s", snap.Run.State, snap.Run.StateReason, snap.Run.WaitingOperationID)
			if at == stuck {
				u.t.Fatalf("回车继续后原地不动：%s", at)
			}
			stuck = ""
			if snap.Change == nil && (snap.Decision == nil || snap.Decision.Stale || !snap.Decision.HasProposal && snap.Decision.Held == "") {
				stuck = at // 下面会回车继续
			}
		}
		switch {
		case snap.Change != nil && (snap.Change.Stale || len(snap.Change.Options) > 0):
			u.t.Fatalf("意外的确认卡：%+v", snap.Change)
		case snap.Change != nil:
			u.log("y 确认主编的修改：%s", snap.Change.Proposal.Reason)
			if err := u.session.Confirm(u.ctx, snap.Change.Proposal.ID); err != nil {
				u.t.Fatal(err)
			}
		case snap.Decision != nil && snap.Decision.Held != "":
			u.accept(snap)
		case snap.Decision != nil && snap.Decision.HasProposal && !snap.Decision.Stale:
			u.approve(snap)
		case snap.Run == nil || snap.Run.State == model.RunCompleted || snap.Run.State == model.RunCancelled:
			u.t.Fatalf("推进停在意外的地方：%+v", snap.Run)
		case snap.Run.State == model.RunFailed:
			u.t.Fatalf("创作失败：%s", snap.Run.StateReason)
		default:
			u.log("回车继续（%s：%s）", snap.Run.State, snap.Run.StateReason)
			u.session.Write(chief.WriteParams{Chapters: novelapp.KeepChapters})
		}
	}
	u.t.Fatal("推进 100 步还没到")
	return workbench.WorkbenchSnapshot{}
}

func (u *liveUser) approve(snap workbench.WorkbenchSnapshot) {
	u.t.Helper()
	what := "规划方案"
	if number := draftOf(snap); number > 0 {
		what = fmt.Sprintf("第 %d 章", number)
	}
	u.log("y 通过%s（%s）", what, snap.Decision.Reason)
	if _, err := u.api.Decisions.Approve(u.ctx, snap.Decision.Proposal.ID, liveUserID, time.Now().UTC()); err != nil {
		u.t.Fatal(err)
	}
	u.session.Write(chief.WriteParams{Chapters: novelapp.KeepChapters})
}

// accept 是修订预算用尽时按 y 接受现状：阻塞意见逐条入账为用户裁决（D43）。
func (u *liveUser) accept(snap workbench.WorkbenchSnapshot) {
	u.t.Helper()
	u.log("y 接受现状：%s", snap.Decision.Reason)
	now := time.Now().UTC()
	base := projectdoc.NewID("adjudication", now)
	for i, finding := range snap.Findings {
		if finding.ChapterID != snap.Decision.Held || finding.Severity != model.FindingBlocking {
			continue
		}
		u.log("  接受的阻塞意见：%s", finding.Note)
		if _, err := u.api.Reviews.AddAdjudication(u.ctx, novelapp.AddAdjudicationCommand{
			ProjectID: u.project, ChangeID: fmt.Sprintf("%s:%d", base, i+1), UserID: liveUserID,
			Finding: finding.ID, Reason: "接受当前版本", CreatedAt: now,
		}); err != nil {
			u.t.Fatal(err)
		}
	}
	u.session.Write(chief.WriteParams{Chapters: novelapp.KeepChapters})
}

// draftOf 是等用户决定的章节稿的章号；不是章节稿为 0。
func draftOf(snap workbench.WorkbenchSnapshot) int {
	if snap.Decision == nil || !snap.Decision.HasProposal {
		return 0
	}
	for _, candidate := range snap.Candidates {
		if candidate.OperationID == snap.Decision.Proposal.OperationID {
			return candidate.Chapter.Number
		}
	}
	return 0
}

func completed(snap workbench.WorkbenchSnapshot) bool {
	return snap.Run != nil && snap.Run.State == model.RunCompleted
}

func liveDirective(snap workbench.WorkbenchSnapshot, match func(model.Directive) bool) *model.Directive {
	for _, directive := range snap.Directives {
		if match(directive) {
			return &directive
		}
	}
	return nil
}

func liveChapter(snap workbench.WorkbenchSnapshot, number int) model.ManuscriptChapter {
	for _, chapter := range snap.Manuscript {
		if chapter.Number == number {
			return chapter
		}
	}
	return model.ManuscriptChapter{}
}

func liveText(chapter model.ManuscriptChapter) string {
	var text strings.Builder
	for _, block := range chapter.Blocks {
		text.WriteString(block.Text)
	}
	return text.String()
}

func (u *liveUser) directives(snap workbench.WorkbenchSnapshot) {
	for _, directive := range snap.Directives {
		u.log("  要求 %s ·「%s」· rework=%t · source=%s · %+v", directive.Scope, directive.Text, directive.Rework, directive.Source, directive.Constraints)
	}
}

// 一本 4 章、逐章确认的书：开书（S1）；规划等确认时提字数与第 2 章的要求（S13、S11）；退回
// 第 1 章并说明原因（S10）；写第 2 章途中插话（S12）；完本后点名返工第 3 章（D78）。
func TestLiveChiefAcceptance(t *testing.T) {
	u := openLive(t)

	u.log("S1：一句话开书，4 章，逐章确认")
	u.session.Write(chief.WriteParams{Premise: livePremise, Chapters: 4, Approval: model.ApprovalManual})
	u.advance(func(snap workbench.WorkbenchSnapshot) bool { return snap.Decision != nil && snap.Decision.HasProposal })

	u.log("S13、S11：规划等确认时提字数与第 2 章的要求")
	u.say("每章两千字左右。第 2 章陈渡要吃一次大亏，别让他赢。", 0)
	snap := u.snapshot()
	u.directives(snap)
	if words := liveDirective(snap, func(d model.Directive) bool { return d.Constraints != nil }); words == nil ||
		words.Scope != model.DirectiveScopeProject || words.Constraints.TargetWords != 2000 {
		t.Errorf("S13：字数要求没有记成全书每章 2000 字：%+v", words)
	}
	setback := liveDirective(snap, func(d model.Directive) bool { return d.Scope == model.DirectiveScopeChapters(2, 2) })
	if setback == nil || setback.Source == "" || !strings.Contains(setback.Text, "大亏") {
		t.Errorf("S11：第 2 章的要求没有按原话记在第 2 章：%+v", setback)
	}

	u.log("S10：第 1 章稿件等确认时退回并说明原因")
	u.advance(func(snap workbench.WorkbenchSnapshot) bool { return draftOf(snap) == 1 })
	turn := u.say("第 1 章不行：开头铺垫太长，直接从陈渡拆开那封信写起。", 1)
	snap = u.snapshot()
	u.directives(snap)
	if !used(turn, "reject_draft") {
		t.Errorf("S10：主编没有退回稿件")
	}
	if rejection := liveDirective(snap, func(d model.Directive) bool { return strings.Contains(d.Text, "开头铺垫太长") }); rejection == nil || rejection.Source == "" {
		t.Errorf("S10：退稿理由没有按原话入账：%+v", rejection)
	}

	u.log("S12：写第 2 章途中插话；之后逐章通过到完本")
	u.cue = &liveCue{phase: "落笔第 2 章", words: "接下来让邮局局长露个面，先别揭穿他。"}
	snap = u.advance(completed)
	u.directives(snap)
	if u.cue != nil {
		t.Errorf("S12：没赶上第 2 章的写作途中")
	}
	// “接下来”可以是正在写的这一章，也可以是从这一章起：都不该落到已写的章上。
	if villain := liveDirective(snap, func(d model.Directive) bool { return strings.Contains(d.Text, "邮局局长") }); villain == nil ||
		villain.Source == "" || villain.Covers(model.DirectiveTarget{ChapterNumber: 1}) {
		t.Errorf("S12：插话没有记成正在写的章及之后的要求：%+v", villain)
	}
	if len(snap.Manuscript) != 4 {
		t.Fatalf("完本应有 4 章：%d", len(snap.Manuscript))
	}
	for _, chapter := range snap.Manuscript {
		u.log("第 %d 章《%s》%d 字", chapter.Number, outlineTitle(snap, chapter.PlanNodeID), chapter.Runes())
		if n := chapter.Runes(); n < 1800 || n > 2200 {
			t.Errorf("S13：第 %d 章 %d 字，超出两千字的 ±10%%", chapter.Number, n)
		}
	}
	if setback != nil {
		requirement := novelapp.DirectiveRequirementID(setback.ID)
		for _, r := range snap.Briefs[2].Requirements {
			if r.ID == requirement && r.Status != model.CheckSatisfied {
				t.Errorf("S11：第 2 章的要求审阅结论是 %q", r.Status)
			}
		}
	}

	u.log("D78：完本后点名返工第 3 章")
	first := snap.Run.ID
	before := make(map[int]string)
	for _, chapter := range snap.Manuscript {
		before[chapter.Number] = liveText(chapter)
	}
	u.say("第 3 章太拖了，紧凑点重写。", 3)
	snap = u.snapshot()
	u.directives(snap)
	if rework := liveDirective(snap, func(d model.Directive) bool { return d.Rework }); rework == nil || rework.Scope != model.DirectiveScopeChapters(3, 3) {
		t.Errorf("D78：没有记成第 3 章的返工要求：%+v", rework)
	}
	snap = u.advance(completed)
	if snap.Run.ID == first {
		t.Errorf("D78：完本后的返工没有开新一轮")
	}
	for _, chapter := range snap.Manuscript {
		changed := liveText(chapter) != before[chapter.Number]
		u.log("第 %d 章 %d 字 · 改过 %t", chapter.Number, chapter.Runes(), changed)
		if changed != (chapter.Number == 3) {
			t.Errorf("D78：第 %d 章改过=%t，只该重写第 3 章", chapter.Number, changed)
		}
	}
}

func outlineTitle(snap workbench.WorkbenchSnapshot, planID string) string {
	for _, entry := range snap.Outline {
		if entry.Node.ID == planID {
			return entry.Node.Title
		}
	}
	return ""
}

// S4：两章、自动推进的书写完后，改一个已经写进正文的设定；有出入时选重写受影响的章。主编
// 吃不准改动的用意时会先问，验收里的用户答一句。
func TestLiveSettingChange(t *testing.T) {
	u := openLive(t)
	u.session.Write(chief.WriteParams{Premise: livePremise, Chapters: 2, Approval: model.ApprovalAuto})
	snap := u.advance(completed)
	first := snap.Run.ID
	written := make(map[int]string)
	for _, chapter := range snap.Manuscript {
		written[chapter.Number] = liveText(chapter)
	}
	u.say("改个设定：信里那把铜钥匙其实是一枚旧邮票。", 0)
	if snap = u.snapshot(); snap.Change == nil {
		u.say("它只是姐姐留下的信物和线索，开锁的事你看着改。", 0) // 主编吃不准用法时会先问
		snap = u.snapshot()
	}
	var rewrite []int
	switch change := snap.Change; {
	case change == nil:
		t.Errorf("S4：主编没有提出修改")
	case len(change.Options) == 0:
		t.Errorf("S4：语义分析没有发现与已写章节的出入：%+v", change.View)
		u.log("y 确认主编的修改")
		if err := u.session.Confirm(u.ctx, change.Proposal.ID); err != nil {
			t.Fatal(err)
		}
	default:
		card := strings.Join(change.Findings, "\n")
		for _, finding := range change.Findings {
			u.log("  出入：%s", finding)
		}
		for i, option := range change.Options {
			u.log("  %d %s %v：%s", i+1, option.Strategy, option.Chapters, option.Explanation)
			card += "\n" + option.Explanation
			if option.Strategy == model.ResolutionRewriteAffected {
				rewrite = option.Chapters
			}
		}
		if id := internalID.FindString(card); id != "" {
			t.Errorf("S4：确认卡露出了内部编号 %q", id)
		}
		u.log("选 1 重写受影响的章")
		if err := u.session.Resolve(u.ctx, change.Proposal.ID, model.ResolutionRewriteAffected); err != nil {
			t.Fatal(err)
		}
	}
	snap = u.advance(completed)
	if snap.Run.ID == first {
		t.Errorf("S4：完本后的修改没有开新一轮")
	}
	for _, chapter := range snap.Manuscript {
		text := liveText(chapter)
		changed := text != written[chapter.Number]
		u.log("第 %d 章：改过 %t · 钥匙 %d 处 · 邮票 %d 处", chapter.Number, changed, strings.Count(text, "钥匙"), strings.Count(text, "邮票"))
		if slices.Contains(rewrite, chapter.Number) && (!changed || !strings.Contains(text, "邮票")) {
			t.Errorf("S4：第 %d 章没有按改过的设定重写", chapter.Number)
		}
	}

}
