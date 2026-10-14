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
	resources "github.com/voocel/ainovel-cli/internal/app/resource"
	"github.com/voocel/ainovel-cli/internal/app/workbench"
	"github.com/voocel/ainovel-cli/internal/bootstrap"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/activity"
	"github.com/voocel/ainovel-cli/internal/infra/capability/prompt"
	appconfig "github.com/voocel/ainovel-cli/internal/infra/config"
	"github.com/voocel/ainovel-cli/internal/infra/store"
)

// 真实模型验收（§0.4，D37）：AINOVEL_LIVE 是数据库路径，设了才跑，用本机配置的模型。断言只管
// 结构，文学上的判断看日志与诊断：
//
//	AINOVEL_LIVE=/tmp/live.db go test ./internal/bootstrap -run TestLive -v -timeout 8h
//	go run ./cmd/ainovel-cli --headless --db /tmp/live.db diag --project <日志里的作品>
//
// AINOVEL_LIVE_PROJECT 给出库里已有的书时接着跑它（长篇超时后续跑）。
// 换模型、思考强度用 AINOVEL_MODEL、AINOVEL_THINKING（见 TestLiveModelTrial）。
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
	seen    int    // 已记进日志的对话条目
	ended   int    // 已记进日志的推进
	retried uint64 // 已看过的实时活动条目
	cue     *liveCue
	watch   func(workbench.WorkbenchSnapshot) // 推进途中每次看现场时调用
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
	project := os.Getenv("AINOVEL_LIVE_PROJECT") // 接着跑库里已有的书
	if project == "" {
		project = "live-" + time.Now().Format("20060102-150405")
	}
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

// idle 等推进与主编都停下；途中按 cue 插话。现场 90 分钟没有变化算卡住。
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
			u.t.Fatal("现场 90 分钟没有变化")
		}
		select {
		case <-u.session.Changes():
		case <-tick.C:
			snap := u.snapshot()
			if snap.CurrentPhase != phase {
				phase = snap.CurrentPhase
				deadline = time.Now().Add(90 * time.Minute)
				u.log("现场：%s", phase)
			}
			u.retries()
			if u.watch != nil {
				u.watch(snap)
			}
			if snap.Change != nil && !snap.Change.Stale && len(snap.Change.Options) == 0 {
				u.confirm(snap) // 守着工作台的用户看到确认卡就按 y，不等创作停下
			}
			if u.cue != nil && strings.Contains(phase, u.cue.phase) {
				cue := u.cue
				u.cue = nil
				u.say(cue.words, 0)
			}
		}
	}
}

// retries 把重试记进日志：模型调用断了重发、任务重开只出现在实时活动里，不落库。
func (u *liveUser) retries() {
	feed, ok := u.api.Workbench.RunActivity(u.project)
	if !ok {
		return
	}
	for _, entry := range feed.Entries {
		if entry.ID <= u.retried {
			continue
		}
		u.retried = entry.ID
		switch {
		case entry.Kind == activity.Retry:
			u.log("重试第 %d 次：%s", entry.Attempt, entry.Err)
		case entry.Kind == activity.Notice && entry.Tone == activity.ToneRetry:
			u.log("%s", entry.Text)
		}
	}
}

// advance 像用户守着工作台一样推进，直到 stop：稿件按 y 通过，主编的修改按 y 确认，修订
// 预算用尽的章接受现状，停着就回车继续。
func (u *liveUser) advance(stop func(workbench.WorkbenchSnapshot) bool) workbench.WorkbenchSnapshot {
	u.t.Helper()
	stuck := ""
	for range 200 {
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
			u.confirm(snap)
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
	u.t.Fatal("推进 200 步还没到")
	return workbench.WorkbenchSnapshot{}
}

func (u *liveUser) confirm(snap workbench.WorkbenchSnapshot) {
	u.t.Helper()
	u.log("y 确认主编的修改：%s", snap.Change.Proposal.Reason)
	if err := u.session.Confirm(u.ctx, snap.Change.Proposal.ID); err != nil {
		u.t.Fatal(err)
	}
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

// planSummary 是第 number 章的规划梗概。
func planSummary(snap workbench.WorkbenchSnapshot, number int) string {
	for _, entry := range snap.Outline {
		if entry.Node.Kind == model.PlanChapter && entry.Number == number {
			return entry.Node.Summary
		}
	}
	return ""
}

// S2、S5：完善设定开书（受众、必须、禁止、结局方向），蓝图等确认时给出自己的前两章大纲、
// 后两章交给 AI；开写后锁住主角人设与第 4 章。写完看用户的大纲落进了那两章的规划、锁定
// 生效且 AI 没动锁住的内容、要求都核验通过。
func TestLiveDetailedSetup(t *testing.T) {
	u := openLive(t)
	intent := model.Intent{
		Premise: livePremise, Audience: "喜欢悬疑的成年读者", Required: []string{"每章结尾留悬念"},
		Forbidden: []string{"超自然力量"}, EndingDirection: "姐弟重逢，局长伏法",
	}
	u.log("S2：完善设定开书，4 章，里程碑确认")
	u.session.Write(chief.WriteParams{Intent: &intent, Chapters: 4, Approval: model.ApprovalMilestone})
	u.advance(func(snap workbench.WorkbenchSnapshot) bool { return snap.Decision != nil && snap.Decision.HasProposal })

	u.log("S2：蓝图等确认时给前两章的大纲（与常见开局不同，蓝图得按它重拟）")
	const outline = "大纲前两章按我的来：第 1 章从局长的视角写，他在雨夜烧掉一封信；第 2 章陈渡才出场，在退信里发现姐姐的信。后两章你来安排。"
	turn := u.say(outline, 0)
	u.log("退回蓝图：%v", used(turn, "reject_draft"))
	u.cue = &liveCue{phase: "落笔第 1 章", words: "陈渡的人设锁死：沉默寡言，左手有旧伤，别改。第 4 章也锁住，别让 AI 动。"}
	var locked string // 锁定时第 4 章的规划
	u.watch = func(snap workbench.WorkbenchSnapshot) {
		if locked == "" && u.cue == nil && liveOwned(u, model.DocumentPlan) {
			locked = planSummary(snap, 4)
		}
	}
	snap := u.advance(completed)
	if u.cue != nil {
		t.Fatal("S5：没赶上第 1 章的写作途中")
	}
	u.directives(snap)
	for number := 1; number <= len(snap.Manuscript); number++ {
		u.log("第 %d 章《%s》%d 字：%s", number, outlineTitle(snap, liveChapter(snap, number).PlanNodeID), liveChapter(snap, number).Runes(), planSummary(snap, number))
	}
	project, err := u.api.Projects.Project(u.ctx, u.project, model.InitialRevision)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range project.Ownership {
		u.log("  锁定 %s %s %q", rule.Target.Key(), rule.Control, rule.Guidance)
	}
	// 人设落进了书里并锁住：陈渡名下有写着左手旧伤的设定，它受锁定保护。
	persona := slices.ContainsFunc(project.Canon, func(fact model.CanonFact) bool {
		owner := slices.IndexFunc(project.Entities, func(entity model.Entity) bool { return entity.ID == fact.SubjectID })
		return owner >= 0 && project.Entities[owner].Name == "陈渡" && strings.Contains(string(fact.Value), "左手") &&
			slices.ContainsFunc(project.Ownership, func(rule model.OwnershipRule) bool {
				return rule.Target == model.DocumentRef{Kind: model.DocumentCanon, ID: fact.ID} && rule.Control != model.ControlOpen
			})
	})
	if !persona || !liveOwned(u, model.DocumentPlan) {
		t.Errorf("S5：主角人设（%v）或第 4 章没有锁定", persona)
	}
	if locked != "" && planSummary(snap, 4) != locked {
		t.Errorf("S5：锁定后第 4 章的规划被改了：%s → %s", locked, planSummary(snap, 4))
	}
	if summary := planSummary(snap, 1); !strings.Contains(summary, "烧") {
		t.Errorf("S2：第 1 章的规划没有按用户的大纲：%s", summary)
	}
	for number, brief := range snap.Briefs {
		for _, requirement := range brief.Requirements {
			if requirement.Status == model.CheckViolated || strings.Contains(outline, requirement.Text) && requirement.Status != model.CheckSatisfied {
				t.Errorf("S2：第 %d 章的要求「%s」：%s", number, requirement.Text, requirement.Status)
			}
		}
	}
}

// liveOwned 报告作品里有没有 AI 不能自由改的某类文档。
func liveOwned(u *liveUser, kind model.DocumentKind) bool {
	project, err := u.api.Projects.Project(u.ctx, u.project, model.InitialRevision)
	if err != nil {
		u.t.Fatal(err)
	}
	return slices.ContainsFunc(project.Ownership, func(rule model.OwnershipRule) bool {
		return rule.Target.Kind == kind && rule.Control != model.ControlOpen
	})
}

// S9、S3：自动推进写第 2 章时把审批调严（之后每章等用户过目）；第一份等确认的稿件到来时，改第
// 4 章的结局（未来计划），已经通过的章不动，第 4 章按新的写。
func TestLiveMidCourseControl(t *testing.T) {
	u := openLive(t)
	u.log("S9：自动推进，写第 2 章时调严审批")
	u.session.Write(chief.WriteParams{Premise: livePremise, Chapters: 4, Approval: model.ApprovalAuto})
	u.cue = &liveCue{phase: "落笔第 2 章", words: "从现在起每章写完先给我看，我确认了再往下写。"}
	snap := u.advance(func(snap workbench.WorkbenchSnapshot) bool { return draftOf(snap) > 0 })
	if u.cue != nil {
		t.Fatal("S9：没赶上第 2 章的写作途中")
	}
	if snap.Approval != model.ApprovalManual {
		t.Errorf("S9：审批没有调成逐章确认：%s", snap.Approval)
	}
	held := draftOf(snap)
	u.log("第一份等确认的稿件：第 %d 章", held)

	u.log("S3：改第 4 章的结局")
	accepted := make(map[int]string)
	for _, chapter := range snap.Manuscript {
		accepted[chapter.Number] = liveText(chapter)
	}
	before := planSummary(snap, 4)
	u.say("第 4 章的结局改一下：姐姐没有回来，只托人带回一封信。", 0)
	snap = u.advance(completed)
	after := planSummary(snap, 4)
	u.log("第 4 章梗概：%s → %s", before, after)
	if after == before || !strings.Contains(after, "信") {
		t.Errorf("S3：第 4 章的规划没有按新结局改")
	}
	for number, text := range accepted {
		if liveText(liveChapter(snap, number)) != text {
			t.Errorf("S3：已经通过的第 %d 章被改了", number)
		}
	}
	if !strings.Contains(liveText(liveChapter(snap, 4)), "信") {
		t.Errorf("S3：第 4 章正文没有按新结局写")
	}
}

// 长篇验收（§0.4 #3）：自动推进写 40 章，记每章摊到的用量与用时；单章成本不该随章数线性增长。
func TestLiveLongBook(t *testing.T) {
	const chapters = 40
	u := openLive(t)
	u.log("长篇：一句话开书，%d 章，自动推进", chapters)
	var start workbench.WorkbenchSnapshot
	if os.Getenv("AINOVEL_LIVE_PROJECT") != "" {
		start = u.snapshot() // 接着跑：已写的章不计入
	} else {
		u.session.Write(chief.WriteParams{Premise: livePremise, Chapters: chapters, Approval: model.ApprovalAuto})
	}
	perChapter := u.book(chapters, start)
	if len(perChapter) < 20 {
		return // 接着跑的书只看到后半程，首末段对照见库里的执行记录
	}
	// 每章摊到的输入按十章一段取平均：末段明显高于首段就是上下文随章数膨胀。
	decile := func(from int) int {
		sum := 0
		for _, usage := range perChapter[from : from+10] {
			sum += usage.Input
		}
		return sum / 10
	}
	first, final := decile(0), decile(len(perChapter)-10)
	u.log("每章摊到的输入：前十章 %dK，后十章 %dK", first/1000, final/1000)
	if final > 2*first {
		t.Errorf("单章输入随章数膨胀：前十章 %dK，后十章 %dK", first/1000, final/1000)
	}
}

// 换模型、思考强度对照：同一设定写 6 章，篇幅用官方写作标准的默认（软要求）；模型与档位用
// AINOVEL_MODEL、AINOVEL_THINKING 换，比每章用时、用量、字数与正文。
func TestLiveModelTrial(t *testing.T) {
	const chapters = 6
	u := openLive(t)
	u.log("对照：%d 章，默认篇幅，自动推进", chapters)
	if _, err := u.api.Projects.CreateProject(u.ctx, projectdoc.CreateProjectCommand{
		ProjectID: u.project, ChangeID: u.project + ":create", UserID: liveUserID, Reason: "建立作品",
		Draft: projectdoc.ProjectDraft{Intent: model.Intent{Premise: livePremise}, Approval: model.ApprovalAuto}, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	u.session.Write(chief.WriteParams{Chapters: chapters})
	u.book(chapters, workbench.WorkbenchSnapshot{})
}

// 对照 v0（§0.4 第 3、4 条）：同一设定与要求写 40 章、自动推进。v0 用日志里打出的同一段需求
// 跑 headless，比每章输入、完本、返工与要求满足。篇幅两边都是软要求：v1 在官方写作标准里，
// v0 写进需求原文。
func TestLiveVersusV0(t *testing.T) {
	const chapters = 40
	intent := model.Intent{Premise: livePremise, Required: []string{"第 5 章陈渡第一次正面见到邮局局长"}, Forbidden: []string{"感情线"}}
	u := openLive(t)
	u.log("v0 的需求：%s全书 %d 章，每章三到四千字。要求：%s。不要写%s。", intent.Premise, chapters, intent.Required[0], intent.Forbidden[0])
	u.session.Write(chief.WriteParams{Intent: &intent, Chapters: chapters, Approval: model.ApprovalAuto})
	u.book(chapters, workbench.WorkbenchSnapshot{})
	snap := u.snapshot()
	for number := 1; number <= chapters; number++ {
		for _, requirement := range snap.Briefs[number].Requirements {
			if requirement.Status != model.CheckSatisfied {
				u.log("第 %d 章的要求「%s」：%s", number, requirement.Text, requirement.Status)
			}
		}
	}
}

// book 自动推进到完本：每入稿一章记它摊到的用量与用时，完本后记总量与各角色占比。start 是
// 接着跑时开始的现场，此前写的章不计入。
func (u *liveUser) book(chapters int, start workbench.WorkbenchSnapshot) []model.Usage {
	written, last := len(start.Manuscript), start.Usage
	perChapter := make([]model.Usage, 0, chapters)
	u.watch = func(snap workbench.WorkbenchSnapshot) {
		if len(snap.Manuscript) <= written {
			return
		}
		delta := snap.Usage.Total
		delta.Input -= last.Total.Input
		delta.Output -= last.Total.Output
		delta.CacheRead -= last.Total.CacheRead
		delta.Cost -= last.Total.Cost
		count := len(snap.Manuscript) - written
		for range count {
			perChapter = append(perChapter, model.Usage{
				Input: delta.Input / count, Output: delta.Output / count,
				CacheRead: delta.CacheRead / count, Cost: delta.Cost / float64(count),
			})
		}
		u.log("入稿到第 %d 章 · 输入 %dK（缓存 %dK）输出 %dK · $%.3f · 用时 %s",
			len(snap.Manuscript), delta.Input/1000, delta.CacheRead/1000, delta.Output/1000, delta.Cost,
			(snap.Usage.Worked - last.Worked).Round(time.Second))
		written, last = len(snap.Manuscript), snap.Usage
	}
	snap := u.advance(completed)
	u.watch(snap) // 最后几章可能在两次查看之间入稿

	runes := 0
	for _, chapter := range snap.Manuscript {
		runes += chapter.Runes()
	}
	total := snap.Usage.Total
	u.log("完本 %d 章 %d 字 · 输入 %dK（缓存 %dK）输出 %dK · $%.2f · 用时 %s",
		len(snap.Manuscript), runes, total.Input/1000, total.CacheRead/1000, total.Output/1000, total.Cost,
		snap.Usage.Worked.Round(time.Second))
	for role, usage := range snap.Usage.Roles {
		u.log("  %s：输入 %dK（%.0f%%）输出 %dK · $%.2f · 用时 %s", role, usage.Usage.Input/1000,
			100*float64(usage.Usage.Input)/float64(total.Input), usage.Usage.Output/1000, usage.Usage.Cost, usage.Worked.Round(time.Second))
	}
	if len(snap.Manuscript) != chapters {
		u.t.Fatalf("写了 %d 章，要的是 %d 章", len(snap.Manuscript), chapters)
	}
	return perChapter
}

// S6、S7、S8：装一个创作包（写手用第一人称；审阅标准改为每章以问句收尾），配错的包装不上；
// 书级规则要求每章出现钟楼。写完两章看生效的 Prompt 带着包与规则的来源和版本、正文照着写；
// 用户把第 1 章删到每段只剩第一句，学出偏好候选，确认后进入用户的全局偏好。
func TestLiveCreatorAssets(t *testing.T) {
	u := openLive(t)
	now := func() time.Time { return time.Now().UTC() }
	pack := func(slot, text string) string {
		dir := t.TempDir()
		if err := os.MkdirAll(dir+"/prompts", 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := fmt.Sprintf(`{"id":"live-style","version":"1","name":"验收文风","prompts":{%q:"prompts/a.md","editor.story_review":"prompts/b.md"}}`, slot)
		for name, content := range map[string]string{
			"pack.jsonc": manifest, "prompts/a.md": text,
			"prompts/b.md": "审阅标准：每章最后一句必须是问句（以问号结尾），不是就记一条 blocking。",
		} {
			if err := os.WriteFile(dir+"/"+name, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	const voice = "用第一人称写：叙述者就是主角陈渡，以“我”自称。"
	if _, err := u.api.Resources.InstallPackDirectory(u.ctx, pack("writer.chapter_drafts", voice), u.project+":bad-pack", liveUserID, "装错的包", now()); err == nil {
		t.Fatal("S8：写错槽位的包装上了")
	} else {
		u.log("S8：写错槽位的包装不上：%v", err)
	}
	installed, err := u.api.Resources.InstallPackDirectory(u.ctx, pack("writer.chapter_draft", voice), u.project+":pack", liveUserID, "装验收文风", now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.api.Projects.CreateProject(u.ctx, projectdoc.CreateProjectCommand{
		ProjectID: u.project, ChangeID: u.project + ":create", UserID: liveUserID, Reason: "建立作品",
		Draft: projectdoc.ProjectDraft{Intent: model.Intent{Premise: livePremise}, Approval: model.ApprovalAuto}, CreatedAt: now(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.api.Resources.SetProjectAssets(u.ctx, resources.SetProjectAssetsCommand{
		ProjectID: u.project, ChangeID: u.project + ":assets", UserID: liveUserID, Reason: "启用验收文风",
		Packs: []resources.PackRef{{ID: resources.OfficialPackID}, {ID: installed.Manifest.ID}}, CreatedAt: now(),
	}); err != nil {
		t.Fatal(err)
	}
	const rule = "每章都要出现一次雨城的钟楼。"
	if _, err := u.api.Resources.SetProjectOverlay(u.ctx, resources.SetProjectOverlayCommand{
		ProjectID: u.project, ChangeID: u.project + ":overlay", UserID: liveUserID, Rules: []string{rule}, Reason: "书级规则", CreatedAt: now(),
	}); err != nil {
		t.Fatal(err)
	}

	u.log("S6、S8：两章，自动推进")
	u.session.Write(chief.WriteParams{Chapters: 2, Approval: model.ApprovalAuto})
	snap := u.advance(completed)
	operation, err := u.api.Tasks.Operation(u.ctx, snap.Run.ID+":chapter:"+liveChapter(snap, 1).PlanNodeID)
	if err != nil {
		t.Fatal(err)
	}
	text, sources, err := u.api.Prompts.Prompt(u.ctx, operation.Snapshot.ConfigDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		u.log("  来源 %s %s r%d %v", source.Layer, source.ID, source.Revision, source.Slots)
	}
	if !strings.Contains(text, voice) || !strings.Contains(text, rule) ||
		!slices.ContainsFunc(sources, func(source prompt.Source) bool {
			return source.ID == "live-style@1" && source.Revision == installed.Revision
		}) {
		t.Errorf("S6：写第 1 章生效的 Prompt 没有带上包与书级规则")
	}
	for _, chapter := range snap.Manuscript {
		body := liveText(chapter)
		last := strings.TrimRight(body, "”」』\"' \n")
		u.log("第 %d 章 %d 字 · “我” %d 次 · 钟楼 %v · 结尾「%s」", chapter.Number, chapter.Runes(),
			strings.Count(body, "我"), strings.Contains(body, "钟楼"), string([]rune(last)[max(0, len([]rune(last))-20):]))
		if strings.Count(body, "我") < 10 {
			t.Errorf("S8：第 %d 章不是第一人称", chapter.Number)
		}
		if !strings.Contains(body, "钟楼") {
			t.Errorf("S6：第 %d 章没有钟楼", chapter.Number)
		}
		if !strings.HasSuffix(last, "？") && !strings.HasSuffix(last, "?") {
			t.Errorf("S6：第 %d 章没有以问句收尾", chapter.Number)
		}
	}

	u.log("S7：用户把第 1 章删到每段只剩第一句")
	from := snap.Revision
	projection, err := u.api.Projects.ExportProject(u.ctx, u.project, from)
	if err != nil {
		t.Fatal(err)
	}
	for i := range projection.Manuscript {
		if projection.Manuscript[i].Number != 1 {
			continue
		}
		for j, block := range projection.Manuscript[i].Blocks {
			if end := strings.IndexAny(block.Text, "。！？"); end >= 0 {
				projection.Manuscript[i].Blocks[j].Text = block.Text[:end+len("。")]
			}
		}
	}
	proposal, err := u.api.Projects.ImportProject(u.ctx, u.project+":edit", liveUserID, "删繁就简", projection, now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.api.Decisions.Approve(u.ctx, proposal.ID, liveUserID, now()); err != nil {
		t.Fatal(err)
	}
	to := u.snapshot().Revision
	record, err := u.api.Resources.LearnPreference(u.ctx, resources.LearnPreferenceCommand{
		CandidateID: u.project + ":terse", ProfileID: liveUserID, Scope: "global", ProjectID: u.project,
		FromRevision: from, ToRevision: to, CreatedAt: now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	u.log("偏好候选：%s · 依据 %q · 规则 %q", record.Candidate.Summary, record.Candidate.Evidence, record.Candidate.ProposedRules)
	if record.State != "pending" || len(record.Candidate.ProposedRules) == 0 {
		t.Fatalf("S7：候选 = %+v", record)
	}
	if _, err := u.api.Resources.ConfirmPreferenceCandidate(u.ctx, u.project+":confirm", liveUserID, "确认偏好",
		liveUserID, "global", record.Candidate.ID, now()); err != nil {
		t.Fatal(err)
	}
	profile, _, err := u.api.Resources.CreatorProfile(u.ctx, liveUserID, "global", 0)
	if err != nil || !slices.Contains(profile.ExplicitRules, record.Candidate.ProposedRules[0]) {
		t.Errorf("S7：确认后的全局偏好 = %+v, %v", profile, err)
	}
}
