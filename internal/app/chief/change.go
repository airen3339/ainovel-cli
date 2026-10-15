package chief

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/voocel/ainovel-cli/internal/app/decision"
	"github.com/voocel/ainovel-cli/internal/app/novel"
	projectdoc "github.com/voocel/ainovel-cli/internal/app/project"
	"github.com/voocel/ainovel-cli/internal/domain/model"
	"github.com/voocel/ainovel-cli/internal/domain/narrative"
)

// 主编改书（D77）：修改与撤销都落成主编署名、带来源的提案，经 Change Engine 校验，按
// 审批放行或等用户确认；回执按提出时的作品逐处说清改了什么。

// historyLimit 是 book_history 列出的修改次数，historyLines 是每次列出的改动处数。
const (
	historyLimit = 10
	historyLines = 8
)

type changeArgs struct {
	Reason string `json:"reason"`
	narrative.Edit
}

func (s *Session) proposeChange(ctx context.Context, args changeArgs) (string, error) {
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		return "", errors.New("reason 必填：一句话说明为什么这么改")
	}
	project, story, err := s.project(ctx)
	if err != nil {
		return "", err
	}
	s.own(&args.Edit)
	patches, intent, err := story.ResolveEdit(args.Edit, narrative.Settings{Intent: project.Intent, Ownership: project.Ownership})
	if err != nil {
		return "", err
	}
	if err := s.authored(project, intent, args.Locks); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	proposal, err := s.proposal(projectdoc.NewID("change", now), reason, now)
	if err != nil {
		return "", err
	}
	proposal.BaseRevision, proposal.Patches = project.Revision, patches
	return s.submit(ctx, proposal)
}

// authored 核对以用户名义写入的文字都出自用户（D74 引用即署名）：作品方向里新写的、
// 锁定时新给的指导。沿用原有的不必再引。
func (s *Session) authored(project projectdoc.Snapshot, intent *model.Intent, locks []narrative.LockEdit) error {
	var texts []string
	if intent != nil {
		before := project.Intent
		for _, field := range [][2]string{
			{before.Premise, intent.Premise}, {before.Audience, intent.Audience}, {before.EndingDirection, intent.EndingDirection},
		} {
			if field[1] != field[0] {
				texts = append(texts, field[1])
			}
		}
		for _, list := range [][2][]string{
			{before.DesiredExperience, intent.DesiredExperience}, {before.Required, intent.Required}, {before.Forbidden, intent.Forbidden},
		} {
			for _, item := range list[1] {
				if !slices.Contains(list[0], item) {
					texts = append(texts, item)
				}
			}
		}
	}
	for _, lock := range locks {
		for _, item := range lock.Guidance {
			if !slices.ContainsFunc(project.Ownership, func(rule model.OwnershipRule) bool { return slices.Contains(rule.Guidance, item) }) {
				texts = append(texts, item)
			}
		}
	}
	for _, text := range texts {
		if _, _, err := s.quote(text); err != nil {
			return err
		}
	}
	return nil
}

// own 把修改里出自用户的文字换回用户的写法（见 quote）；不出自用户的原样留给 authored 核对。
func (s *Session) own(edit *narrative.Edit) {
	var texts []*string
	if intent := edit.Intent; intent != nil {
		texts = append(texts, &intent.Premise, &intent.Audience, &intent.EndingDirection)
		for _, list := range [][]string{intent.DesiredExperience, intent.Required, intent.Forbidden} {
			for i := range list {
				texts = append(texts, &list[i])
			}
		}
	}
	for _, lock := range edit.Locks {
		for i := range lock.Guidance {
			texts = append(texts, &lock.Guidance[i])
		}
	}
	for _, text := range texts {
		if original, _, err := s.quote(*text); err == nil && *text != "" {
			*text = original
		}
	}
}

// proposal 是主编署名的提案外壳：来源是促成它的那句用户的话。
func (s *Session) proposal(id, reason string, at time.Time) (model.Proposal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.said) == 0 {
		return model.Proposal{}, errors.New("用户还没有说话：修改要有用户的话作依据")
	}
	return model.Proposal{
		ID: id, Target: model.AuthorityTarget{Kind: model.AuthorityProject, ID: s.projectID},
		Author: model.ChiefAuthor, Reason: reason, Source: model.ConversationSource(s.said[len(s.said)-1].sequence),
		ApprovalState: model.ApprovalPending, CreatedAt: at,
	}, nil
}

// submit 提出修改并写回执。改动按提出时的作品描述：生效之后再描述就对不上了。还有一份
// 等确认的修改时，新的提出成功后撤回旧的；作品因此变了或不再有要等的，停下等用户的创作
// 接着推进。
func (s *Session) submit(ctx context.Context, proposal model.Proposal) (string, error) {
	previous, superseded, err := s.service.decisions.PendingChange(ctx, s.projectID)
	if err != nil {
		return "", err
	}
	view, err := s.service.bench.DescribeProposal(ctx, s.projectID, proposal)
	if err != nil {
		return "", err
	}
	submitted, err := s.service.decisions.Submit(ctx, proposal)
	if err != nil {
		return "", err
	}
	receipt := summarize(view.Changes, 3)
	if superseded {
		if err := s.service.decisions.Withdraw(ctx, previous.ID, proposal.CreatedAt); err != nil {
			return "", err
		}
		receipt = "替换上一份待确认的修改 · " + receipt
	}
	switch {
	case submitted.ChangeSet != nil:
		return s.settled(ctx, receipt+" · 已生效，可撤销", false)
	case submitted.Conflicted:
		return receipt + " · 与已写的章节有出入，等你在确认卡上输入序号选怎么处理", nil
	}
	return receipt + " · 等你确认", nil
}

// settled 收尾一次作品变化：等用户决定的稿件因此过时的（§5.5 第 4 条），回执写明它会按现在
// 的内容重写，不能再照它通过；然后让创作接着对账。
func (s *Session) settled(ctx context.Context, receipt string, newRound bool) (string, error) {
	snap, err := s.service.bench.WorkbenchSnapshot(ctx, s.projectID)
	if err != nil {
		return "", err
	}
	if snap.Decision != nil && snap.Decision.Stale {
		receipt += " · 待定的稿件过时了，按现在的内容重写"
	}
	return receipt, s.proceed(ctx, newRound)
}

// proceed 让作品变化之后的创作接着对账：它等的事可能已经没了，也可能多了要做的事。进行中的
// 创作就推进（已经在写时什么也不做），用户暂停的不动；newRound 时已完本或停止的书开新一轮
// （D29：完本之后的修改是新一轮创作），用于用户点名的返工。
func (s *Session) proceed(ctx context.Context, newRound bool) error {
	run, ok, err := s.service.runs.ActiveCreationRun(ctx, s.projectID)
	if err != nil {
		return err
	}
	if ok && run.State != model.RunPaused || !ok && newRound {
		s.Write(WriteParams{Chapters: novel.KeepChapters})
	}
	return nil
}

// Confirm 是用户按 y 确认主编的修改（D77）：生效之后创作接着对账。
func (s *Session) Confirm(ctx context.Context, proposalID string) error {
	if _, err := s.service.decisions.Approve(ctx, proposalID, s.userID, time.Now().UTC()); err != nil {
		return err
	}
	return s.proceed(ctx, false)
}

// Resolve 是用户在确认卡上选的处置（S4）。重写受影响的章记为返工要求（D86），与主编记下的
// 返工一样：已完本或停止的书开新一轮；之后创作接着对账。
func (s *Session) Resolve(ctx context.Context, proposalID string, strategy model.ResolutionStrategy) error {
	command := decision.ResolveProposalCommand{ProposalID: proposalID, UserID: s.userID, Strategy: string(strategy), CreatedAt: time.Now().UTC()}
	if _, err := s.service.decisions.ResolveProposal(ctx, command); err != nil {
		return err
	}
	return s.proceed(ctx, strategy == model.ResolutionRewriteAffected)
}

func (s *Session) history(ctx context.Context, _ none) (string, error) {
	records, err := s.service.bench.History(ctx, s.projectID, historyLimit)
	if err != nil {
		return "", err
	}
	type change struct {
		ID       string   `json:"id"`
		At       string   `json:"at"`
		By       string   `json:"by"`
		Reason   string   `json:"reason,omitempty"`
		Changes  []string `json:"changes"`
		Undoable bool     `json:"undoable"`
	}
	type pending struct {
		ID       string   `json:"id"`
		Reason   string   `json:"reason"`
		Changes  []string `json:"changes"`
		Findings []string `json:"findings,omitempty"`
		Options  []string `json:"options,omitempty"`
		Stale    bool     `json:"stale,omitempty"`
	}
	var result struct {
		Pending *pending `json:"pending,omitempty"`
		Changes []change `json:"changes"`
	}
	snap, err := s.service.bench.WorkbenchSnapshot(ctx, s.projectID)
	if err != nil {
		return "", err
	}
	if c := snap.Change; c != nil {
		result.Pending = &pending{ID: c.Proposal.ID, Reason: c.Proposal.Reason, Changes: c.View.Changes, Findings: c.Findings, Stale: c.Stale}
		for i, option := range c.Options {
			result.Pending.Options = append(result.Pending.Options, fmt.Sprintf("%d. %s", i+1, option.Explanation))
		}
	}
	result.Changes = []change{}
	for _, record := range records {
		lines := record.Changes
		if len(lines) > historyLines {
			lines = append(slices.Clone(lines[:historyLines]), fmt.Sprintf("…另 %d 处", len(lines)-historyLines))
		}
		result.Changes = append(result.Changes, change{
			ID: record.ID, At: record.At.Local().Format("01-02 15:04"), By: record.By, Reason: record.Reason, Changes: lines, Undoable: record.Undoable,
		})
	}
	return encode(result)
}

func (s *Session) revert(ctx context.Context, args struct {
	ID string `json:"id"`
}) (string, error) {
	pending, ok, err := s.service.decisions.PendingChange(ctx, s.projectID)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if ok && pending.ID == args.ID {
		if err := s.service.decisions.Withdraw(ctx, pending.ID, now); err != nil {
			return "", err
		}
		return "撤回待确认的修改 · 作品不变", s.proceed(ctx, false)
	}
	proposal, err := s.proposal(projectdoc.NewID("undo", now), "撤销一次修改", now)
	if err != nil {
		return "", err
	}
	proposal, err = s.service.decisions.Undo(ctx, args.ID, proposal)
	switch {
	case errors.Is(err, model.ErrNotFound):
		return "", fmt.Errorf("没有编号为 %q 的修改，先用 book_history 查", args.ID)
	case errors.Is(err, model.ErrRevisionConflict):
		return "", errors.New("这次修改之后又有改动碰过同样的内容，不能单独撤销；要改回去就按用户的意思重新 propose_change")
	case errors.Is(err, model.ErrInvalid):
		return "", errors.New("这次修改只留下了记录（要求、裁决），没有可撤销的内容；要求用 retire_requirement 退掉")
	case err != nil:
		return "", err
	}
	receipt, err := s.submit(ctx, proposal)
	if err != nil {
		return "", err
	}
	return "撤销：" + receipt, nil
}

// summarize 把逐处改动收成一行：最多 limit 处，其余计数。
func summarize(lines []string, limit int) string {
	if len(lines) <= limit {
		return strings.Join(lines, "；")
	}
	return strings.Join(lines[:limit], "；") + fmt.Sprintf(" 等 %d 处", len(lines))
}
