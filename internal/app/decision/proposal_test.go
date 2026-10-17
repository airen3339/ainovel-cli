package decision

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/app/resource"
	"github.com/voocel/ainovel-cli/internal/domain/change"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/infra/store"
)

// stubAnalyzer 返回预设的语义影响报告；用例在建提案前改 report。
type stubAnalyzer struct{ report model.SemanticImpactReport }

func (a *stubAnalyzer) Analyze(context.Context, model.Proposal, model.StructuralImpact) (json.RawMessage, error) {
	return json.Marshal(a.report)
}

func conflictReport() model.SemanticImpactReport {
	return model.SemanticImpactReport{
		Status: model.SemanticImpactConflict,
		Findings: []model.SemanticImpactFinding{{
			Document: &model.DocumentRef{Kind: model.DocumentManuscript, ID: "chapter-1"}, Explanation: "第一章已经用行动落实了旧底线",
		}},
		Options: []model.ResolutionOption{
			{Strategy: model.ResolutionRewriteAffected, ChapterIDs: []string{"chapter-1"}, Explanation: "同步重写第一章"},
			{Strategy: model.ResolutionReinterpretFuture, Explanation: "保留旧章并在后文解释变化"},
			{Strategy: model.ResolutionAbandon, Explanation: "放弃本次事实变更"},
		},
	}
}

type reviewFixture struct {
	ctx      context.Context
	store    *store.Store
	projects *projectdoc.Repository
	review   *Review
	analyzer *stubAnalyzer
	now      time.Time
}

const (
	fixtureProject = "book"
	fixtureUser    = "user-1"
)

// newReviewFixture 建库并种一部作品：revision 1 是设定，revision 2 加入用户写的第一章。
func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "decision.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	analyzer := &stubAnalyzer{report: conflictReport()}
	changes := change.NewWithSemanticAnalyzer(s, analyzer)
	official := resource.NewOfficial(s, changes, fstest.MapFS{"pack.jsonc": {Data: []byte(`{"id":"official","version":"1","name":"官方基线"}`)}})
	projects := projectdoc.New(s, changes, official)
	f := &reviewFixture{
		ctx: ctx, store: s, projects: projects, analyzer: analyzer,
		review: New(s, changes, projects),
		now:    time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC),
	}
	if _, err := projects.CreateProject(ctx, projectdoc.CreateProjectCommand{
		ProjectID: fixtureProject, ChangeID: "create", UserID: fixtureUser, Reason: "创建作品", CreatedAt: f.now,
		Draft: projectdoc.ProjectDraft{
			Intent: model.Intent{Premise: "一个凡人进入修仙宗门"},
			Plan: []model.PlanNode{
				{ID: "volume-1", Kind: model.PlanVolume, Title: "入道", Summary: "进入修行世界"},
				{ID: "arc-1", Kind: model.PlanArc, ParentID: "volume-1", Title: "山门", Summary: "通过山门考验"},
				{ID: "chapter-plan-1", Kind: model.PlanChapter, Turn: "转折", ParentID: "arc-1", Order: 1, Title: "第一章", Summary: "抵达山门"},
			},
			Entities: []model.Entity{{ID: "hero", Kind: model.EntityCharacter, Name: "主角"}},
			Canon: []model.CanonFact{{
				ID: "hero-bottom-line", Kind: model.CanonWorldRule, SubjectID: "hero", Predicate: "rule.bottom_line", Value: json.RawMessage(`"绝不滥杀"`),
			}},
		},
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	f.commitEdit(t, "add-chapter", func(projection *projectdoc.ProjectProjection) {
		projection.Manuscript = append(projection.Manuscript, model.ManuscriptChapter{
			ID: "chapter-1", PlanNodeID: "chapter-plan-1", Number: 1, Author: model.AuthorUser,
			Blocks:    []model.ManuscriptBlock{{ID: "block-1", Text: "他在山门前放下了刀。"}},
			DependsOn: []model.DocumentRef{{Kind: model.DocumentEntity, ID: "hero"}},
		})
	})
	return f
}

func (f *reviewFixture) export(t *testing.T) projectdoc.ProjectProjection {
	t.Helper()
	projection, err := f.projects.ExportProject(f.ctx, fixtureProject, 0)
	if err != nil {
		t.Fatalf("export project: %v", err)
	}
	return projection
}

// commitEdit 以用户身份直接提交一次改动，推进作品 Revision。
func (f *reviewFixture) commitEdit(t *testing.T, changeID string, edit func(*projectdoc.ProjectProjection)) {
	t.Helper()
	projection := f.export(t)
	edit(&projection)
	proposal, err := f.projects.ImportProject(f.ctx, changeID, fixtureUser, changeID, projection, f.now)
	if err != nil {
		t.Fatalf("import %s: %v", changeID, err)
	}
	if _, err := f.review.Approve(f.ctx, proposal.ID, fixtureUser, f.now); err != nil {
		t.Fatalf("approve %s: %v", changeID, err)
	}
}

// conflict 建一个改动角色底线、带语义冲突报告的待裁决提案。
func (f *reviewFixture) conflict(t *testing.T, id string) model.Proposal {
	t.Helper()
	projection := f.export(t)
	projection.Canon[0].Value = json.RawMessage(`"可以为目的伤害无辜"`)
	proposal, err := f.projects.ImportProjectWithSemantic(f.ctx, id, fixtureUser, "改变角色底线", projection, f.now)
	if err != nil {
		t.Fatalf("prepare semantic proposal: %v", err)
	}
	if proposal.BaseRevision != 2 || proposal.Impact.Semantic == nil {
		t.Fatalf("semantic proposal = %#v", proposal)
	}
	return proposal
}

func (f *reviewFixture) command(proposalID string, strategy model.ResolutionStrategy) ResolveProposalCommand {
	return ResolveProposalCommand{
		ProposalID: proposalID, UserID: fixtureUser, Strategy: string(strategy), Reason: " 放弃改动 ",
		CreatedAt: f.now.Add(5 * time.Minute),
	}
}

func (f *reviewFixture) assertState(t *testing.T, proposalID string, state model.ApprovalState, revision model.Revision) {
	t.Helper()
	stored, err := f.store.GetProposal(f.ctx, proposalID)
	if err != nil || stored.ApprovalState != state {
		t.Fatalf("proposal state = %q (%v), want %q", stored.ApprovalState, err, state)
	}
	current, err := f.store.CurrentRevision(f.ctx, model.AuthorityTarget{Kind: model.AuthorityProject, ID: fixtureProject})
	if err != nil || current != revision {
		t.Fatalf("current revision = %d (%v), want %d", current, err, revision)
	}
}

func TestResolveProposalRejectsIncompleteCommand(t *testing.T) {
	f := newReviewFixture(t)
	proposal := f.conflict(t, "change-bottom-line")
	cases := map[string]struct {
		edit func(*ResolveProposalCommand)
		want error
	}{
		"missing proposal": {func(c *ResolveProposalCommand) { c.ProposalID = "" }, model.ErrInvalid},
		"missing user":     {func(c *ResolveProposalCommand) { c.UserID = "" }, model.ErrInvalid},
		"missing time":     {func(c *ResolveProposalCommand) { c.CreatedAt = time.Time{} }, model.ErrInvalid},
		"unknown proposal": {func(c *ResolveProposalCommand) { c.ProposalID = "ghost" }, model.ErrNotFound},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			command := f.command(proposal.ID, model.ResolutionAbandon)
			tc.edit(&command)
			if _, err := f.review.ResolveProposal(f.ctx, command); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			f.assertState(t, proposal.ID, model.ApprovalPending, 2)
		})
	}
}

func TestResolveProposalRequiresSemanticConflict(t *testing.T) {
	f := newReviewFixture(t)
	plain, err := f.projects.ImportProject(f.ctx, "plain-edit", fixtureUser, "无语义报告", func() projectdoc.ProjectProjection {
		projection := f.export(t)
		projection.Plan[2].Summary = "冒雨抵达山门"
		return projection
	}(), f.now)
	if err != nil {
		t.Fatalf("prepare plain proposal: %v", err)
	}
	f.analyzer.report = model.SemanticImpactReport{Status: model.SemanticImpactConsistent}
	consistent := f.conflict(t, "consistent-edit")
	f.analyzer.report = conflictReport()
	conflict := f.conflict(t, "change-bottom-line")

	cases := map[string]struct {
		proposal string
		strategy model.ResolutionStrategy
		want     string
	}{
		"unknown strategy":          {conflict.ID, "merge", `unknown resolution strategy "merge"`},
		"proposal without report":   {plain.ID, model.ResolutionAbandon, "has no semantic impact report"},
		"consistent proposal":       {consistent.ID, model.ResolutionReinterpretFuture, "approve it directly"},
		"consistent proposal abort": {consistent.ID, model.ResolutionAbandon, "approve it directly"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.review.ResolveProposal(f.ctx, f.command(tc.proposal, tc.strategy))
			if !errors.Is(err, model.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want ErrInvalid containing %q", err, tc.want)
			}
			f.assertState(t, tc.proposal, model.ApprovalPending, 2)
		})
	}
}

func TestResolveProposalAbandonRejectsWithReason(t *testing.T) {
	f := newReviewFixture(t)
	proposal := f.conflict(t, "change-bottom-line")
	command := f.command(proposal.ID, model.ResolutionAbandon)
	result, err := f.review.ResolveProposal(f.ctx, command)
	if err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if result.Strategy != "abandon" || result.ChangeSet != nil || len(result.Reworks) != 0 || result.Proposal == nil {
		t.Fatalf("result = %#v", result)
	}
	rejected := *result.Proposal
	if rejected.ApprovalState != model.ApprovalRejected || rejected.DecisionReason != "放弃改动" ||
		rejected.DecidedBy == nil || rejected.DecidedBy.ID != fixtureUser || rejected.DecidedAt == nil || !rejected.DecidedAt.Equal(command.CreatedAt) {
		t.Fatalf("rejected proposal = %#v", rejected)
	}
	f.assertState(t, proposal.ID, model.ApprovalRejected, 2)
	if snapshot, err := f.projects.Project(f.ctx, fixtureProject, 0); err != nil || string(snapshot.Canon[0].Value) != `"绝不滥杀"` {
		t.Fatalf("canon after abandon = %s (%v)", snapshot.Canon[0].Value, err)
	}

	// 重复放弃幂等；放弃后再批准是状态冲突。
	again, err := f.review.ResolveProposal(f.ctx, command)
	if err != nil || again.Proposal == nil || again.Proposal.ApprovalState != model.ApprovalRejected || again.Proposal.DecisionReason != "放弃改动" {
		t.Fatalf("repeated abandon = %#v, %v", again, err)
	}
	if _, err := f.review.ResolveProposal(f.ctx, f.command(proposal.ID, model.ResolutionReinterpretFuture)); !errors.Is(err, model.ErrStateConflict) {
		t.Fatalf("approve after reject: %v", err)
	}
}

func TestResolveProposalReinterpretFutureCommitsWithoutRework(t *testing.T) {
	f := newReviewFixture(t)
	proposal := f.conflict(t, "change-bottom-line")
	result, err := f.review.ResolveProposal(f.ctx, f.command(proposal.ID, model.ResolutionReinterpretFuture))
	if err != nil {
		t.Fatalf("reinterpret future: %v", err)
	}
	if result.Strategy != "reinterpret_future" || result.Proposal != nil || len(result.Reworks) != 0 || result.ChangeSet == nil {
		t.Fatalf("result = %#v", result)
	}
	if result.ChangeSet.ID != proposal.ID || result.ChangeSet.NewRevision != 3 || result.ChangeSet.ApprovalState != model.ApprovalApproved {
		t.Fatalf("change set = %#v", result.ChangeSet)
	}
	f.assertState(t, proposal.ID, model.ApprovalApproved, 3)
	snapshot, err := f.projects.Project(f.ctx, fixtureProject, 0)
	if err != nil || string(snapshot.Canon[0].Value) != `"可以为目的伤害无辜"` || string(snapshot.Canon[0].PreviousValue) != `"绝不滥杀"` {
		t.Fatalf("canon after commit = %#v (%v)", snapshot.Canon, err)
	}

	// 已批准的提案重复裁决返回同一 ChangeSet；再放弃是状态冲突。
	again, err := f.review.ResolveProposal(f.ctx, f.command(proposal.ID, model.ResolutionReinterpretFuture))
	if err != nil || again.ChangeSet == nil || again.ChangeSet.NewRevision != 3 {
		t.Fatalf("repeated resolution = %#v, %v", again, err)
	}
	if _, err := f.review.ResolveProposal(f.ctx, f.command(proposal.ID, model.ResolutionAbandon)); !errors.Is(err, model.ErrStateConflict) {
		t.Fatalf("abandon after approve: %v", err)
	}
	f.assertState(t, proposal.ID, model.ApprovalApproved, 3)
}

// 重写受影响的章：批准之后逐章记为返工要求（D86），入账晚于正文，由推导派返工；重试幂等。
func TestResolveProposalRewriteAffectedNotesReworks(t *testing.T) {
	f := newReviewFixture(t)
	proposal := f.conflict(t, "change-bottom-line")
	command := f.command(proposal.ID, model.ResolutionRewriteAffected)
	result, err := f.review.ResolveProposal(f.ctx, command)
	if err != nil {
		t.Fatalf("rewrite affected: %v", err)
	}
	if result.ChangeSet == nil || result.ChangeSet.NewRevision != 3 || len(result.Reworks) != 1 {
		t.Fatalf("result = %#v", result)
	}
	want := model.Directive{
		ID: "change-bottom-line:rework:1", Scope: model.DirectiveScopePlanNode("chapter-plan-1"),
		Text: "同步重写第一章", Status: model.DirectiveActive, Rework: true,
	}
	if got := result.Reworks[0]; got.ID != want.ID || got.Scope != want.Scope || got.Text != want.Text || got.Status != want.Status || !got.Rework {
		t.Fatalf("rework = %#v", got)
	}
	f.assertState(t, proposal.ID, model.ApprovalApproved, 4)
	snapshot, err := f.projects.Project(f.ctx, fixtureProject, 0)
	if err != nil || len(snapshot.Directives) != 1 || snapshot.Directives[0].ID != want.ID {
		t.Fatalf("directives = %#v (%v)", snapshot.Directives, err)
	}
	if again, err := f.review.ResolveProposal(f.ctx, command); err != nil || len(again.Reworks) != 1 {
		t.Fatalf("repeated resolution = %#v, %v", again, err)
	}
	f.assertState(t, proposal.ID, model.ApprovalApproved, 4)
}

func TestResolveProposalStaleProposalCannotBeApprovedButCanBeAbandoned(t *testing.T) {
	f := newReviewFixture(t)
	proposal := f.conflict(t, "change-bottom-line")
	// 报告判断过的正文其后改了：报告过时，不能照它批准。
	f.commitEdit(t, "later-edit", func(projection *projectdoc.ProjectProjection) {
		projection.Manuscript[0].Blocks[0].Text = "他冒雨抵达山门。"
	})
	_, err := f.review.ResolveProposal(f.ctx, f.command(proposal.ID, model.ResolutionReinterpretFuture))
	if !errors.Is(err, model.ErrRevisionConflict) {
		t.Fatalf("stale approval: %v", err)
	}
	f.assertState(t, proposal.ID, model.ApprovalPending, 3)
	result, err := f.review.ResolveProposal(f.ctx, f.command(proposal.ID, model.ResolutionAbandon))
	if err != nil || result.Proposal == nil || result.Proposal.ApprovalState != model.ApprovalRejected {
		t.Fatalf("stale abandon = %#v, %v", result, err)
	}
	f.assertState(t, proposal.ID, model.ApprovalRejected, 3)
}
